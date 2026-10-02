#!/usr/bin/env bash
# Regenerates the desktop app and tray icons from caribou-mark.svg:
#
#   wallet/cmd/caribou-desktop/src/   window, tray, installer and macOS icons
#   wallet/cmd/caribou/icons/         tray icons for the browser-UI build
#   rsrc_windows_*.syso in both       Windows exe resources, via go generate
#
#   docs/branding/make-desktop-icons.sh
#
# Needs resvg (cargo install resvg, or a distro package), ImageMagick, python3
# and Go. resvg rather than ImageMagick's own SVG renderer, which antialiases
# the thin tines badly at small sizes.
#
# The variants follow the web UI's theme mapping (newui/src/theme.ts): brown
# antlers for the warm theme, blue for cool and light. The plain icons are the
# brown mark on transparency, since warm is the default theme. The dark and
# light variants put the mark on a circle of the warm and light themes'
# --body-bg. The macOS icon is a rounded tile on the same ground as the iOS
# app icon (wallet/ios/icon/appicon.svg).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
desktop="$root/wallet/cmd/caribou-desktop"
tray="$root/wallet/cmd/caribou/icons"
mark="$here/caribou-mark.svg"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

command -v resvg   >/dev/null || { echo "error: resvg not found." >&2; exit 1; }
command -v python3 >/dev/null || { echo "error: python3 not found." >&2; exit 1; }
command -v go      >/dev/null || { echo "error: go not found." >&2; exit 1; }
if command -v magick >/dev/null; then im=(magick)
elif command -v convert >/dev/null; then im=(convert)
else echo "error: ImageMagick not found." >&2; exit 1
fi

# The mark's <g> element, which fills 960 of the master's 1024 units wide.
group="$(sed -n '/^<g /,/^<\/g>/p' "$mark")"
blue() {
    sed -e 's/#a9761f/#78b1fb/; s/#855a15/#294de7/; s/#503509/#0204a2/; s/#6b4711/#183bd5/'
}

# compose NAME WIDTH COLOR [BACKGROUND...]
# Writes $work/NAME.svg: a 1024 canvas holding BACKGROUND elements, then the
# mark scaled to WIDTH units wide and centered. COLOR is brown or blue.
compose() {
    local name="$1" width="$2" color="$3"; shift 3
    local s t g="$group"
    s="$(python3 -c "print(f'{$width/960:.6f}')")"
    t="$(python3 -c "print(f'{512-512*$width/960:.3f}')")"
    [[ "$color" == blue ]] && g="$(blue <<<"$g")"
    {
        echo '<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">'
        printf '%s\n' "$@"
        echo "<g transform=\"translate($t,$t) scale($s)\">"
        echo "$g"
        echo '</g></svg>'
    } >"$work/$name.svg"
}

# render NAME SIZE OUT
render() { resvg -w "$2" -h "$2" "$work/$1.svg" "$3"; }

compose plain 960 brown
compose dark  720 brown '<circle cx="512" cy="512" r="512" fill="#17130d"/>'
compose light 720 blue  '<circle cx="512" cy="512" r="512" fill="#f7f2e8"/>'
compose macos 640 brown \
    '<defs><linearGradient id="bg" gradientUnits="userSpaceOnUse" x1="100" y1="100" x2="924" y2="924"><stop offset="0" stop-color="#241c11"/><stop offset="1" stop-color="#141009"/></linearGradient></defs>' \
    '<rect x="100" y="100" width="824" height="824" rx="185" fill="url(#bg)"/>'

# Desktop window, tray and notification icons. caribou.png keeps the 100px
# size the bison had; the Linux packages install it as the 128x128 icon, and
# caribou.svg beside it as the scalable one.
src="$desktop/src"
render plain 16  "$src/caribou-16.png"
render plain 100 "$src/caribou.png"
render plain 256 "$src/caribou-256.png"
render dark  256 "$src/caribou-dark-256.png"
render light 256 "$src/caribou-light-256.png"
render plain 32  "$src/favicon-32.png"
cp "$mark" "$src/caribou.svg"

# The -bw (black and white) icon is the systray menu item's. Greyscale alone
# leaves the brown too dark to read on a dark menu, so lift it.
"${im[@]}" "$src/favicon-32.png" -colorspace Gray -level 0,70% -colorspace sRGB -strip \
    "PNG32:$src/favicon-bw-32.png"
"${im[@]}" "$src/favicon-32.png"    "$src/favicon-32.ico"
"${im[@]}" "$src/favicon-bw-32.png" "$src/favicon-bw-32.ico"

# The MSI's Add/Remove Programs icon (ARPPRODUCTICON in Caribou.wxs). A
# separate .ico rather than the exe, because the MSI stores the whole file.
for n in 16 24 32 48 64 256; do
    render plain "$n" "$work/ico-$n.png"
done
"${im[@]}" "$work"/ico-{16,24,32,48,64,256}.png "$src/caribou.ico"

# macOS: the iconset, then the icns built from it. iconutil is macOS only, so
# the icns is assembled here: a header, then one PNG per OSType.
set_="$src/caribou.iconset"
for n in 16 32 64 128 256 512; do
    render macos "$n" "$set_/icon_${n}x${n}.png"
done
for n in 16 32 128 256 512; do
    render macos "$((n * 2))" "$set_/icon_${n}x${n}@2x.png"
done
python3 - "$set_" "$src/caribou.icns" <<'EOF'
import struct, sys
d, out = sys.argv[1], sys.argv[2]
types = [("icp4", "16x16"), ("icp5", "32x32"), ("icp6", "64x64"),
         ("ic07", "128x128"), ("ic08", "256x256"), ("ic09", "512x512"),
         ("ic10", "512x512@2x"), ("ic11", "16x16@2x"), ("ic12", "32x32@2x"),
         ("ic13", "128x128@2x"), ("ic14", "256x256@2x")]
body = b""
for t, n in types:
    png = open(f"{d}/icon_{n}.png", "rb").read()
    body += t.encode() + struct.pack(">I", len(png) + 8) + png
open(out, "wb").write(b"icns" + struct.pack(">I", len(body) + 8) + body)
EOF

# The browser-UI build's tray icons are the same set.
for f in caribou-256.png caribou-dark-256.png caribou-light-256.png \
         favicon-32.png favicon-32.ico favicon-bw-32.png favicon-bw-32.ico; do
    cp "$src/$f" "$tray/$f"
done

# Windows exe resources (winres.json in each command).
(cd "$desktop" && go generate ./gen.go)
(cd "$tray/.." && go generate ./gen.go)
