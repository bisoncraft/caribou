#!/usr/bin/env bash
# Regenerates Caribou/Assets.xcassets/AppIcon.appiconset/AppIcon-1024.png from
# appicon.svg. Only the 1024 master is committed: the asset catalog uses Xcode's
# single-size app icon, so every home-screen / Spotlight / Settings size is
# derived from it at build time.
#
#   cd wallet/ios/icon && ./make-appicon.sh
#
# The mark is the Caribou antlers -- the same artwork the web UI ships as
# shared/img/logo-antlers-brown.png, and appicon.svg is the only vector copy of
# it in the tree. Its ink is 1.4:1, so it is placed by measured bounding box
# rather than by the SVG canvas: inset to 82% of the width and centered, which
# keeps the outer tines clear of the iOS superellipse mask. The ground is the
# warm theme's --body-bg (#17130d), lifted to a #241c11 -> #141009 diagonal so
# the tile does not read as flat black behind the brown.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="$here/../Caribou/Assets.xcassets/AppIcon.appiconset/AppIcon-1024.png"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

command -v qlmanage >/dev/null || { echo "error: qlmanage not found (macOS only)." >&2; exit 1; }
command -v swiftc   >/dev/null || { echo "error: swiftc not found (install Xcode)." >&2; exit 1; }

# Quick Look is the only SVG rasterizer guaranteed on a stock Mac; it writes
# <name>.png next to -o's directory.
qlmanage -t -s 1024 -o "$work" "$here/appicon.svg" >/dev/null 2>&1
raster="$work/appicon.svg.png"
[[ -f "$raster" ]] || { echo "error: qlmanage did not rasterize appicon.svg." >&2; exit 1; }

# App Store Connect rejects an app icon that carries an alpha channel, even one
# that is fully opaque, so re-encode without it (ITMS "Invalid Image").
swiftc -O -o "$work/flatten" "$here/flatten.swift"
"$work/flatten" "$raster" "$out"

if [[ "$(sips -g hasAlpha "$out" | awk '/hasAlpha/{print $2}')" != "no" ]]; then
    echo "error: $out still has an alpha channel." >&2
    exit 1
fi
echo "Wrote $out"
sips -g pixelWidth -g pixelHeight -g hasAlpha "$out" | tail -3
