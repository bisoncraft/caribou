# Build Instructions for caribou-desktop

caribou-desktop is a cross-platform desktop application for Caribou. The instructions below are for building the client from source. If you are looking for pre-built binaries, see the [releases page](https://github.com/bisoncraft/caribou/releases).

**Note**: The instructions below are to be run in your CLI from the `caribou-desktop` directory.

## Debian

### Install build dependencies

```bash
sudo apt-get install libgtk-3-dev libwebkit2gtk-4.1-dev build-essential
```

### Run build

```bash
./linux/pkg-debian.sh
```

The deb archive will be located in **./build**.

## Snap

Snap is a package standard that can be installed on many Linux distributions from the Snap Store, and via software centers in e.g. Fedora, Ubuntu.

### Preparing the build environment

```bash
sudo apt-get install snapd
sudo snap install --classic snapcraft
sudo adduser $USER lxd
newgrp lxd
lxd init --auto
sudo ufw disable # if ufw is installed on the host, this is neccessary for lxd to have network access

```

### Running the build

 1. Build the Debian package
 2. `./linux/pkg-snap.sh`

### Publishing the snap

The snap can be uploaded to the Snap Store using `./linux/publish-snap.sh`.  This requires [Snapcraft developer account credentials](https://snapcraft.io/docs/releasing-your-app).  After this is completed, the package can be installed on any system running `snap` by running `snap install caribou`.  The app will be available on the [Snap Store](https://snapcraft.io/store/caribou).

## Windows

See [windows/README.md](windows/README.md) for full instructions on setting up the build environment and building the Windows binary and MSI installer.

## macOS (darwin)

macOS builds use Electron.

> **Monero (XMR) builds need the native library first.** Any `--xmr` build links
> `wallet/asset/xmr/lib/darwin-{amd64,arm64}/libmonero_wallet2_api_c.dylib`, which
> is not committed. Build it once (native macOS, reusing a local Xcode xip) with
> `wallet/asset/xmr/build-darwin.sh` — see
> [`wallet/asset/xmr/lib/README.md`](../../asset/xmr/lib/README.md). Non-XMR
> builds don't need it.

### Dev/test build

```bash
cd electron
npm ci            # installs electron-forge and runs prepare.sh (builds the web bundle + resources)
npm run make
```

For Monero (XMR) support, re-run prepare with `--xmr` before `make`:

```bash
cd electron
npm ci
./prepare.sh --xmr
npm run make
```

`npm run make` produces a `.zip` of the app in `electron/out/make/` (the
signed, notarized `.dmg` is produced only by the release `pkg.sh` below, via
`hdiutil` — Electron's dmg maker no longer builds on current Node).

### Release build

Release builds are signed with Developer ID and notarized, which requires:

- A **Developer ID Application** certificate in your login keychain — create it at
  [developer.apple.com](https://developer.apple.com/account/resources/certificates)
  under your organization team (*not* "Apple Distribution", which is for the App
  Store). Verify with
  `security find-identity -v -p codesigning | grep "Developer ID Application"`.
- An **app-specific password** for notarization (appleid.apple.com → Sign-In &
  Security → App-Specific Passwords).

Set the credentials (`read -s` keeps the password out of your shell history):

```bash
export APPLE_ID="your-apple-id@example.com"
export APPLE_TEAM_ID="your-team-id"
read -rs -p "App-specific password: " APPLE_PASSWORD && export APPLE_PASSWORD && echo
```

If `security find-identity` lists more than one "Developer ID Application"
certificate (common once one has expired), pin the one to sign with — the name
is only a prefix match, so codesign would otherwise fail as ambiguous at the end
of the build:

```bash
export APPLE_IDENTITY="<SHA-1 hash from security find-identity>"
```

Then run the packaging script (add `--xmr` to include Monero; the script installs
its own dependencies and does the signing, notarization, and stapling):

```bash
./electron/pkg.sh --xmr
```

The signed, notarized `.dmg` installers will be in `installers/`.

`pkg.sh` installs the electron deps with `--ignore-scripts`, which leaves
`node_modules/electron/dist` empty. That doesn't affect packaging, but a later
`npm start` in `electron/` will report "Electron failed to install correctly"
until you re-run a plain `npm ci` there.

#### Handing signing to a teammate

There are two separate credentials, and the certificate is the one people get
wrong: the `.cer` downloaded from the developer portal is only the *public*
certificate and **cannot sign** — the private key lives in the keychain of the
Mac that generated it. A teammate has two ways to get a working setup:

- **Regenerate on their own account (preferred).** If they're an **Admin** or
  the **Account Holder** on the *same* Apple Developer team (a plain
  Member/Developer can't create Developer ID certs, and the team has a small cap
  on how many exist — revoke old ones to free a slot), they create their own
  "Developer ID Application" certificate and their own app-specific password
  under their own Apple ID. No secret changes hands. `APPLE_TEAM_ID` stays the
  same; only `APPLE_ID`, `APPLE_PASSWORD`, and the individual certificate differ.
- **Share your identity.** Export a **`.p12`** from Keychain Access (right-click
  the "Developer ID Application" identity → Export → set an export password) so
  it bundles the certificate *with* its private key, and have them import it into
  their login keychain. Treat the `.p12` as a sensitive secret — it is the org's
  code-signing key; transfer it encrypted (shared password manager), never over
  Slack/email in the clear. They can reuse your `APPLE_ID` + app-specific
  password, or use their own under the same team.

**Updates keep working across signers.** There is no in-app auto-updater on
macOS — an update is just a newly downloaded, independently notarized `.dmg`.
What makes macOS treat a new build as "the same app" (preserving the wallet's
data, keychain items, and granted permissions) is the **Team ID + bundle ID**
(`com.bisoncraft.caribou.desktop`), *not* which individual's certificate signed
it. So any teammate signing under the same team is fine. The one thing to avoid
is signing under a **different Apple account / Team ID**, which macOS treats as a
different app (permission re-prompts, and loss of any keychain items the previous
version created).
