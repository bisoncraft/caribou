require("@nomicfoundation/hardhat-toolbox");
require('dotenv').config();

/** @type import('hardhat/config').HardhatUserConfig */
module.exports = {
  defaultNetwork: "hardhat",
  networks: {
    hardhat: {},
    ...(process.env.AMOY_RPC_URL ? {
      amoy: {
        url: process.env.AMOY_RPC_URL,
        accounts: [process.env.PRIVATE_KEY]
      }
    } : {}),
    ...(process.env.POLYGON_RPC_URL ? {
      polygon: {
        url: process.env.POLYGON_RPC_URL,
        accounts: [process.env.PRIVATE_KEY]
      }
    } : {})
  },
  solidity: {
    version: "0.8.18",
    settings: {
      optimizer: {
        enabled: true,
        runs: 1000
      }
    }
  },
  etherscan: {
    apiKey: {
      polygon: process.env.POLYGONSCAN_API_KEY || "",
      polygonAmoy: process.env.POLYGONSCAN_API_KEY || "",
    },
    customChains: [
      {
        network: "polygonAmoy",
        chainId: 80002,
        urls: {
          apiURL: "https://api-amoy.polygonscan.com/api",
          browserURL: "https://amoy.polygonscan.com"
        }
      }
    ]
  },
  sourcify: {
    enabled: true
  }
};
