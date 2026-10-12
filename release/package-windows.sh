#!/usr/bin/env bash
# Build, sign and package the Windows desktop release: a signed setup.exe
# installer and a signed portable zip, both amd64, both carrying the XMR
# library.
#
#   release/package-windows.sh v1.2.0
#
# The .exe is cross-compiled with MinGW inside the caribou-builder image, the
# installer is built there with NSIS from release/windows/caribou.nsi, and
# everything is Authenticode-signed with jsign (Azure Artifact Signing, or a
# local PKCS#12 for testing). Order matters: the .exe and DLLs are signed
# before the installer is built so it carries signed files; makensis then
# signs the uninstaller it generates and finally the installer itself,
# through `--stage sign`.
#
# Nothing here runs the installer, so an install test on a Windows machine is
# part of every release.

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
	with_xmr && cp "wallet/asset/xmr/lib/windows-${ARCH}/libmonero_wallet2_api_c.dll" "$stage/"

	# WebView2Loader.dll is Microsoft-signed already; the others are ours.
	sign_windows_file "${stage}/${APP}.exe"
	verify_windows_file "${stage}/${APP}.exe"
	if with_xmr; then
		sign_windows_file "${stage}/libmonero_wallet2_api_c.dll"
		verify_windows_file "${stage}/libmonero_wallet2_api_c.dll"
	fi

	local base="${APP}-${VERSION}-windows-${ARCH}"
	log "building ${base}.zip"
	(cd "$stage" && zip -q -X -r "${out}/${base}.zip" .)

	log "building ${base}-setup.exe"
	local nsis=(-V2 -NOCD -DVERSION="$VERSION" -DVERSION_NUM="$VERSION_NUM"
		-DOUTFILE="${out}/${base}-setup.exe" -DICON="${PWD}/wallet/cmd/${APP}/src/caribou.ico")
	with_xmr && nsis+=(-DXMR)
	[[ "$NO_SIGN" == 1 ]] || nsis+=(-DSIGN_CMD="bash /release/package-windows.sh --stage sign")

	# makensis packs its own plug-in DLLs into the installer, which unpacks
	# them to a temporary directory and loads them while it runs. Sign copies
	# of the ones caribou.nsi pulls in (the Modern UI pages use both) and put
	# them first on the plug-in path, where they win over the stock ones.
	# audit_installer fails the build if an unsigned one is ever packed.
	local plugins="${out}/nsis-plugins" p
	mkdir -p "$plugins"
	for p in nsDialogs System; do
		cp "/usr/share/nsis/Plugins/x86-unicode/${p}.dll" "$plugins/"
		sign_windows_file "${plugins}/${p}.dll"
		verify_windows_file "${plugins}/${p}.dll"
	done
	nsis+=(-X"!addplugindir /x86-unicode ${plugins}")

	(cd "$stage" && makensis "${nsis[@]}" /release/windows/caribou.nsi)
	audit_installer "${out}/${base}-setup.exe"
	rm -rf "$stage" "$plugins"

	log "artifacts:"
	ls -1 "$out" >&2
}

# audit_installer <setup.exe>: check that every PE file the installer carries
# is signed. 7-Zip unpacks the payload and the plug-ins; the uninstaller is
# written at build time and was signed and verified by SIGN_CMD already.
# WebView2Loader.dll carries Microsoft's signature, not ours, so it must be
# byte-identical to the checksum-pinned copy in the image instead.
audit_installer() {
	[[ "$NO_SIGN" == 1 || -z "${ACS_ACCOUNT:-}" ]] && return 0
	local setup="$1" dir f n=0
	dir="$(mktemp -d)"
	7z x -y -o"$dir" "$setup" >/dev/null
	while IFS= read -r -d '' f; do
		[[ "$(head -c2 "$f")" == MZ ]] || continue
		if [[ "$(basename "$f")" == WebView2Loader.dll ]]; then
			cmp -s "$f" /opt/webview2/runtimes/win-x64/native/WebView2Loader.dll ||
				die "installer audit: WebView2Loader.dll is not the pinned copy"
		else
			verify_windows_file "$f"
		fi
		n=$((n + 1))
	done < <(find "$dir" -type f -print0)
	rm -rf "$dir"
	log "installer audit: all ${n} PE files in $(basename "$setup") are signed"
}

# stage_sign <file>: sign and verify one file. makensis runs this on the
# uninstaller it generates and on the finished installer (SIGN_CMD in
# caribou.nsi); the signing credentials reach it through the environment.
stage_sign() {
	sign_windows_file "$1"
	verify_windows_file "$1"
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
	"stage_$2" "${@:3}"
else
	host_main "$@"
fi
