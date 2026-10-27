# Release Checklist

Releases are cut from a release branch. `master` carries the next version
with a `-pre` suffix. When a minor version is ready to stabilize, a
`release-vX.Y` branch is forked from `master`, and every `vX.Y.Z[-rcN]` tag
for that minor version is made on the branch. Fixes land on `master` first
and are cherry-picked to the branch. The release branch is never merged back
into `master`, because it carries the built web bundle that `master`
ignores.

Desktop installers and command-line archives are built, signed and packaged
from a Linux host by the scripts in `release/`; see [release/README.md](../release/README.md) for the
tooling, credentials and per-platform notes.

## 1. Before forking the release branch

Everything in this section lands on `master`, so the release branch and
every later release inherit it.

### Endpoint lists

Built-in endpoint lists decay between releases. Drop dead or degraded
entries, re-rank, add replacements, and update the verification dates in the
list comments. Assets commented out of `wallet/asset/importall/` do not ship,
so their lists can wait until they are registered again.

RPC providers for account-based assets:

| Asset | List | Check |
| --- | --- | --- |
| ETH | `defaultProviders` in `newWallet`, `wallet/asset/eth/eth.go` | `TestFreeServers`, tag `rpclive` |
| Polygon | `defaultProviders` in `Driver.Open`, `wallet/asset/polygon/polygon.go` | `TestFreeServers`, tag `rpclive` |
| Base | `defaultProviders` in `Driver.Open`, `wallet/asset/base/base.go` | `TestFreeServers`, tag `rpclive` |
| BNB | `defaultProviders` in `Driver.Open`, `wallet/asset/bnb/bnb.go` | manual (`eth_chainId`, `eth_blockNumber`) |
| HyperEVM | `defaultProviders` in `Driver.Open`, `wallet/asset/hyperevm/hyperevm.go` | manual (`eth_chainId`, `eth_blockNumber`) |
| Hyperliquid | `DefaultAPIEndpoints`, `DefaultWSEndpoints` in `wallet/asset/hyperliquid/params.go` | manual |
| NEAR | `DefaultRPCEndpoints` in `wallet/asset/near/params.go` | manual (`status`, `block`, `query`) |
| SOL | `DefaultRPCEndpoints`, `DefaultWSEndpoints` in `wallet/asset/sol/params.go` | manual (`getHealth`) |
| XRP | `DefaultRPCEndpoints`, `DefaultWSEndpoints` in `wallet/asset/xrp/params.go` | manual (`server_info`) |
| XMR | `defaultMainnetDaemon` in `wallet/asset/xmr/xmr.go` | manual (`get_info`) |

The `TestFreeServers` tests probe the list in their own test file, not the
default list, so copy the default list into the test before running it:

```bash
go test -tags rpclive -run TestFreeServers -v ./wallet/asset/eth/ ./wallet/asset/polygon/ ./wallet/asset/base/
```

Mobile builds use only the first two EVM providers (`maxMobileProviders` in
`wallet/asset/eth/providers_mobile.go`), so the top two entries of each EVM
list need to be the most reliable.

UTXO assets:

- [ ] Electrum servers: `btcMainnetElectrumServers`,
      `ltcMainnetElectrumServers` and `dashMainnetElectrumServers` in
      `wallet/utxowallet/assets/`.
- [ ] Fee-rate sources:
      `go test -tags live -run TestLiveFeeSources ./wallet/asset/btc/`.

### Other external data

- [ ] NEAR Intents: `chainBipIDs` in `wallet/intents/catalog.go` maps every
      1Click chain the release supports. Compare it with the live
      `/v0/tokens` catalog, and check the per-chain quote deadlines in
      `intentsDeadlines` (`wallet/core/near-intents.go`) for any newly
      mapped UTXO chain.
- [ ] UTXO checkpoints: append a mainnet checkpoint to
      `wallet/utxowallet/assets/btc.go`, `ltc.go` and `dash.go` for every
      interval mined since the last entry. Each spacing targets about three
      months of blocks, aligned to the chain's retarget period where it has
      one:

  | Chain | Spacing | Basis |
  | --- | --- | --- |
  | BTC | 14112 blocks | 7 retarget periods of 2016 |
  | LTC | 52416 blocks | 26 retarget periods of 2016 |
  | Dash | 52500 blocks | no retarget period (retargets every block) |

  Cross-check each hash against two explorers, then sync through the
  new entries with
  `go test -tags livenet -run TestAnchorHeaderSync ./wallet/utxowallet/electrum/`.
  Never remove or move an entry: the first one is the launch anchor
  the electrum header store starts from, and `DefaultWalletBirthday`
  stays pinned just above it. Testnets keep their single anchor.

### Documentation

- [ ] README: roadmap reflects what shipped and what is next; build
      commands, toolchain requirements (the `go` line in `go.mod`, Node in
      `release/Dockerfile`) and the design notes match the code; links in
      the Documentation section resolve.

### Translations

Every supported language is complete when the branch is forked. The
supported languages are the entries of `Locales` in
`wallet/appserver/desktop/locales/locales.go`. Translation takes lead time, so
it starts once the release's UI strings stop changing on `master`.

