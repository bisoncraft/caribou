# caribou-desktop

The desktop build of Caribou: the wallet core and appserver in one process,
presented in a native WebView window with a system tray icon. It is a package
of the root module and builds from the repository root.

## Build and run

Requires cgo. On Linux, install the GTK and WebKitGTK development packages
first:

```bash
sudo apt-get install libgtk-3-dev libwebkit2gtk-4.1-dev
```

Release builds always carry the `xmr` tag, which links
`libmonero_wallet2_api_c` from `wallet/asset/xmr/lib/<platform>/`. See
`wallet/asset/xmr/lib/README.md` for obtaining the library; the binary looks
for it beside the executable and in `/usr/lib/caribou`.

```bash
go build -tags xmr -o caribou-desktop ./wallet/cmd/caribou-desktop
./caribou-desktop
```

Omit `-tags xmr` for a build without Monero support, which needs no native
library.

Every source file carries a `cgo` build constraint, so a `CGO_ENABLED=0`
`go build ./...` from the root skips this package rather than failing.

## Windows resources

`winres.json` describes the icon and version resources embedded in the Windows
binary. `rsrc_windows_*.syso` are the generated objects and are picked up
automatically by a `GOOS=windows` build. Regenerate after editing
`winres.json`:

```bash
go generate ./wallet/cmd/caribou-desktop
```

## Packaging

Installer packaging (Debian, snap, Windows installer, macOS dmg) lives in
the top-level `release/` directory; see
[release/README.md](../../../release/README.md).
