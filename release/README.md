# Release packaging

Scripts that build, sign and package the Caribou desktop app and the
command-line `caribou` binary for Linux, Windows and macOS, and the Android
app, all from a Linux host, and the iOS app from a Mac. Each script takes a
release tag and produces its artifacts with no further input:

```bash
release/package-linux.sh   v1.2.0   # .deb + .tar.gz (amd64)
release/package-windows.sh v1.2.0   # signed setup.exe + .zip (amd64)
release/package-darwin.sh  v1.2.0   # notarized .dmg (x86_64 and arm64)
release/package-cli.sh     v1.2.0   # command-line archives (see below)
release/package-android.sh v1.2.0   # signed .apk + .aab for Google Play (arm64)
release/release.sh         v1.2.0   # all five, one SHA256SUMS + GPG signature, draft GitHub release
release/publish-play.sh    v1.2.0   # upload the .aab to a Google Play testing track
release/package-ios.sh     v1.2.0   # on a Mac: signed App Store .ipa, optional upload
```

Every release build carries the `xmr` tag. The prebuilt
`libmonero_wallet2_api_c` libraries are read from `wallet/asset/xmr/lib/`
(or `--xmr-libs <dir>`) and verified against the `SHA256SUMS` committed at
the tag being built before they are linked.

## How a build runs

1. The tag is resolved to a commit and every version form is derived from it
   (semver for the binaries, `~` form for dpkg, numeric for Windows version
   resources). `--ref
   <commit>` builds something other than the tag: with a tag, the commit is
   recorded as build metadata; without one, the version is
   `<nearest tag>-dev.<sha>`.
2. The commit is exported with `git archive` into `release/out/<version>/src`.
   The working tree is never built directly, so an artifact always
   corresponds to a commit.
3. The script re-runs itself inside the `caribou-builder` Docker image with
   `release/` mounted read-only, `release/out/<version>` as the work
   directory, and `release/.env` mounted read-only as `/secrets`.
   Credentials set in `env.sh` are passed to the container by name, so their
   values do not appear in the `docker` command line.
4. Inside the image: web UI build, Go build, signing, packaging. Outputs land
   in `release/out/<version>/<os>/`.

Only the Android scripts narrow step 3: `package-android.sh` builds in one
container with no credentials and signs in a second one, and
`publish-play.sh` gets the Google Play key alone (see the Android notes
below). The desktop and command-line scripts build and sign in one
container, which holds every credential in `release/.env`.

Toolchain caches (Go, npm, Electron, dotnet, Gradle) persist in
`release/out/.cache` across runs. `release/out` and `release/.env` are gitignored.

## The builder image

`release/Dockerfile` builds `caribou-builder` on Ubuntu 22.04, which fixes
the Linux binary's glibc and WebKitGTK floor at Ubuntu 22.04 / Debian 12. It
holds Go, Node, MinGW, an aarch64 Linux cross compiler, NSIS, rcodesign,
jsign, osslsigncode, genisoimage and libdmg-hfsplus, an osxcross toolchain
when a macOS SDK is available, and the Android toolchain: a JDK, the SDK
platform, build tools and platform-tools, the NDK and bundletool.
Every download is pinned by checksum in the Dockerfile.

The image builds automatically on first use as `caribou-builder:<base>`.
The base is chosen by `package-linux.sh` and `package-cli.sh` from the Linux
XMR libraries: the oldest Ubuntu whose glibc and libstdc++ can link them
(22.04 when they need glibc 2.35 or older, 24.04 otherwise), since those
libraries set the Linux floor and the binaries should be built on the oldest
base that can satisfy it. The other platforms reuse whatever builder image
exists. When a new base is needed, the image is rebuilt on it and the
previous builder image (identified by the `caribou.builder` label, nothing
else) is removed once the new one exists. Rebuild after changing the
Dockerfile or staging the Xcode xip with `--build-image`.

### macOS toolchain

