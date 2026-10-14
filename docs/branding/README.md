# Branding: replacing the Bison Wallet artwork

The rename to Caribou (see [`../caribou-rename.md`](../caribou-rename.md)) moved
file paths and identifiers but did not replace any artwork. Most icon files were
renamed from `bisonwallet-*` / `bw-*` to `caribou-*`, so a filename search looks
clean while the images themselves were still the blue Bison Wallet bison.

The Caribou mark is a pair of gold antlers. It has replaced the bison in the
desktop app, the `caribou` server binary and the web UI. The old web UI in
`wallet/appserver/site/`, the last place the bison and the legacy DCRDEX
artwork appeared, has been removed.

There is no vector master for the antlers in the repository. Every icon is a
raster export, so new sizes are best cut from the largest existing PNG
(`release/linux/icons/caribou-512.png`) until a master is added here.

## Done: the antlers

### Desktop app: `wallet/cmd/caribou-desktop/src/`

Regenerated in #182. Embedded via `//go:embed` in `main.go`, `icon.go`,
`icon_linux.go` and `icon_windows.go`.

| File | Used by |
| --- | --- |
| `caribou.ico` | Windows executable (winres `#1` and `#32512`) and the NSIS installer |
| `caribou.png` | window icon (`Icon`) |
| `caribou-dark-256.png` | toast notification icon (`main.go`) |
| `caribou-256.png` | nothing in the build, kept as a 256 px export |
| `favicon-32.png` / `.ico` | tray icon |
| `favicon-bw-32.png` / `.ico` | greyed tray icon (`FavIconBW`) |

Packaging artwork lives under `release/`: the hicolor PNG set in
`release/linux/icons/`, `caribou.icns` and the menu-bar template images in
`release/darwin/`, and the dmg background in `release/darwin/dmg/`.

### Server tray icons: `wallet/cmd/caribou/icons/`

Copied from the desktop set so both binaries show the same mark. Embedded via
`//go:embed` in `icons.go` and `icons_windows.go`, and `caribou.ico` is stamped
into the Windows executable through `winres.json` (`#1` and `#32512`, as in the
desktop app). Regenerate the `.syso` files with `go generate` in that directory
after changing it.

| File | Used by |
| --- | --- |
| `caribou.ico` | Windows executable icon |
| `favicon-32.png` / `.ico` | tray icon |
| `favicon-bw-32.png` / `.ico` | greyed tray icon |

### Web UI: `wallet/appserver/newui/`

The desktop UI already uses the antlers. `shared/img/favicon-brown.png` and
`favicon-blue.png` follow the theme (`newui/src/theme.ts`), and `img.logo-full`
renders `shared/img/logo-antlers-brown.png` or `logo-antlers-blue.png`
(`newui/src/scss/components.scss`).

## Notes

- Keep the `-bw` suffix. It means black and white, not "Bison Wallet" (the
  Bison Wallet files used a `bw-` prefix). Those files are the monochrome
  variant used for the systray menu item.
- Verify a swap with `go build ./...`, then build the app and confirm the tray
  icon, window icon and installer icon all render.
