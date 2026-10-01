// Assemble Caribou.app for macOS with @electron/packager. Runs on Linux inside
// the builder image; signing and notarization happen afterwards with
// rcodesign (see package-darwin.sh), so no signing is configured here.
//
//   node package.js --arch x64|arm64 --version 1.2.0-rc1 --build-version 1.2.0 \
//     --bundle-id com.bisoncraft.caribou.desktop --icon <caribou.icns> \
//     --extra <dir> --out <dir>
//
// --extra is copied to Contents/Resources/mac and holds the Go `caribou`
// binary, the XMR dylib and the menu-bar icon, the code already signed.

const path = require("node:path");
const fs = require("node:fs");
const crypto = require("node:crypto");
const plist = require("plist");
const { packager } = require("@electron/packager");
const { flipFuses, FuseVersion, FuseV1Options } = require("@electron/fuses");

function arg(name) {
  const i = process.argv.indexOf(name);
  if (i < 0 || i + 1 >= process.argv.length) throw new Error(`missing ${name}`);
  return process.argv[i + 1];
}

const arch = arg("--arch");
const version = arg("--version");
const buildVersion = arg("--build-version");
const bundleId = arg("--bundle-id");
const icon = path.resolve(arg("--icon"));
const extra = path.resolve(arg("--extra"));
const out = path.resolve(arg("--out"));

// The packager treats a missing icon as "no icon for this platform" and
// quietly ships Electron's own, so check for it here.
if (!fs.existsSync(icon)) throw new Error(`icon not found: ${icon}`);

// Fuses are flipped after packaging, before signing: they lock down Node
// integration paths an attacker could use to run their own code inside the
// signed app.
async function lockDown(buildPath) {
  const exe = path.join(buildPath, "Caribou.app", "Contents", "MacOS", "Caribou");
  await flipFuses(exe, {
    version: FuseVersion.V1,
    resetAdHocDarwinSignature: false, // rcodesign re-signs everything
    [FuseV1Options.RunAsNode]: false,
    [FuseV1Options.EnableCookieEncryption]: true,
    [FuseV1Options.EnableNodeOptionsEnvironmentVariable]: false,
    [FuseV1Options.EnableNodeCliInspectArguments]: false,
    [FuseV1Options.EnableEmbeddedAsarIntegrityValidation]: true,
    [FuseV1Options.OnlyLoadAppFromAsar]: true,
    // The window loads the appserver over http; nothing needs file:// to
    // behave like a privileged origin.
    [FuseV1Options.GrantFileProtocolExtraPrivileges]: false,
  });
}

// Electron 41+ keeps a digest of the ElectronAsarIntegrity plist entry inside
// the Electron Framework binary, so a tampered Info.plist cannot loosen the
// asar check. @electron/packager writes it only when running on macOS, since
// patching the framework breaks its ad-hoc signature and packager cannot
// re-sign elsewhere. Here everything is re-signed with rcodesign afterwards,
// so the digest is written the same way packager does it: SHA256 over the
// sorted (key, algorithm, hash) tuples, stored as [used=1, version=1, digest]
// after each sentinel in the binary (one per Mach-O slice).
// See electron/shell/common/asar/integrity_digest.mm.
const SENTINEL = Buffer.from("AGbevlPCksUGKNL8TSn7wGmJEuJsXb2A");

function embedIntegrityDigest(appPath) {
  const info = plist.parse(fs.readFileSync(path.join(appPath, "Contents", "Info.plist"), "utf8"));
  const integrity = info.ElectronAsarIntegrity;
  if (!integrity || Object.keys(integrity).length === 0) {
    throw new Error("Info.plist has no ElectronAsarIntegrity entry");
  }
  const h = crypto.createHash("sha256");
  for (const key of Object.keys(integrity).sort()) {
    h.update(key);
    h.update(integrity[key].algorithm);
    h.update(integrity[key].hash);
  }
  const payload = Buffer.concat([Buffer.from([1, 1]), h.digest()]);

  const framework = path.join(
    appPath, "Contents", "Frameworks", "Electron Framework.framework", "Electron Framework",
  );
  const bin = fs.readFileSync(framework);
  const positions = [];
  for (let i = bin.indexOf(SENTINEL); i !== -1; i = bin.indexOf(SENTINEL, i + 1)) positions.push(i);
  if (positions.length === 0) throw new Error(`no integrity digest sentinel in ${framework}`);
  for (const pos of positions) {
    if (pos + SENTINEL.length + payload.length > bin.length) {
      throw new Error(`no room for the integrity digest after the sentinel at ${pos}`);
    }
    payload.copy(bin, pos + SENTINEL.length);
  }
  fs.writeFileSync(framework, bin);
  console.log(`wrote asar integrity digest at ${positions.length} sentinel(s)`);
}

packager({
  dir: __dirname,
  name: "Caribou",
  executableName: "Caribou",
  platform: "darwin",
  arch,
  out,
  overwrite: true,
  asar: true,
  prune: true,
  junk: true,
  appBundleId: bundleId,
  appVersion: version,
  buildVersion,
  appCopyright: "The Caribou Developers. Blue Oak Model License 1.0.0",
  appCategoryType: "public.app-category.finance",
  darwinDarkModeSupport: true,
  asarIntegrityDigest: false, // written by embedIntegrityDigest below instead
  icon,
  extraResource: [extra],
  // Only the runtime files belong in app.asar.
  ignore: [/^\/package\.js$/, /^\/package-lock\.json$/, /^\/node_modules($|\/)/],
  afterComplete: [
    async ({ buildPath }) => {
      await lockDown(buildPath);
      embedIntegrityDigest(path.join(buildPath, "Caribou.app"));
    },
  ],
})
  .then((paths) => {
    console.log(`packaged ${paths.join(", ")}`);
  })
  .catch((err) => {
    console.error(err);
    process.exit(1);
  });
