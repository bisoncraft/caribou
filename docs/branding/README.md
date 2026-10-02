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
`caribou-mark.svg` here is the vector master for the antler mark. Its paths
are the ones in `wallet/ios/icon/appicon.svg`, without the iOS background tile.

## Needs replacement

### Desktop app and tray icons: done

The desktop app (`wallet/cmd/caribou-desktop/src/`) and the browser-UI build's
tray icons (`wallet/cmd/caribou/icons/`) now carry the antler mark. They are
all generated from `caribou-mark.svg` by `make-desktop-icons.sh`, which also
assembles `caribou.icns` and regenerates the Windows `rsrc_windows_*.syso`
resources. Rerun it rather than editing those files by hand.

One file there is still unchecked: `caribou-installer-bg.tiff`, the macOS DMG
background. Look at it before shipping a macOS build.

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

1. Update `caribou-mark.svg` and rerun `make-desktop-icons.sh` for the desktop
   and tray icons.
2. Replace each web UI file above in place, keeping the same names and
   dimensions.
3. Keep the `-bw` suffix. It means black and white, not "Bison Wallet" (the
   Bison Wallet files used a `bw-` prefix). Those files are the monochrome
   variant used for the systray menu item, so the replacement set needs a
   greyscale version too. The script derives it from the mark.
4. Verify: `go build ./...`, then build the desktop app and confirm the tray
   icon, window icon, and installer icon all render.
