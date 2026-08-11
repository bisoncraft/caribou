
# This file defines common variables to be source'd by the various build scripts
# in this directory.

# electron/package.json is the single source of the desktop version (see
# electron/pkg.sh). Read with sed rather than `node -p`, since the snap scripts
# that source this file don't otherwise need node. "version" is the only such
# key in that file — dependency entries are keyed by package name.
COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VER=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
  "${COMMON_DIR}/../electron/package.json" | head -1)
if [[ -z "${VER}" ]]; then
  echo "could not read version from ${COMMON_DIR}/../electron/package.json" >&2
  exit 1
fi

# The Debian form of the version. dpkg compares end-of-string as lower than any
# character except '~', so a pre-release written as "1.1.0-rc2" sorts *newer*
# than the final "1.1.0" and `apt upgrade` would never move a user off the rc.
# "1.1.0~rc2" sorts correctly. Used for the .deb filename and the control file's
# Version: field; the snap's display version stays the plain semver form, since
# snapd upgrades by revision rather than by version comparison.
VER_DEB="${VER/-/\~}"
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
DEB_NAME="${PKG}_${VER_DEB}-${REV}_${ARCH}"