Cross-compiling the Go binary for macOS needs Apple's SDK, which comes from
an Xcode xip that only a signed-in Apple developer can download. Stage it in
`wallet/asset/xmr/xcode/` exactly as described in that directory's README
(the same pinned xip serves the XMR library build), then run any package
script with `--build-image`. Without a staged xip the image builds without a
macOS toolchain and `package-darwin.sh` stops with a message saying so. When
the xip is staged in a different checkout, `CARIBOU_XCODE_DIR=<path to that
xcode directory>` points the image build at it.

## Credentials

Copy `release/env.example` to `release/.env/env.sh` and fill it in; the
comments there say where each value comes from. In short:

- **Apple**: the Developer ID Application certificate and private key (PEM
  pair, or a `.p12` exported from a Mac keychain), the Team ID, and an App
  Store Connect API key for notarization. rcodesign cannot use an
  app-specific password.
- **Windows**: an Azure Artifact Signing account (the service formerly
  called Trusted Signing), a completed identity validation, a Public Trust
  certificate profile, and a service principal holding the "Artifact Signing
  Certificate Profile Signer" role. A local PKCS#12 keystore with a
  self-signed certificate exercises the same code path for testing.
- **Android**: two keystores made with `keytool`. The app signing key signs
  the APK and is the app's permanent identity on a device; a copy is enrolled
  in Play App Signing, so Google Play signs what it serves with the same key
  and a user can move between the two channels without reinstalling. The
  upload key signs the bundle sent to Play and can be reset by Google. Both
  certificates are pinned in `release/android/`, and `package-android.sh`
  fails a build signed by anything else. The app signing key cannot be reset
  or recovered, so it needs an offline backup.
- **Google Play**: a service account key for the Google Play Developer API,
  invited in the Play Console with permission to release to testing tracks.
- **iOS**: nothing in `release/.env` beyond the Team ID. Signing happens on
  the Mac through Xcode with a distribution certificate Apple holds; see
  the iOS notes below.
- **Manifest**: the GPG key that signs `SHA256SUMS`.

`--no-sign` builds without any signing; `--no-notarize` signs but skips the
notary service. `--no-xmr` exists for pipeline testing without the Monero
libraries and must never be used for a release.

## Per-platform notes

**Linux.** The `.deb` installs `caribou-desktop` to `/usr/bin`, the XMR
library to `/usr/lib/caribou` (where the binary's rpath looks), and the
desktop entry, AppStream metadata and icons. Its `Depends` line is derived
from the binaries with `dpkg-shlibdeps`, so the glibc, libstdc++, GTK and
WebKitGTK floors are whatever the shipped code references; the snap template
gets the matching core base. `--test-install` installs the package in a
clean container of the same Ubuntu base and runs `caribou-desktop --version`
as a smoke test. The tarball holds the binary and library side by side for
other distributions.

**Windows.** The `.exe` is cross-compiled with MinGW and stamped with icon
and version resources by go-winres. NSIS builds the installer,
`<name>-setup.exe`, from `release/windows/caribou.nsi`: a per-machine install
into Program Files with a Start Menu shortcut, an Add/Remove Programs entry
and an uninstaller. Its last page offers to open Caribou, as the signed-in
user rather than with the installer's administrator token, and to create a
desktop shortcut. Nothing on Linux runs the installer, so install it on a
Windows machine before publishing. It refuses to install where the Edge
WebView2 Runtime is absent (it ships with Windows 11 and Microsoft Edge) and
points at the download. The `.exe` and the DLLs, the NSIS plug-in DLLs the
installer loads while it runs, then the generated uninstaller, then the
installer are each Authenticode-signed with an RFC 3161 timestamp, and each
signature is verified with osslsigncode against the pinned Microsoft
Identity Verification root in `release/windows/`. A last check unpacks the
finished installer and fails the build if any PE file in it is unsigned
(`WebView2Loader.dll`, signed by Microsoft, must instead match the pinned
copy).

**macOS.** Electron wraps the CLI `caribou` binary; `release/darwin/electron`
holds the shell. The Go binary and XMR dylib are signed first, then the
packaged `Caribou.app`, which is notarized and stapled; the `.dmg` built
around it is signed, notarized and stapled in turn. Notarization normally
returns in minutes, but a team's first submissions have taken over an hour
in Apple's queue; the script waits up to six hours (`NOTARY_WAIT_SECONDS`
overrides) and `rcodesign notary-list` with the API key shows the queue.
The dmg is a genisoimage ISO 9660 image (Rock Ridge plus Apple extensions)
compressed by `dmg`, the construction Bitcoin Core used for its Linux-built
images. `release/darwin/dmg/` holds its Finder presentation: `background.png`
(the drag-to-Applications arrow, 2x with a 144 dpi tag) and `dsstore.py`,
which writes the `.DS_Store` with the window geometry, icon positions and
the alias record Finder follows to the background. The volume name is fixed
to `Caribou` because that alias embeds it. A volume icon is not possible
with this image format: genisoimage's Apple extensions carry Finder flags
for files only, not for the root folder. The app itself offers to move to
`/Applications` when launched from anywhere else, such as the mounted dmg.
`--zip` also emits a `.zip` of the
notarized app. Verify on a Mac with `spctl --assess --type open
--context context:primary-signature -v Caribou.dmg` and by launching the
installed app; an Apple Silicon Mac can also run the x86_64 build under
Rosetta.

