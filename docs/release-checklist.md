# Release Checklist

Desktop installers are built, signed and packaged from a Linux host by the
scripts in `release/`; see [release/README.md](../release/README.md) for the
tooling, credentials and per-platform notes.

## Before tagging

- [ ] Ensure that the NEAR Intents asset list is up-to-date.
- [ ] Bump `Version` in `wallet/app/version.go` on master and merge it.
- [ ] Rebuild the `libmonero_wallet2_api_c` libraries if the monero_c pin
      changed, and publish them with `wallet/asset/xmr/publish-libs.sh` so
      `lib/SHA256SUMS` carries the release tag.

## Tag and build

- [ ] Tag the release commit `vX.Y.Z` (or `vX.Y.Z-rcN`) and push the tag.
- [ ] Stage the XMR libraries for every desktop platform:
      `cd wallet/asset/xmr && ./fetch-libs.sh --all`.
- [ ] `release/release.sh vX.Y.Z --publish` builds every platform, writes
      `SHA256SUMS` and `SHA256SUMS.asc`, and opens a draft GitHub release
      with the artifacts attached.

## Verify

- [ ] Linux: `release/package-linux.sh vX.Y.Z --test-install` installs the
      `.deb` in a clean container; launch the installed app on a desktop.
- [ ] Windows: run the `-setup.exe` on a Windows machine, confirm the
      signature in the file's Properties, launch, then uninstall.
- [ ] macOS: on an Apple Silicon Mac, open both `.dmg` files (the x86_64
      build under Rosetta), confirm Gatekeeper accepts them, launch; find an
      Intel Mac tester when one is available.
- [ ] Verify `SHA256SUMS.asc` with the release signing key.

## Publish

- [ ] Publish the draft GitHub release.
- [ ] Run the "Build and Publish Snap Package" workflow for the tag with
      `publish` enabled.
