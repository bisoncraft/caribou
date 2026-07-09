const { FusesPlugin } = require('@electron-forge/plugin-fuses');
const { FuseV1Options, FuseVersion } = require('@electron/fuses');

const appleCredentials = process.env.APPLE_ID && process.env.APPLE_PASSWORD && process.env.APPLE_TEAM_ID;

module.exports = {
  packagerConfig: {
    name: 'Caribou',
    appBundleId: 'com.caribou.desktop',
    extraResource: ['../resources/'+require('./utils.js').getPlatform()],
    icon: '../src/caribou.icns',
    appVersion: require('./package.json').version,
    executableName: 'Caribou',
    asar: true,
    junk: true,
    platforms: ['darwin', 'win32', 'linux'],
    win32metadata: {
      CompanyName: 'The Decred Developers',
      FileDescription: 'A multi-coin wallet with DEX in-built.',
      OriginalFilename: 'Caribou.exe',
      ProductName: 'Caribou',
      InternalName: 'Caribou'
    },
    ...(appleCredentials ? {
      osxSign: {},
      osxNotarize: {
        appleId: process.env.APPLE_ID,
        appleIdPassword: process.env.APPLE_PASSWORD,
        teamId: process.env.APPLE_TEAM_ID,
      },
    } : {}),
    ignore: ['./node_modules', './resources', './installers'],
  },
  rebuildConfig: {},
  makers: [
    {
      name: '@electron-forge/maker-squirrel',
      config: {
        name: 'Caribou',
        authors: 'The Decred Developers',
        description: 'A multi-coin wallet with DEX in-built.',
        setupIcon: '../src/favicon-32.ico',
        setupExe: 'Caribou.exe',
        noMsi: true,
      },
    },
    {
      name: '@electron-forge/maker-dmg',
      config: {
        format: 'ULFO',
        name: 'Caribou',
        icon: '../src/caribou.icns',
        overwrite: true,
      }
    },
    {
      name: '@electron-forge/maker-deb',
      config: {
        options: {
          maintainer: 'The Decred Developers',
          homepage: 'https://bisonwallet.com',
          icon: '../src/caribou.png',
          categories: ['Office', 'Finance'],
          description: 'A multi-coin wallet with DEX in-built.',
        },
      },
    },
    {
      name: '@electron-forge/maker-rpm',
      config: {
        options: {
          maintainer: 'The Decred Developers',
          summary: 'A multi-coin wallet with DEX in-built.',
          icon: '../src/caribou.png',
          categories: ['Office', 'Finance'],
        },
      },
    }
    // {
    //   name: '@electron-forge/maker-zip',
    //   platforms: ['darwin', 'win32', 'linux'],
    // }
  ],
  plugins: [
    {
      name: '@electron-forge/plugin-auto-unpack-natives',
      config: {},
    },
    // Fuses are used to enable/disable various Electron functionality
    // at package time, before code signing the application
    new FusesPlugin({
      version: FuseVersion.V1,
      [FuseV1Options.RunAsNode]: false,
      [FuseV1Options.EnableCookieEncryption]: true,
      [FuseV1Options.EnableNodeOptionsEnvironmentVariable]: false,
      [FuseV1Options.EnableNodeCliInspectArguments]: false,
      [FuseV1Options.EnableEmbeddedAsarIntegrityValidation]: true,
      [FuseV1Options.OnlyLoadAppFromAsar]: true,
    }),
  ],
};