The desktop and mobile bundles translate through `t()`, `tvar()`, `tdom()`
and `prep()` (`wallet/appserver/shared/src/intl.ts`). Those look keys up in
the map that `POST /api/locale` returns, and that map holds only the keys
defined in `EnUS` (`wallet/appserver/desktop/locales/en-us.go`). A key used in
code but missing from `EnUS` displays its own text in every language, and
`translationsreport` does not see it.

- [ ] Every key passed to those functions in `desktop/src`, `mobile/src` and
      `shared/src` has an `EnUS` entry. That includes keys reached through
      label maps: theme, sidebar, activity filter and transaction type
      labels.
- [ ] Every language translates every `EnUS` key and every core
      notification. `go run ./wallet/cmd/translationsreport` writes
      `worksheets/<lang>.json` with each language's missing entries.
      Changing an English string without changing its key needs a `Version`
      bump on the `EnUS` entry, which marks the existing translations as
      missing. Once the `translation` fields are filled in,
      `go run ./wallet/cmd/translationsreport -format worksheets/<lang>.json`
      prints Go map entries:
  - `js` entries go in `wallet/appserver/desktop/locales/<lang>.go`.
  - `notifications` entries go in `wallet/core/locale_ntfn.go`.

## 2. Fork the release branch

- [ ] Fork from the tip of upstream `master`:

  ```bash
  git fetch upstream
  git push upstream upstream/master:refs/heads/release-vX.Y
  ```

