#!/usr/bin/env bash
# Build, sign and package the Linux desktop release: a Debian package and a
# plain tarball, both amd64, both carrying the XMR library, plus a
# snapcraft.yaml that wraps the .deb for the Snap Store.
#
#   release/package-linux.sh v1.2.0 [--test-install]
#
# The build runs inside the caribou-builder image (ubuntu:22.04), which fixes
# the glibc and WebKitGTK floor at Ubuntu 22.04 / Debian 12. Linux "signing"
# is the GPG signature over SHA256SUMS that release.sh produces; nothing here
# signs the .deb itself.

source "$(dirname "$0")/common.sh"

DEB_PKG="caribou-wallet" # plain "caribou" is a dead GNOME package in the Debian archive
DEB_ARCH="amd64"
DEB_REV="1"

usage() {
	cat <<USAGE
usage: $(basename "$0") vX.Y.Z [options]
$(common_usage)
  --test-install     after packaging, install the .deb in a clean container of
                     the base it was built on and run \`${APP} --version\`
USAGE
}

# deb_shlibdeps <pkgdir> <binary>...: the Depends line dpkg-shlibdeps derives
# for the given binaries, with libraries inside <pkgdir> treated as part of
# the package (no dependency on them).
deb_shlibdeps() {
	local pkgdir="$1"; shift
	local tmp f
	tmp="$(mktemp -d)"
	mkdir -p "${tmp}/debian"
	printf 'Source: caribou\nSection: misc\nPriority: optional\nMaintainer: The Caribou Developers <briantstafford@gmail.com>\n\nPackage: %s\nArchitecture: %s\nDescription: Caribou\n' \
		"$DEB_PKG" "$DEB_ARCH" >"${tmp}/debian/control"
	local -a exes=()
	for f in "$@"; do exes+=(-e "$f"); done
	(cd "$tmp" && dpkg-shlibdeps -O "-S${pkgdir}" "${exes[@]}") | sed -n 's/^shlibs:Depends=//p'
	rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
stage_build() {
	local out=/work/linux stage=/work/linux/stage
	rm -rf "$out"
	mkdir -p "$stage"

	build_ui
	go_tags

	log "building ${APP} for linux/${DEB_ARCH}"
	CGO_ENABLED=1 GOOS=linux GOARCH="$DEB_ARCH" \
		go build -trimpath "${GO_TAGS[@]}" -ldflags "$(go_ldflags)" -o "${stage}/${APP}" "./wallet/cmd/${APP}"
	with_xmr && cp "wallet/asset/xmr/lib/linux-${DEB_ARCH}/libmonero_wallet2_api_c.so" "$stage/"

	# Tarball: binary plus the library beside it, which the $ORIGIN rpath finds.
	local tarname="${APP}-${VERSION}-linux-${DEB_ARCH}"
	mkdir -p "${out}/${tarname}"
	cp "${stage}"/* "${out}/${tarname}/"
	tar -C "$out" --owner=0 --group=0 --sort=name --mtime="@0" -czf "${out}/${tarname}.tar.gz" "$tarname"
	rm -rf "${out:?}/${tarname}"

	# Debian package. The tree under $deb mirrors the installed filesystem.
	# The file name carries VERSION, not VERSION_DEB: GitHub replaces "~" with
	# "." in uploaded asset names, which would break the SHA256SUMS line and
	# the download link. dpkg takes the version from DEBIAN/control, where
	# "~" stays and keeps a release candidate ordered before the release.
	local debname="${DEB_PKG}_${VERSION}-${DEB_REV}_${DEB_ARCH}"
	local deb="${out}/${debname}"
	install -Dm755 "${stage}/${APP}" "${deb}/usr/bin/${APP}"
	with_xmr && install -Dm644 "${stage}/libmonero_wallet2_api_c.so" "${deb}/usr/lib/caribou/libmonero_wallet2_api_c.so"
	install -Dm644 /release/linux/org.bisoncraft.caribou.desktop "${deb}/usr/share/applications/org.bisoncraft.caribou.desktop"
	install -Dm644 /release/linux/org.bisoncraft.caribou.metainfo.xml "${deb}/usr/share/metainfo/org.bisoncraft.caribou.metainfo.xml"
	local n
	for n in 16 24 32 48 64 128 256 512; do
		install -Dm644 "/release/linux/icons/caribou-${n}.png" "${deb}/usr/share/icons/hicolor/${n}x${n}/apps/caribou.png"
	done
	install -dm755 "${deb}/DEBIAN"
	# Runtime dependencies come from the binaries themselves, the way Debian's
	# own tooling derives them: the glibc, libstdc++, GTK and WebKitGTK floors
	# follow what caribou-desktop and the XMR library reference, so the
	# package's floor is always the true one.
	local depends size
	if with_xmr; then
		depends="$(deb_shlibdeps "$deb" "${deb}/usr/bin/${APP}" "${deb}/usr/lib/caribou/libmonero_wallet2_api_c.so")"
	else
		depends="$(deb_shlibdeps "$deb" "${deb}/usr/bin/${APP}")"
	fi
	[[ -n "$depends" ]] || die "dpkg-shlibdeps produced no dependency list"
	log "Depends: ${depends}"
	size="$(du -sk --exclude=DEBIAN "$deb" | cut -f1)"
	sed -e "s|@VERSION@|${VERSION_DEB}-${DEB_REV}|" -e "s|@ARCH@|${DEB_ARCH}|" \
		-e "s|@SIZE@|${size}|" -e "s|@HOMEPAGE@|${HOMEPAGE}|" -e "s|@DEPENDS@|${depends}|" \
		/release/linux/control.in >"${deb}/DEBIAN/control"
	log "building ${debname}.deb"
	dpkg-deb --build --root-owner-group "$deb" "${out}/${debname}.deb" >/dev/null
	rm -rf "$deb" "$stage"

	# snapcraft.yaml beside the .deb it wraps: `cd out/<version>/linux &&
	# snapcraft` builds the snap locally (needs snapcraft with LXD), and the
	# build-snap workflow points snapcore/action-build at the same directory.
	mkdir -p "${out}/snap"
	sed -e "s|@VERSION@|${VERSION}|" -e "s|@DEB@|${debname}.deb|" -e "s|@SNAP_BASE@|core${BASE%%.*}|" \
		/release/linux/snapcraft.yaml.in >"${out}/snap/snapcraft.yaml"

	log "artifacts:"
	ls -1 "$out" >&2
}

