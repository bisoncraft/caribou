# Release packaging

Scripts that build, sign and package the Caribou desktop app and the
command-line `caribou` binary for Linux, Windows and macOS, all from a Linux
host. Each script takes a release tag and produces its artifacts with no
further input:

```bash
release/package-linux.sh   v1.2.0   # .deb + .tar.gz (amd64)
release/package-windows.sh v1.2.0   # signed setup.exe + .zip (amd64)
release/package-darwin.sh  v1.2.0   # notarized .dmg (x86_64 and arm64)
release/package-cli.sh     v1.2.0   # command-line archives (see below)
release/release.sh         v1.2.0   # all four, one SHA256SUMS + GPG signature, draft GitHub release
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
4. Inside the image: web UI build, Go build, signing, packaging. Outputs land
   in `release/out/<version>/<os>/`.

Toolchain caches (Go, npm, Electron, dotnet) persist in `release/out/.cache`
across runs. `release/out` and `release/.env` are gitignored.

## The builder image

`release/Dockerfile` builds `caribou-builder` on Ubuntu 22.04, which fixes
the Linux binary's glibc and WebKitGTK floor at Ubuntu 22.04 / Debian 12. It
holds Go, Node, MinGW, an aarch64 Linux cross compiler, NSIS, rcodesign,
jsign, osslsigncode, genisoimage and libdmg-hfsplus, and an osxcross
toolchain when a macOS SDK is available.
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

## Release checklist

The full release process, from forking the release branch to publishing,
is in [docs/release-checklist.md](../docs/release-checklist.md).
