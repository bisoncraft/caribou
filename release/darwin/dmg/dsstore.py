#!/usr/bin/env python3
# Write the Finder layout of the macOS dmg: a .DS_Store that opens the volume
# in a fixed-size icon view with no toolbar or sidebar, the app on the left,
# the Applications link on the right and a background image behind them.
#
#   dsstore.py --out <staging>/.DS_Store --volume Caribou --app Caribou.app \
#       --background .background/background.png
#
# Finder locates the background through an alias record stored in the view
# settings. macOS normally builds that record from the mounted volume; here it
# is assembled by hand with the volume name and the image path, which is what
# Finder falls back to when the catalog IDs in the record do not match (the
# Linux-built Bitcoin Core dmgs shipped the same construction for years).
# The volume name passed here must be the one genisoimage gives the image.
#
# The layout constants match release/darwin/dmg/background.png: a 660x400
# point window with 128 point icons centered at the two slots.

import argparse
import datetime
import os
import sys

from ds_store import DSStore
from mac_alias import (
    ALIAS_EJECTABLE_DISK,
    ALIAS_KIND_FILE,
    Alias,
    TargetInfo,
    VolumeInfo,
)

WINDOW_ORIGIN = (200, 160)
WINDOW_SIZE = (660, 400)
ICON_SIZE = 128
TEXT_SIZE = 14
APP_SLOT = (165, 175)
APPLICATIONS_SLOT = (495, 175)

# Arbitrary fixed dates: alias resolution does not depend on them, and a
# constant keeps the file reproducible.
EPOCH = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc)


def background_alias(volume, background):
    folder, name = os.path.split(background)
    return Alias(
        version=2,
        volume=VolumeInfo(
            name=volume,
            creation_date=EPOCH,
            fs_type=b"H+",
            disk_type=ALIAS_EJECTABLE_DISK,
            attribute_flags=0,
            fs_id=b"\0\0",
            posix_path="/Volumes/" + volume,
        ),
        target=TargetInfo(
            kind=ALIAS_KIND_FILE,
            filename=name,
            folder_cnid=0,
            cnid=0,
            creation_date=EPOCH,
            creator_code=b"\0\0\0\0",
            type_code=b"\0\0\0\0",
            folder_name=folder,
            carbon_path=volume + ":" + background.replace("/", ":"),
            posix_path="/" + background,
        ),
    )


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--volume", required=True)
    ap.add_argument("--app", required=True)
    ap.add_argument("--background", required=True, help="path inside the volume")
    args = ap.parse_args()

    (x, y), (w, h) = WINDOW_ORIGIN, WINDOW_SIZE
    if os.path.exists(args.out):
        os.remove(args.out)
    with DSStore.open(args.out, "w+") as ds:
        ds["."]["vSrn"] = ("long", 1)
        ds["."]["bwsp"] = {
            "WindowBounds": "{{%d, %d}, {%d, %d}}" % (x, y, w, h),
            "ShowStatusBar": False,
            "ShowToolbar": False,
            "ShowPathbar": False,
            "ShowSidebar": False,
            "ShowTabView": False,
            "ContainerShowSidebar": False,
            "SidebarWidth": 0,
            "PreviewPaneVisibility": False,
        }
        ds["."]["icvp"] = {
            "viewOptionsVersion": 1,
            "backgroundType": 2,
            "backgroundImageAlias": background_alias(args.volume, args.background).to_bytes(),
            "backgroundColorRed": 1.0,
            "backgroundColorGreen": 1.0,
            "backgroundColorBlue": 1.0,
            "iconSize": float(ICON_SIZE),
            "textSize": float(TEXT_SIZE),
            "labelOnBottom": True,
            "showIconPreview": True,
            "showItemInfo": False,
            "arrangeBy": "none",
            "gridOffsetX": 0.0,
            "gridOffsetY": 0.0,
            "gridSpacing": 100.0,
            "scrollPositionX": 0.0,
            "scrollPositionY": 0.0,
        }
        ds[args.app]["Iloc"] = APP_SLOT
        ds["Applications"]["Iloc"] = APPLICATIONS_SLOT

    # Read it back: a malformed store would surface only on a Mac.
    with DSStore.open(args.out, "r") as ds:
        entries = {(e.filename, e.code.decode()): e for e in ds}
    for key in ((".", "bwsp"), (".", "icvp"), (args.app, "Iloc"), ("Applications", "Iloc")):
        if key not in entries:
            sys.exit(f"{args.out}: missing {key}")
    alias = Alias.from_bytes(entries[(".", "icvp")].value["backgroundImageAlias"])
    if alias.volume.name != args.volume or alias.target.posix_path != "/" + args.background:
        sys.exit(f"{args.out}: background alias does not round-trip")
    print(f"wrote {args.out}: {len(entries)} entries, background {args.background} on volume {args.volume}")


if __name__ == "__main__":
    main()