**Command line.** `package-cli.sh` builds `wallet/cmd/caribou` for
linux/amd64, linux/arm64, darwin/amd64, darwin/arm64 and windows/amd64, each
with cgo and the XMR library, and archives the binary and library together in
a directory named like the archive, `caribou-<version>-<os>-<arch>`. Each
binary loads the library from its own directory: through the `$ORIGIN` rpath
on Linux, the dylib's `@executable_path` install name on macOS and the DLL
search order on Windows.

- Linux: `.tar.gz`. The arm64 build uses the image's `aarch64-linux-gnu`
  cross compiler. Both inherit the builder base's glibc floor.
- macOS: `.zip`. The binary and dylib get the Developer ID signature with
  the hardened runtime, and the zip is notarized. A ticket cannot be stapled
  to a zip, so Gatekeeper fetches it online when the binary first runs.
- Windows: `.zip`, built with the `systray` tag and `-H windowsgui` so it
  runs as a tray app without a console window. The `.exe` and the DLL are
  Authenticode-signed and verified like the desktop app's, and the
  resources in `wallet/cmd/caribou/winres.json` are regenerated with the
  release's version.

FreeBSD, OpenBSD and other Linux architectures have no prebuilt XMR library
and are not built.

**Android.** `package-android.sh` builds the mobile web bundle, binds the Go
core with gomobile (`wallet/android/bind.sh`, at the gomobile version pinned
in `go.mod`), and runs the Gradle build in `wallet/android` for the mainnet
flavor. Gradle is the version the committed wrapper names, checked against
`distributionSha256Sum`, and every plugin and library it resolves is checked
against `wallet/android/gradle/verification-metadata.xml`. Two artifacts come
out, both arm64 and both carrying the XMR library and the `libc++_shared.so`
it was built with:

- `caribou-<version>-android-arm64.apk`, signed with the app signing key
  (APK signature schemes 1 to 3). It goes into `dist/` and the GitHub release.
- `caribou-<version>.aab`, the Android App Bundle for Google Play, signed
  with the upload key. It stays in `out/<version>/android/`.

The work runs in two containers, so that nothing fetched from the network
runs next to a signing key. The first builds: npm, gomobile, Gradle and
everything they resolve run there, with no credential variable, with
`release/.env` hidden, and with write access under `out/<version>` only to
the exported source and to its own output directory. Gradle emits both
artifacts unsigned. The second
signs and checks them. It receives the two keystores and their passwords and
nothing else, has no network, no access to the shared caches and a home
directory of its own, and runs only tools that are part of the builder
image.