# ---------------------------------------------------------------------------
host_main() {
	parse_common_args "$@"
	local test_install=0
	for a in "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}"; do
		case "$a" in
		--test-install) test_install=1 ;;
		*) die "unknown option: $a" ;;
		esac
	done
	resolve_version
	load_env
	export_source
	stage_xmr_libs "linux-${DEB_ARCH}" libmonero_wallet2_api_c.so
	if with_xmr; then
		select_image "$(linux_base_for_lib "${SRC_DIR}/wallet/asset/xmr/lib/linux-${DEB_ARCH}/libmonero_wallet2_api_c.so")"
	else
		select_image 22.04
	fi
	ensure_image
	run_stage build

	if [[ "$test_install" == 1 ]]; then
		local deb
		deb="$(ls "${OUT_DIR}"/linux/*.deb)"
		log "test-installing $(basename "$deb") in ubuntu:${BASE}"
		docker run --rm -v "${deb}:/tmp/pkg.deb:ro" "ubuntu:${BASE}" bash -euc '
			export DEBIAN_FRONTEND=noninteractive
			apt-get update -qq >/dev/null
			apt-get install -y -qq /tmp/pkg.deb >/dev/null
			dpkg -L '"$DEB_PKG"'
			'"$APP"' --version'
	fi
}

if [[ "${1:-}" == "--stage" ]]; then
	"stage_$2"
else
	host_main "$@"
fi
