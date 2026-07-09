
# This file defines common variables to be source'd by the various build scripts
# in this directory.

# pick up the release tag from git
VER=$(git describe --tags --abbrev=0 --always | sed -e 's/^v//')
META= # "release"
REV="0"

APP="caribou"

# The Debian package name. Deliberately not plain "caribou": the Ubuntu/Debian
# archives already carry a (dead) GNOME package by that name.
PKG="caribou-wallet"
ARCH="amd64"

# The build directory will be deleted at the beginning of every build. The
# directory is .gitignore'd.
BUILD_DIR="./build"

# DEB_NAME follows the prescribed format for debian packaging.
DEB_NAME="${PKG}_${VER}-${REV}_${ARCH}"