After signing, the second container checks, for the APK and the bundle
alike: the signer against its pin; the package name, version name and
version code; that the native libraries are the expected ones for arm64
only, each linked with 16 KB segment alignment; and that the XMR library and
its `libc++_shared.so` have the checksums the host recorded before the build
started. The APK must also be zip-aligned to 16 KB, and the bundle must pass
`bundletool validate` and ask, in its configuration, for uncompressed native
libraries aligned to 16 KB, since Google Play generates the APKs it serves
from that setting. Android 15 devices with 16 KB pages, and Google Play,
refuse anything less.

`out/<version>/android/` is created by the second container, in one step
and only when every check has passed, holding the two artifacts and a
`SHA256SUMS` of them. The first container cannot write there, and a run
that fails leaves no such directory. `release.sh` copies the APK, and
`publish-play.sh` uploads the bundle, by the exact name that script gives
it and only when `SHA256SUMS` lists it and still matches.
`release.sh --skip-android` leaves an APK from an earlier run out of
`dist/`.

`versionName` is the tag's version. `versionCode` is the number of commits in
the history of the commit being built, the build number `package-ios.sh` gives
the iOS app from the same commit. Google Play takes a version code once and
only above every one it has seen, so rebuilding an uploaded commit, or
building from a branch with fewer commits than the last upload, needs
`--build <N>` (`--android-build <N>` through `release.sh`).

`publish-play.sh` uploads the bundle through the Google Play Developer API
and creates a draft release on the internal testing track; `--rollout` makes
it available to the track's testers at once, `--track alpha` or `beta`
selects closed or open testing, and `--check` has Google validate the upload
and then discards it. Promotion to production is done in the Play Console.
If the final commit gets no answer, because of a timeout or a server error,
the script says so and does not guess: the release may or may not have gone
through, and the track in the Play Console is the place to look before
running it again.
The API can neither create the app nor accept its first build: the app
record, the first bundle and the Play App Signing enrollment are done by hand
in the Console. On that first upload, choose to use the existing app signing
key instead of a Google-generated one, before the bundle goes up.

Nothing on Linux runs the app, so install the APK on an arm64 device before
publishing (`adb install`), and confirm that it installs over the previous
release.

**iOS.** The app is built on a Mac by `package-ios.sh`, which does not use
the builder image: compiling the Swift shell and its asset catalog, signing
for the App Store and uploading need Xcode. It exports the tag like the other
scripts, checks Xcode and XcodeGen against the versions pinned at its top and
Go and Node against the ones pinned in `release/Dockerfile`, verifies the two
iOS XMR libraries against `SHA256SUMS`, and then runs `wallet/ios/bind.sh`,
XcodeGen and `wallet/ios/release.sh` in the export. The result is
`out/<version>/ios/Caribou-<version>-<build>.ipa` and a zip of its debug
symbols. Before anything is uploaded, the script checks the signature of the
app and both embedded frameworks, that the signer is a distribution
certificate of the expected team, that the provisioning profile is an App
Store profile for the bundle ID, that `get-task-allow` is off, and the
identifier, version and build number in the bundle. `--upload` then
validates and uploads the build; a few minutes later it appears in
TestFlight, or fails processing, which `xcrun altool --build-status` reports
(`wallet/ios/README.md`).

The App Store version is the tag's `X.Y.Z` with any pre-release suffix
dropped, since `CFBundleShortVersionString` allows three integers only; the
Go core still reports the full version. The build number is the commit count,
as on Android.

Signing is Xcode's automatic signing with a cloud-managed distribution
certificate, so the Mac holds no signing key and several people can cut an
iOS release, each signed into Xcode with their own Apple ID (Admin or Account
Holder role). An App Store Connect API key with the Admin role can provision
too. A key with the Developer role, such as the one the desktop release
notarizes with, can only upload: set `ASC_KEY_NO_PROVISIONING=1` with it, so
that provisioning goes through the signed-in Apple ID.

## Release checklist

The full release process, from forking the release branch to publishing,
is in [docs/release-checklist.md](../docs/release-checklist.md).
