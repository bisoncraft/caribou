require("@nomicfoundation/hardhat-toolbox");
require('dotenv').config();

/** @type import('hardhat/config').HardhatUserConfig */
module.exports = {
  defaultNetwork: "hardhat",
  networks: {
    hardhat: {},
    ...(process.env.AMOY_RPC_URL ? {
      polygonAmoy: {
        url: process.env.AMOY_RPC_URL,
        accounts: process.env.PRIVATE_KEY ? [process.env.PRIVATE_KEY] : []
      }
    } : {}),
    ...(process.env.POLYGON_RPC_URL ? {
      polygon: {
        url: process.env.POLYGON_RPC_URL,
        accounts: process.env.PRIVATE_KEY ? [process.env.PRIVATE_KEY] : []
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
    // Etherscan V2 unified multichain API: a single etherscan.io API key
    // verifies across all supported chains (Polygon mainnet 137 and Amoy
    // 80002 are built in), routed by chainId. A string apiKey selects V2; the
    // legacy per-explorer object form hits the Polygonscan V1 API, which was
    // sunset on 2025-05-31. POLYGONSCAN_API_KEY is still accepted as a
    // fallback name, but its value must now be an etherscan.io key.
    apiKey: process.env.ETHERSCAN_API_KEY || process.env.POLYGONSCAN_API_KEY || "",
  },
  sourcify: {
    enabled: true
  }
};
