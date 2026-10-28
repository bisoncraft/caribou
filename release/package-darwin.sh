#!/usr/bin/env bash
# Build, sign, notarize and package the macOS desktop release: one .dmg per
# architecture (x86_64 and arm64), each an Electron shell around the Go
# `caribou` binary and the XMR library.
#
#   release/package-darwin.sh v1.2.0 [--zip]
#
# Everything runs on Linux inside the caribou-builder image:
#   - the Go binary is cross-compiled with osxcross, which needs the Xcode xip
#     staged in wallet/asset/xmr/xcode/ when the image is built;
#   - @electron/packager assembles Caribou.app from a prebuilt Electron;
#   - rcodesign signs with the Developer ID identity, submits to Apple's
#     notary service and staples the ticket;
#   - the .dmg is a genisoimage ISO 9660 image (Rock Ridge plus Apple
#     extensions, which macOS mounts with Finder metadata intact) compressed
#     by libdmg-hfsplus' `dmg`, the construction Bitcoin Core used for its
#     Linux-built dmgs. release/darwin/dmg/ holds the Finder layout: the
#     background image and the .DS_Store generator.
#
# The Electron floor is macOS 13; the Go binary is built with the same
# deployment target (MACOS_DEPLOYMENT_TARGET in common.sh).

source "$(dirname "$0")/common.sh"

ARCHS="amd64 arm64"
BUNDLE_ID="com.bisoncraft.caribou.desktop"
# Mounted-volume name. The .DS_Store's background alias embeds it, so the two
# must agree (see release/darwin/dmg/dsstore.py).
DMG_VOLUME="Caribou"

usage() {
	cat <<USAGE
usage: $(basename "$0") vX.Y.Z [options]
$(common_usage)
  --zip              also produce a .zip of the notarized .app per architecture
USAGE
}

# ---------------------------------------------------------------------------
electron_arch() { case "$1" in amd64) echo x64 ;; *) echo "$1" ;; esac; }

stage_build() {
	local want_zip="${1:-}"
	local out=/work/darwin
	rm -rf "$out"
	mkdir -p "$out"

	[[ -x /opt/osxcross/bin/oa64-clang ]] || die "the builder image has no macOS toolchain.
Stage the pinned Xcode xip in wallet/asset/xmr/xcode/ (see its README.md) and
rerun with --build-image."

	build_ui
	go_tags

	# Electron project: copied out of the read-only /release mount so npm can
	# write node_modules. One install serves both architectures.
	local eldir="${out}/electron"
	cp -r /release/darwin/electron "$eldir"
	(cd "$eldir" && npm ci --no-audit --no-fund)

	local arch
	for arch in $ARCHS; do
		local adir="${out}/${arch}" res="${out}/${arch}/mac"
		mkdir -p "$res"
		local cc; case "$arch" in amd64) cc=o64-clang ;; arm64) cc=oa64-clang ;; esac

		log "building caribou for darwin/${arch}"
		# The XMR dylib's install name is @executable_path/..., so the loader
		# finds it beside the binary with no rpath needed.
		PATH="/opt/osxcross/bin:$PATH" MACOSX_DEPLOYMENT_TARGET="$MACOS_DEPLOYMENT_TARGET" \
			CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" CC="$cc" CXX="${cc}++" \
			go build -trimpath "${GO_TAGS[@]}" -ldflags "$(go_ldflags)" -o "${res}/caribou" ./wallet/cmd/caribou
		with_xmr && cp "wallet/asset/xmr/lib/darwin-${arch}/libmonero_wallet2_api_c.dylib" "$res/"
		cp /release/darwin/caribouTemplate.png /release/darwin/caribouTemplate@2x.png "$res/"

		# Sign the Go binary and the dylib before they are copied into the
		# bundle's Resources. Bundle signing seals Resources rather than signing
		# code inside it, and the notary service rejects unsigned Mach-Os. The
		# wallet process needs no entitlements: Go does not JIT, and the dylib
		# carries the same team signature, so library validation passes.
		sign_macos "${res}/caribou"
		with_xmr && sign_macos "${res}/libmonero_wallet2_api_c.dylib"

		local earch; earch="$(electron_arch "$arch")"
		log "packaging Caribou.app for darwin/${earch}"
		(cd "$eldir" && node package.js --arch "$earch" --version "$VERSION" --build-version "$VERSION_NUM" \
			--bundle-id "$BUNDLE_ID" --icon /release/darwin/caribou.icns --extra "$res" --out "${adir}/out")
		local app="${adir}/out/Caribou-darwin-${earch}/Caribou.app"
		[[ -d "$app" ]] || die "packager produced no bundle at $app"

		sign_macos "$app" /release/darwin/entitlements.plist
		verify_macos_runtime "$app"
		notarize_macos "$app"

		local base="${APP}-${VERSION}-darwin-${arch}"
		log "building ${base}.dmg"
		local staging="${adir}/dmg"
		mkdir -p "${staging}/.background"
		cp -a "$app" "${staging}/"
		ln -s /Applications "${staging}/Applications"
		cp /release/darwin/dmg/background.png "${staging}/.background/"
		/opt/dmgtools/bin/python /release/darwin/dmg/dsstore.py --out "${staging}/.DS_Store" --volume "$DMG_VOLUME" \
			--app "$(basename "$app")" --background .background/background.png
		genisoimage -quiet -no-cache-inodes -D -l -probe -V "$DMG_VOLUME" -no-pad -r -dir-mode 0755 -apple \
			-o "${adir}/uncompressed.dmg" "$staging"
		# libdmg-hfsplus' stripped-down dmg tool: `dmg <input image> <output dmg>`.
		dmg "${adir}/uncompressed.dmg" "${out}/${base}.dmg" >/dev/null
		rm -rf "$staging" "${adir}/uncompressed.dmg"
		sign_macos "${out}/${base}.dmg"
		notarize_macos "${out}/${base}.dmg"

		if [[ "$want_zip" == "--zip" ]]; then
			log "building ${base}.zip"
			(cd "$(dirname "$app")" && zip -q -X -r -y "${out}/${base}.zip" "$(basename "$app")")
		fi
		rm -rf "${adir}/out"
	done
	rm -rf "$eldir"

	log "artifacts:"
	ls -1 "$out" >&2
}

# ---------------------------------------------------------------------------
host_main() {
	parse_common_args "$@"
	local want_zip=""
	for a in "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}"; do
		case "$a" in
		--zip) want_zip="--zip" ;;
		*) die "unknown option: $a" ;;
		esac
	done
	resolve_version
	load_env
	export_source
	local arch
	for arch in $ARCHS; do
		stage_xmr_libs "darwin-${arch}" libmonero_wallet2_api_c.dylib
	done
	ensure_image
	run_stage build $want_zip
}

if [[ "${1:-}" == "--stage" ]]; then
	"stage_$2" "${@:3}"
else
	host_main "$@"
fi
