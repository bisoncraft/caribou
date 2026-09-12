# Branding TODO: replacing the Bison Wallet artwork

The rename to Caribou (see [`../caribou-rename.md`](../caribou-rename.md)) moved
file paths and identifiers but did **not** replace any artwork. Most icon files
were renamed from `bisonwallet-*` / `bw-*` to `caribou-*`, so a filename search
looks clean while the images themselves are still the blue Bison Wallet bison.
At the larger sizes the macOS app icon renders the words "Bison Wallet" directly
into the image.

Everything listed under "Needs replacement" is live: it ships in the desktop
app, the web UI, the installers, or the marketing site. None of it can simply be
deleted without leaving blank icons and broken images, so each file needs a new
asset at the same path, name, and dimensions. Once new artwork exists the swap
is mechanical.

The old source SVGs that used to sit in this directory (`bison-square.svg`,
`bisonwallet-full-*.svg`) were unreferenced by any code and have been deleted.
There is currently no vector master for the bison mark in the repository.

## Needs replacement

### Desktop app: `wallet/cmd/caribou-desktop/src/`

Embedded via `//go:embed` in `icon.go`, `icon_linux.go`, and `icon_windows.go`,
and consumed by `electron/forge.config.js` for packaging.

| File | Size | Currently shows | Used by |
| --- | --- | --- | --- |
| `caribou.iconset/icon_16x16.png` … `icon_512x512@2x.png` | 16 to 1024 | Bison, **with "Bison Wallet" text** at 128px and above | macOS app icon source |
| `caribou.icns` | — | as above | `forge.config.js:11,52` (macOS bundle) |
| `caribou.png` | 128 | Bison | `forge.config.js:62,74` (deb/rpm), `//go:embed` |
| `caribou-16.png`, `caribou-256.png`, `caribou-dark-256.png`, `caribou-light-256.png` | 16, 256 | Bison, dark and light circular variants | tray and window icons |
| `favicon-32.png` / `.ico` | 32 | Bison | `//go:embed`, `forge.config.js:42` (Windows installer icon) |
| `favicon-bw-32.png` / `.ico` | 32 | Greyed bison | `//go:embed` as `FavIconBW`, systray menu item |
| `caribou-installer-bg.tiff` | — | unverified, check before shipping | macOS DMG background |

`caribou.icns` is generated from `caribou.iconset` with `iconutil`, so regenerate
it rather than editing it directly.

Note that `caribou.svg` is **not** a bison. It is the DCRDEX gradient mark. It
is off-brand for Caribou in a different way and also wants replacing.

`pkg-debian.sh:92-93` installs it as `hicolor/scalable/apps/caribou.svg`
alongside `caribou.png`, so `org.bisoncraft.caribou.desktop` names it as
`Icon=caribou`. The name in the desktop entry has to keep matching the
installed basename, or the entry gets no icon at all. The snap sets an absolute
path over the top of it (`snapcraft.yaml.in:65`) and is unaffected either way.

### Server tray icons: `wallet/cmd/caribou/icons/`

Embedded via `//go:embed` in `icons.go` and `icons_windows.go`.

| File | Size | Currently shows |
| --- | --- | --- |
| `caribou-256.png`, `caribou-dark-256.png`, `caribou-light-256.png` | 256 | Bison |
| `favicon-32.png` / `.ico`, `favicon-bw-32.png` / `.ico` | 32 | Bison |

### Web UI: `wallet/appserver/`

Referenced from SCSS `content: url(...)` rules rather than `src` attributes, so
grepping the templates alone will not find them.

| File | Size | Used by |
| --- | --- | --- |
| `shared/img/logo-full.png` | 97x50 | `newui/src/scss/components.scss:52` |
| `shared/img/favicon.png` | 64 | shared UI favicon |
| `site/src/img/logo-full.png` | 97x50 | `site/src/css/main.scss:128` |
| `site/src/img/logo-square.png` | 50x50 | `site/src/css/main.scss:119` |
| `site/src/img/favicon.png` | 64 | site favicon |

All five are the bison. The components that render them live in
`newui/src/components/` (`TopBar.tsx`, `Loading.tsx`, `InitWizard.tsx`,
`UnlockForm.tsx`) and in `site/src/html/bodybuilder.tmpl`, `forms.tmpl`, and
`mm.tmpl`.

## Deliberately not changing

- `wallet/appserver/site/src/img/softened-icon.png`, `softened-icon-dark.png`,
  `logo_icon_v1.svg`, `logo_wide_v1.svg`, `logo_wide_dark_v1.svg` are **DCRDEX**
  artwork, not Bison Wallet artwork. They are driven by the `UseDEXBranding`
  flag (`wallet/core/types.go:99`), which exists to show legacy DCRDEX branding
  on purpose. Leave them alone.
- `notifications.ts:94` points desktop notifications at `softened-icon.png`,
  which is DCRDEX branding showing unconditionally rather than behind the flag.
  Worth deciding whether that should become the Caribou mark.

## When new artwork lands

1. Drop a vector master in this directory so future exports do not have to be
   traced back out of the PNGs.
2. Replace each file above in place, keeping the same names and dimensions.
3. Regenerate `caribou.icns` from `caribou.iconset` with `iconutil`.
4. Keep the `-bw` suffix. It means black and white, not "Bison Wallet" (the
   Bison Wallet files used a `bw-` prefix). Those files are the monochrome
   variant used for the systray menu item, so the replacement set needs a
   greyscale version too.
5. Verify: `go build ./...`, then build the desktop app and confirm the tray
   icon, window icon, and installer icon all render.
