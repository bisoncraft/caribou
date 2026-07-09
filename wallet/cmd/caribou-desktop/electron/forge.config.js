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
      CompanyName: 'The Caribou Developers',
      FileDescription: 'A non-custodial, multi-chain wallet.',
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
        authors: 'The Caribou Developers',
        description: 'A non-custodial, multi-chain wallet.',
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
          maintainer: 'The Caribou Developers',
          homepage: 'https://github.com/bisoncraft/caribou',
          icon: '../src/caribou.png',
          categories: ['Office', 'Finance'],
          description: 'A non-custodial, multi-chain wallet.',
        },
      },
    },
    {
      name: '@electron-forge/maker-rpm',
      config: {
        options: {
          maintainer: 'The Caribou Developers',
          summary: 'A non-custodial, multi-chain wallet.',
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
