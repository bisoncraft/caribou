#!/usr/bin/env bash
# Build, sign and package the command-line release: the `caribou` binary
# (wallet/cmd/caribou) with the XMR library beside it, one archive per
# target:
#
#   linux/amd64, linux/arm64      .tar.gz
#   darwin/amd64, darwin/arm64    .zip, Developer ID signed and notarized
#   windows/amd64                 .zip, Authenticode signed, system tray build
#
#   release/package-cli.sh v1.2.0
#
# Every target is cross-compiled with cgo inside the caribou-builder image:
# gcc and aarch64-linux-gnu-gcc for Linux, osxcross for macOS and MinGW for
# Windows. Each binary loads the library from its own directory: through the
# $ORIGIN rpath on Linux, the dylib's @executable_path install name on macOS
# and the DLL search order on Windows.
#
# A notarization ticket cannot be stapled to a zip or to a bare Mach-O
# binary, so Gatekeeper fetches the ticket online when a macOS binary first
# runs.

source "$(dirname "$0")/common.sh"

TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"

usage() {
	cat <<USAGE
usage: $(basename "$0") vX.Y.Z [options]
$(common_usage)
USAGE
}

# xmr_lib_file <os>: the XMR library's file name on that OS.
xmr_lib_file() {
	case "$1" in
	linux) echo libmonero_wallet2_api_c.so ;;
	darwin) echo libmonero_wallet2_api_c.dylib ;;
	windows) echo libmonero_wallet2_api_c.dll ;;
	*) die "no XMR library for $1" ;;
	esac
}

# ---------------------------------------------------------------------------
stage_build() {
	local out=/work/cli
	rm -rf "$out"
	mkdir -p "$out"

	[[ -x /opt/osxcross/bin/oa64-clang ]] || die "the builder image has no macOS toolchain.
Stage the pinned Xcode xip in wallet/asset/xmr/xcode/ (see its README.md) and
rerun with --build-image."
	command -v aarch64-linux-gnu-gcc >/dev/null || die "the builder image has no linux/arm64 cross compiler; rerun with --build-image"

	build_ui
	windows_resources "$CLI_APP" amd64
	xmr_import_lib amd64

	local target
	for target in $TARGETS; do
		build_target "${target%/*}" "${target#*/}" "$out"
	done

	log "artifacts:"
	ls -1 "$out" >&2
}

# build_target <os> <arch> <out>: build, sign and archive one target. The
# archive holds a single directory named like the archive, as the CLI
# archives always have.
build_target() {
	local os="$1" arch="$2" out="$3"
	local base="${CLI_APP}-${VERSION}-${os}-${arch}"
	local dir="${out}/${base}" exe="$CLI_APP" ldextra="" lib
	lib="$(xmr_lib_file "$os")"
	local -a env=(CGO_ENABLED=1 GOOS="$os" GOARCH="$arch")
	case "${os}/${arch}" in
	linux/amd64) ;;
	linux/arm64) env+=(CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++) ;;
	darwin/amd64) env+=(PATH="/opt/osxcross/bin:$PATH" CC=o64-clang CXX=o64-clang++ MACOSX_DEPLOYMENT_TARGET="$MACOS_DEPLOYMENT_TARGET") ;;
	darwin/arm64) env+=(PATH="/opt/osxcross/bin:$PATH" CC=oa64-clang CXX=oa64-clang++ MACOSX_DEPLOYMENT_TARGET="$MACOS_DEPLOYMENT_TARGET") ;;
	windows/amd64)
		env+=(CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++)
		exe+=".exe"
		ldextra="-H windowsgui"
		;;
	*) die "unsupported target ${os}/${arch}" ;;
	esac
	# The system tray build is Windows only.
	if [[ "$os" == windows ]]; then go_tags systray; else go_tags; fi

	mkdir -p "$dir"
	log "building ${exe} for ${os}/${arch}"
	env "${env[@]}" go build -trimpath "${GO_TAGS[@]}" -ldflags "$(go_ldflags "$ldextra")" \
		-o "${dir}/${exe}" "./wallet/cmd/${CLI_APP}"
	with_xmr && cp "wallet/asset/xmr/lib/${os}-${arch}/${lib}" "$dir/"

	case "$os" in
	linux)
		log "building ${base}.tar.gz"
		tar -C "$out" --owner=0 --group=0 --sort=name --mtime="@0" -czf "${out}/${base}.tar.gz" "$base"
		;;
	darwin)
		# No entitlements: Go does not JIT, and the dylib carries the same team
		# signature, so library validation passes under the hardened runtime.
		sign_macos "${dir}/${exe}"
		with_xmr && sign_macos "${dir}/${lib}"
		log "building ${base}.zip"
		(cd "$out" && zip -q -X -r -y "${base}.zip" "$base")
		notarize_macos "${out}/${base}.zip" --no-staple
		;;
	windows)
		sign_windows_file "${dir}/${exe}"
		verify_windows_file "${dir}/${exe}"
		if with_xmr; then
			sign_windows_file "${dir}/${lib}"
			verify_windows_file "${dir}/${lib}"
		fi
		log "building ${base}.zip"
		(cd "$out" && zip -q -X -r "${base}.zip" "$base")
		;;
	esac
	rm -rf "$dir"
}

# ---------------------------------------------------------------------------
host_main() {
	parse_common_args "$@"
	[[ ${#EXTRA_ARGS[@]} -eq 0 ]] || die "unknown option: ${EXTRA_ARGS[0]}"
	resolve_version
	load_env
	export_source
	local target
	for target in $TARGETS; do
		stage_xmr_libs "${target%/*}-${target#*/}" "$(xmr_lib_file "${target%/*}")"
	done
	if with_xmr; then
		select_image "$(linux_base_for_lib "${SRC_DIR}"/wallet/asset/xmr/lib/linux-{amd64,arm64}/libmonero_wallet2_api_c.so)"
	else
		select_image 22.04
	fi
	ensure_image
	run_stage build
}

if [[ "${1:-}" == "--stage" ]]; then
	"stage_$2" "${@:3}"
else
	host_main "$@"
fi