- [ ] On `master`, bump to the next minor with a `-pre` suffix (for example
      `X.Y+1.0-pre`) in `wallet/app/version.go` (see
      [Version locations](#version-locations)).

## 3. Before tagging

Repeat for every tag, release candidates included, on `release-vX.Y`.

- [ ] Every fix intended for the release is cherry-picked from `master`, and
      CI is green on the branch.
- [ ] Set every location in [Version locations](#version-locations) to the
      tag's version.
- [ ] XMR libraries: the prebuilt `libmonero_wallet2_api_c` libraries are
      hosted on their own GitHub release, tagged `xmr-libs-<monero_c
      version>-caribou.<n>` and separate from the app release.
      `wallet/asset/xmr/lib/SHA256SUMS` names that release on its
      `# Release:` line and pins every library's hash. If the monero_c pin
      changed, rebuild the libraries, then run
      `wallet/asset/xmr/publish-libs.sh --publish`, which creates the
      release, uploads the assets and rewrites `SHA256SUMS`; commit the
      result. The release scripts never download the libraries (see
      [Build](#4-build)), so the installers do not depend on this release.
      The snap workflow and source builds do: they download from the named
      release on `bisoncraft/caribou` without credentials, so it has to be
      published, not a draft, before the snap is built. A request for
      `https://github.com/bisoncraft/caribou/releases/download/<release>/libmonero_wallet2_api_c-linux-amd64.tar.xz`
      returns the asset once it is.
- [ ] Translations: any string added or changed by a cherry-pick is
      translated in every language (see [Translations](#translations)).
- [ ] README: changes made on `master` since the fork are cherry-picked.
- [ ] Website: changes for this release are prepared in `caribou-site`
      (see [Website](#website)), ready to deploy once the release is public.
- [ ] Release notes are drafted in a file for `release.sh --notes <file>`.
      Without `--notes`, GitHub generates a list of the pull requests since
      the previous release, which needs a human-written summary on top; with
      no previous release, that list is every pull request in the
      repository.
- [ ] Web assets, as the last commit before the tag. `dist/` is ignored, so
      the files are force-added; build with the Node version pinned in
      `release/Dockerfile` so the committed bundle matches the shipped one:

  ```bash
  cd wallet/appserver
  npm ci && npm run build
  git add -f desktop/dist mobile/dist
  git commit -m "release: build web assets for vX.Y.Z"
  ```

  The release scripts rebuild the bundle from source, so the committed copy
  serves `go build` and `go install` from the tag and the mobile bind
  scripts. Any later cherry-pick that touches the UI means rebuilding and
  recommitting before the next tag.
- [ ] Tag the release commit and push the tag:

  ```bash
  git tag -s vX.Y.Z -m "vX.Y.Z"
  git push upstream vX.Y.Z
  ```

## 4. Build

- [ ] XMR libraries on the build host: the release scripts copy them from
      `wallet/asset/xmr/lib/<platform>/` (or `--xmr-libs <dir>`) and check
      them against the `SHA256SUMS` at the tag; they never download. Where
      `lib/` is already populated, `cd wallet/asset/xmr/lib && sha256sum -c
      SHA256SUMS` confirms it matches; otherwise
      `cd wallet/asset/xmr && ./fetch-libs.sh --all` downloads and verifies
      them. Every platform is needed, including `linux-arm64`, which only the
      command-line archives use.
- [ ] Builder image: the scripts build the Docker image only when it is
      missing or `--build-image` is passed. They do not notice a change to
      `release/Dockerfile`, so pass `--build-image` on the first build after
      one.
- [ ] Release signing key: `release.sh` signs `SHA256SUMS` with
      `GPG_KEY_ID` from `release/.env/env.sh`, or with the default secret
      key when that is blank. With neither, it warns and writes no
      `SHA256SUMS.asc`. The public key and its fingerprint are published
      in the repository and on the caribou-site download page
      (`templates/pages/download.tmpl`). `--no-sign` cannot skip only the
      manifest signature: it is forwarded to every package script and also
      disables Windows and macOS code signing.
- [ ] `release/release.sh vX.Y.Z --publish --notes <file>` builds every
      desktop platform and, through `release/package-cli.sh`, the
      command-line archives; writes one `SHA256SUMS` covering every file,
      and `SHA256SUMS.asc`; and opens a draft GitHub release with the
      artifacts attached. The command-line archives are `wallet/cmd/caribou`
      with XMR for linux/amd64 and linux/arm64 (`.tar.gz`), darwin/amd64 and
      darwin/arm64 (`.zip`, signed and notarized) and windows/amd64 (`.zip`,
      signed). A notarization ticket cannot be stapled to a zip, so
      Gatekeeper fetches it online when the binary first runs. After the
      upload, `release.sh` compares the asset names GitHub stored with
      `dist/` and stops on any difference, since GitHub renames some
      characters in asset names without documenting which. The names the
      scripts produce avoid the one known case, `~`: the `.deb` file name
      carries `1.2.0-rc1` while its control file keeps `1.2.0~rc1`.

## 5. Verify

- [ ] Linux: `release/package-linux.sh vX.Y.Z --test-install` installs the
      `.deb` in a clean container; launch the installed app on a desktop.
- [ ] Windows: run the `-setup.exe` on a Windows machine, confirm the
      signature in the file's Properties, launch, then uninstall.
- [ ] macOS: on an Apple Silicon Mac, open both `.dmg` files (the x86_64
      build under Rosetta), confirm Gatekeeper accepts them, launch; find an
      Intel Mac tester when one is available.
- [ ] Command-line archives: extract each one and run `caribou --version` on
      its platform, linux/arm64 on arm64 hardware. The Windows binary has no
      console, so run `caribou.exe --version | more` to see its output, or
      read the version from the Details tab of the file's properties. On a
      Mac, run the binary from a zip downloaded with a browser, so the
      quarantine attribute is set, which confirms Gatekeeper accepts the
      online notarization ticket.
- [ ] Verify `SHA256SUMS.asc` with the release signing key.

## 6. Publish

- [ ] Publish the draft GitHub release.
- [ ] Run the "Build and Publish Snap Package" workflow for the tag with
      `publish` enabled.
- [ ] Deploy the website changes (`caribou-site/docs/deploying.md`).

## Version locations

On the release branch, each location carries the version of the tag being
made; on `master`, the next minor with `-pre`.

| File | Field | Release-branch value |
| --- | --- | --- |
| `wallet/app/version.go` | `Version` | `X.Y.Z[-rcN]+release.local` |

The release scripts override `Version` at build time with the tag's
version, through the linker (`-X` in `go_ldflags`, `release/common.sh`). The
checked-in value is what `go build` and `go install` from the tag report.

The mobile apps are versioned separately and bumped when a mobile build is
released:

| File | Field | Note |
| --- | --- | --- |
| `wallet/android/app/build.gradle` | `versionName`, `versionCode` | `versionCode` must increase on every Play upload |
| `wallet/ios/project.yml` | `MARKETING_VERSION` | `CFBundleVersion` is set by `wallet/ios/release.sh` from the commit count |

Derived from the tag at build time, no edit needed:

- `release/darwin/electron/package.json` (`0.0.0`; `package.js` receives
  `--version` and `--build-version`).
- `release/linux/control.in` and `release/linux/snapcraft.yaml.in`
  (`@VERSION@`).
- `release/windows/caribou.nsi` (`VERSION_NUM`).
- `wallet/cmd/caribou/winres.json` and `wallet/cmd/caribou-desktop/winres.json`:
  `package-cli.sh` and `package-windows.sh` regenerate the resource objects
  with the tag's version (`windows_resources` in `release/common.sh`). The
  checked-in values and `rsrc_windows_*.syso` objects serve source builds
  only.
- `release/linux/org.bisoncraft.caribou.metainfo.xml` has no `<releases>`
  element; if one is added, it needs an entry per release.

Not tied to the app version:

- `UserAgentVersion` in `wallet/utxowallet/spv/neutrino.go`, advertised to
  P2P peers, and `userAgent` in `wallet/utxowallet/electrum/node.go`, sent
  in `server.version`.
- The `version` fields of the npm workspace `package.json` files, which are
  never published.

## Website

The site lives in the `caribou-site` repository and is deployed by hand
(`docs/deploying.md` there).

- [ ] Download page: `internal/content/download_cards.go` links each
      installer the release publishes (`.deb`, `-setup.exe`, `.dmg`) under
      its exact asset name. The page links installers only.
- [ ] Supported assets: `internal/content/holdings.go` matches the assets
      the release ships.
- [ ] Legal: if the release adds a third-party service (a swap venue, an
      on-ramp, a new data source), review the privacy policy
      (`content/legal/`) and terms (`templates/legal/`).
