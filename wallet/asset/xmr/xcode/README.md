# Apple SDK staging

The four Apple targets (`x86_64-apple-darwin`, `aarch64-apple-darwin`,
`aarch64-apple-ios`, `aarch64-apple-ios-simulator`) need an Xcode xip that the
image cannot download for itself. Drop it in this directory and they build.

    cp ~/Downloads/Xcode_26.1_Universal.xip wallet/asset/xmr/xcode/

That is the whole procedure. The name does not matter, the location within this
directory does not matter, and staging several xips is fine: the Dockerfile
hashes every `*.xip` it finds here and uses the one matching the pin. If none
match it stops immediately and prints what it found, rather than failing hours
later inside the first Apple target. `*.xip` is gitignored, and at ~2.8 GB one
must never be committed.

Leave this directory as it is to build only the Linux, Windows and Android
targets. That is supported and prints a notice, not an error: the Dockerfile
records whether a xip was staged and skips the four Apple targets when none was,
so the build succeeds and the image tags with the other six libraries in it. The
Apple extraction commands in `lib/README.md` then find nothing to read, and
everything else works as usual.

## Which file

Xcode **26.1**, the Universal xip, sha256

    434287e1e4c6ecbd6d5ea07e9f408fffcad2b82916b0a3fdfefa7fe60db4de93

Both values live in `xcode/PIN`, which the Dockerfile, `build-darwin.sh` and
`build-ios.sh` all read. That file is the one to edit for an Xcode bump; the copy
above is prose and the `PIN` file is what the builds enforce. (Before `PIN`
existed the hash sat in four places with three separate mismatch handlers, so a
bump that missed one left a build path silently trusting a stale xip.)

Get it from <https://developer.apple.com/download/all/> while signed in with an Apple
developer account. Sign-in is unavoidable: the pinned URL answers a plain
request with a 302 to `developer.apple.com/unauthorized/`, and the simplybs LFS
mirror answers 451 for that one path.

Take the version number literally. Xcode **26.1.1** sits next to 26.1 in that
list, is a near-identical name and size, and hashes `ed55d55f...`. It is a
different SDK and will not produce the pinned toolchain. Getting the wrong one
is the expected mistake here, which is why the build identifies the file by hash
and says so by name when nothing matches.

## Why this is needed at all

simplybs' `native/apple-sdk` package (MrCyjaneK's osxcross) pins the xip like
any other source:

    "kind":   "blob",
    "sha256": "434287e1e4c6ecbd6d5ea07e9f408fffcad2b82916b0a3fdfefa7fe60db4de93",
    "url":    "https://download.developer.apple.com/Developer_Tools/Xcode_26.1/XcodeXIP_26.1_Universal.xip"

`pack.GenerateSourceBuildPath` turns that URL into a path under the download
cache, and `pack.DownloadSource` skips the download entirely when something is
already sitting there. So the Dockerfile symlinks the staged file into

    contrib/depends/simplybs/.buildlib/source/download.developer.apple.com/Developer_Tools/Xcode_26.1/XcodeXIP_26.1_Universal.xip

and simplybs proceeds as if it had fetched it. Note the cache name is
`XcodeXIP_...` while Apple's own filename is `Xcode_...`; that mismatch is the
reason the file is matched on its hash instead of on its name.

Nothing is trusted about where the file came from, because the Dockerfile
hashes every xip in this directory itself and symlinks only a match.

That check is not redundant with simplybs, it replaces one that does not exist.
simplybs verifies only what it downloaded: `pack.DownloadSource` hands the
pinned sha256 to `DownloadFile` on the branch where the file is *absent*, and
`pack.ExtractSource`'s `case "blob"` — the kind this package uses — is a plain
copy with no hash check. A blob that is already sitting in the cache is
therefore never verified by simplybs at all. Staging a file into that cache is
only as safe as whoever staged it, which is why the check happens here.
