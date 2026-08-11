#!/usr/bin/env bash

# A good getting-started guide for Debian packaging can be found at
# https://www.internalpointers.com/post/build-binary-deb-package-practical-guide

# turn this on for debugging, keep noise low for prod builds
# set -ex

# Sets VER from electron/package.json, among other things. VER is deliberately
# not overridden here: DEB_NAME (the .deb filename) is computed in common.sh, so
# a later override would put a different version in the control file than in the
# filename.
source linux/common.sh

# Parse flags.
TAGS=""
for arg in "$@"; do
  case $arg in
    --xmr) TAGS="${TAGS:+${TAGS} }xmr" ;;
    *) echo "Unknown option: $arg"; exit 1 ;;
  esac
done

# A directory containing metadata files
SRC_DIR="./src"

# The DEB_DIR represents the root directory in our target system. The directory
# structure created under DEB_DIR will be duplicated on the target system
# during installation.
DEB_DIR="${BUILD_DIR}/${DEB_NAME}"

# Magic name for a directory containing a specially formatted control file
# (is it INI?) and special scripts to run during installation.
CONTROL_DIR="${DEB_DIR}/DEBIAN"

# posinst/postrm are special filenames under the DEBIAN directory. The scripts
# will be run after installation/uninstall.
POSTINST_PATH="${CONTROL_DIR}/postinst"
POSTRM_PATH="${CONTROL_DIR}/postrm"

# The caribou binary.
BIN_TARGETDIR="/usr/bin"
BIN_BUILDDIR="${DEB_DIR}${BIN_TARGETDIR}"
BIN_FILENAME="${APP}"
BIN_BUILDPATH="${BIN_BUILDDIR}/${BIN_FILENAME}"

# The Desktop Entry is a format for "installing" programs on Linux, creating
# an entry in the main menu.
# https://specifications.freedesktop.org/desktop-entry-spec/latest/
DOT_DESKTOP_TARGETDIR="/usr/share/applications"
DOT_DESKTOP_BUILDDIR="${DEB_DIR}${DOT_DESKTOP_TARGETDIR}"
DOT_DESKTOP_FILENAME="org.bisoncraft.caribou.desktop"
DOT_DESKTOP_BUILDPATH="${DOT_DESKTOP_BUILDDIR}/${DOT_DESKTOP_FILENAME}"

# Prepare the directory structure.
rm -fr "${BUILD_DIR}"
mkdir -p -m 0755 "${CONTROL_DIR}"
mkdir -p "${DOT_DESKTOP_BUILDDIR}"

# Build site bundle. newui is a member of the npm workspace rooted at
# wallet/appserver and webpack is declared at that root, so install from the
# root and build the member (newui has no lockfile of its own).
CWD=$(pwd)
cd ../../appserver
npm ci
npm run build --workspace=newui
cd $CWD

# Build caribou
LDFLAGS="-s -w -X github.com/bisoncraft/caribou/wallet/app.Version=${VER}${META:++${META}}"
GOOS=linux GOARCH=${ARCH} go build ${TAGS:+-tags ${TAGS}} -o "${BIN_BUILDPATH}" -ldflags "$LDFLAGS"
chmod 755 "${BIN_BUILDPATH}"

# Bundle XMR shared library if building with XMR support.
if [[ " ${TAGS} " == *" xmr "* || "${TAGS}" == "xmr" ]]; then
  XMR_LIB_DIR="${DEB_DIR}/usr/lib/caribou"
  mkdir -p "${XMR_LIB_DIR}"
  cp "../../asset/xmr/lib/linux-${ARCH}/libmonero_wallet2_api_c.so" "${XMR_LIB_DIR}/"
fi

# Full control file specification -
# https://www.debian.org/doc/debian-policy/ch-controlfields.html
cat > "${CONTROL_DIR}/control" <<EOF
Package: ${PKG}
Version: ${VER_DEB}
Architecture: ${ARCH}
Maintainer: The Caribou Developers <briantstafford@gmail.com>
Depends: libgtk-3-0, libwebkit2gtk-4.1-0
Description: A non-custodial, multi-chain wallet
EOF

# Copy icons
# This will be the icon shown for the program in the taskbar. I know that both
# PNG and SVG will work. If it's a bitmap, should probably be >= 128 x 128 px.
ICON_TARGETDIR="/usr/share/icons/hicolor"
ICON_BUILDDIR="${DEB_DIR}${ICON_TARGETDIR}"
install -Dm644 -t "${ICON_BUILDDIR}/scalable/apps" "${SRC_DIR}/caribou.svg"
install -Dm644 -t "${ICON_BUILDDIR}/128x128/apps" "${SRC_DIR}/caribou.png"

# AppStream metadata
# https://wiki.debian.org/AppStream
install -Dm644 -t "${DEB_DIR}/usr/share/metainfo" "${SRC_DIR}/org.bisoncraft.caribou.metainfo.xml"

# Update the desktop icons, refresh the "start" menu.
cat > "${POSTINST_PATH}" <<EOF
update-desktop-database
EOF
chmod 775 "${POSTINST_PATH}"

# Example file:
# https://specifications.freedesktop.org/desktop-entry-spec/latest/apa.html
cat > "${DOT_DESKTOP_BUILDPATH}" <<EOF
[Desktop Entry]
Version=1.5
Name=Caribou
Comment=Non-custodial, multi-chain wallet
Exec=${BIN_FILENAME}
Icon=${APP}
Terminal=false
Type=Application
Categories=Office;Finance;
EOF
chmod 644 "${DOT_DESKTOP_BUILDPATH}"

# Build the installation archive (.deb file).
dpkg-deb --build --root-owner-group "${DEB_DIR}"
