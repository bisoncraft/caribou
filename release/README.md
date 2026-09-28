# Desktop release packaging

Scripts that build, sign and package the Caribou desktop app for Linux,
Windows and macOS, all from a Linux host. Each platform has one script that
takes a release tag and produces installers with no further input:

```bash
release/package-linux.sh   v1.2.0   # .deb + .tar.gz (amd64)
release/package-windows.sh v1.2.0   # signed .msi + .zip (amd64)
release/package-darwin.sh  v1.2.0   # notarized .dmg (x86_64 and arm64)
release/release.sh         v1.2.0   # all three, SHA256SUMS + GPG signature, draft GitHub release
```

Every release build carries the `xmr` tag. The prebuilt
`libmonero_wallet2_api_c` libraries are read from `wallet/asset/xmr/lib/`
(or `--xmr-libs <dir>`) and verified against the `SHA256SUMS` committed at
the tag being built before they are linked.

## How a build runs

1. The tag is resolved to a commit and every version form is derived from it
   (semver for the binaries, `~` form for dpkg, numeric for MSI). `--ref
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
holds Go, Node, MinGW, msitools, rcodesign, jsign, osslsigncode, genisoimage
and libdmg-hfsplus, and an osxcross toolchain when a macOS SDK is available.
Every download is pinned by checksum in the Dockerfile.

The image builds automatically on first use as `caribou-builder:<base>`.
The base is chosen by `package-linux.sh` from the Linux XMR library: the
oldest Ubuntu whose glibc and libstdc++ can link it (22.04 when the library
needs glibc 2.35 or older, 24.04 otherwise), since that library sets the
.deb's floor and the binary should be built on the oldest base that can
satisfy it. The other platforms reuse whatever builder image exists. When a
new base is needed, the image is rebuilt on it and the previous builder image
(identified by the `caribou.builder` label, nothing else) is removed once the
new one exists. Rebuild after changing the Dockerfile or staging the Xcode
xip with `--build-image`.

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
and version resources by go-winres. msitools' `wixl` builds the MSI from
`release/windows/Caribou.wxs` (WiX v3 dialect, the subset wixl implements);
nothing on Linux runs the Windows Installer ICE validators, so install the
MSI on a Windows machine before publishing. The MSI refuses to install where
the Edge WebView2 Runtime is absent (it ships with Windows 11 and Microsoft
Edge) and points at the download. The `.exe`, the DLLs and then the MSI are
each Authenticode-signed with an RFC 3161 timestamp, and each signature is
verified with osslsigncode against the pinned Microsoft Identity
Verification root in `release/windows/` before the build is accepted.

**macOS.** Electron wraps the CLI `caribou` binary; `release/darwin/electron`
holds the shell. The Go binary and XMR dylib are signed first, then the
packaged `Caribou.app`, which is notarized and stapled; the `.dmg` built
around it is signed, notarized and stapled in turn. Notarization normally
returns in minutes, but a team's first submissions have taken over an hour
in Apple's queue; the script waits up to six hours (`NOTARY_WAIT_SECONDS`
overrides) and `rcodesign notary-list` with the API key shows the queue.
The dmg is a genisoimage HFS-hybrid image compressed by `dmg`, the
construction Bitcoin Core used for its Linux-built images. `--zip` also emits a `.zip` of the
notarized app. Verify on a Mac with `spctl --assess --type open
--context context:primary-signature -v Caribou.dmg` and by launching the
installed app; an Apple Silicon Mac can also run the x86_64 build under
Rosetta.

## Release checklist

1. Bump `wallet/app/version.go` on master and tag the release commit.
2. Publish the XMR libraries for the tag if their `SHA256SUMS` changed
   (`wallet/asset/xmr/publish-libs.sh`), or make sure the pinned ones are in
   `wallet/asset/xmr/lib/`.
3. `release/release.sh vX.Y.Z --publish` creates a draft GitHub release with
   every artifact, `SHA256SUMS` and `SHA256SUMS.asc`.
4. Install-test each artifact: the `.deb` in a clean container or VM, the
   MSI on Windows, both dmgs on a Mac.
5. Publish the draft.
