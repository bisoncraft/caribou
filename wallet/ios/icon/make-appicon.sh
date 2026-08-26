#!/usr/bin/env bash
# Regenerates Caribou/Assets.xcassets/AppIcon.appiconset/AppIcon-1024.png from
# appicon.svg. Only the 1024 master is committed: the asset catalog uses Xcode's
# single-size app icon, so every home-screen / Spotlight / Settings size is
# derived from it at build time.
#
#   cd wallet/ios/icon && ./make-appicon.sh
#
# The mark is wallet/appserver/site/src/img/logo_icon_v1.svg, inset to ~66% of
# the canvas so the iOS superellipse mask does not clip it, over the brand
# indigo gradient. Two alternate grounds (white, and the #0f1115 app dark) are
# kept in git history if the ground ever needs to change; only the SVG's
# background rect and mark fills differ.
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
