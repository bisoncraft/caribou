#!/usr/bin/env bash
# Build, sign and package the Windows desktop release: a signed MSI installer
# and a signed portable zip, both amd64, both carrying the XMR library.
#
#   release/package-windows.sh v1.2.0
#
# The .exe is cross-compiled with MinGW inside the caribou-builder image, the
# MSI is built there with msitools' wixl from release/windows/Caribou.wxs, and
# both are Authenticode-signed with jsign (Azure Artifact Signing, or a local
# PKCS#12 for testing). Order matters: the .exe and DLLs are signed before the
# MSI is built so the installer carries signed files, then the MSI itself is
# signed.
#
# Nothing on Linux runs the Windows Installer ICE validators, so an install
# test on a Windows machine is part of every release.

source "$(dirname "$0")/common.sh"

ARCH="amd64"

usage() {
	cat <<USAGE
usage: $(basename "$0") vX.Y.Z [options]
$(common_usage)
USAGE
}

# ---------------------------------------------------------------------------
stage_build() {
	local out=/work/windows stage=/work/windows/stage
	rm -rf "$out"
	mkdir -p "$stage"

	build_ui
	go_tags

	# Icon and version resources. The checked-in .syso objects carry whatever
	# version was current when they were generated; regenerate them stamped
	# with this release's version so Explorer's Details tab matches the binary.
	log "generating Windows resources for ${VERSION}"
	(cd "wallet/cmd/${APP}" && go-winres make --in winres.json --arch "$ARCH" \
		--product-version "$VERSION" --file-version "${VERSION_NUM}.0")

	# MinGW cannot link against a .dll directly; make an import library. It is
	# named .a rather than .dll.a because webview_go links with -static, which
	# makes the linker skip .dll.a files.
	with_xmr && log "generating import library for libmonero_wallet2_api_c.dll" &&
		(cd "wallet/asset/xmr/lib/windows-${ARCH}" &&
		gendef libmonero_wallet2_api_c.dll >/dev/null &&
		x86_64-w64-mingw32-dlltool -d libmonero_wallet2_api_c.def -l libmonero_wallet2_api_c.a -D libmonero_wallet2_api_c.dll)

	log "building ${APP}.exe for windows/${ARCH}"
	# /opt/winsdk holds a stub EventToken.h: webview_go's bundled WebView2.h
	# includes that Windows SDK header, which MinGW does not ship.
	CGO_ENABLED=1 GOOS=windows GOARCH="$ARCH" \
		CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ CGO_CXXFLAGS="-I/opt/winsdk" \
		go build -trimpath "${GO_TAGS[@]}" -ldflags "$(go_ldflags '-H windowsgui')" -o "${stage}/${APP}.exe" "./wallet/cmd/${APP}"

	cp /opt/webview2/runtimes/win-x64/native/WebView2Loader.dll "$stage/"
	cp /usr/x86_64-w64-mingw32/lib/libwinpthread-1.dll "$stage/"
	with_xmr && cp "wallet/asset/xmr/lib/windows-${ARCH}/libmonero_wallet2_api_c.dll" "$stage/"

	# WebView2Loader.dll is Microsoft-signed already; the other three are ours.
	sign_windows_file "${stage}/${APP}.exe"
	verify_windows_file "${stage}/${APP}.exe"
	if with_xmr; then
		sign_windows_file "${stage}/libmonero_wallet2_api_c.dll"
		verify_windows_file "${stage}/libmonero_wallet2_api_c.dll"
	fi
	sign_windows_file "${stage}/libwinpthread-1.dll"
	verify_windows_file "${stage}/libwinpthread-1.dll"

	local base="${APP}-${VERSION}-windows-${ARCH}"
	log "building ${base}.zip"
	(cd "$stage" && zip -q -X -r "${out}/${base}.zip" .)

	log "building ${base}.msi"
	# wixl resolves File sources against the working directory and the .wxs
	# file's own directory (where caribou.ico lives); it rejects absolute paths.
	# It also leaves an Icon/ scratch directory in the working directory, which
	# is why the zip above is made first. VersionNext bounds the same-version
	# upgrade range, see Caribou.wxs.
	local next
	next="${VERSION_NUM%.*}.$((${VERSION_NUM##*.} + 1))"
	(cd "$stage" && wixl -a x64 \
		-D "Version=${VERSION_NUM}" -D "VersionNext=${next}" \
		-D "XmrEnabled=$(with_xmr && echo 1 || echo 0)" \
		-o "${out}/${base}.msi" /release/windows/Caribou.wxs)
	sign_windows_file "${out}/${base}.msi"
	verify_windows_file "${out}/${base}.msi"
	rm -rf "$stage"

	log "artifacts:"
	ls -1 "$out" >&2
}

# ---------------------------------------------------------------------------
host_main() {
	parse_common_args "$@"
	[[ ${#EXTRA_ARGS[@]} -eq 0 ]] || die "unknown option: ${EXTRA_ARGS[0]}"
	resolve_version
	load_env
	export_source
	stage_xmr_libs "windows-${ARCH}" libmonero_wallet2_api_c.dll
	ensure_image
	run_stage build
}

if [[ "${1:-}" == "--stage" ]]; then
	"stage_$2"
else
	host_main "$@"
fi
