import app from './application'

// Network identifiers, matching util.Network on the Go side.
const Mainnet = 0
const Testnet = 1
const Simnet = 2

// txURL builds a block-explorer transaction URL for a given network. cid is a
// transaction ID, possibly suffixed (e.g. "<txid>:<vout>"); the suffix is
// stripped.
type txURL = (cid: string) => string

function hash (cid: string): string {
  return cid.split(':')[0]
}

const ethExplorers: Record<number, txURL> = {
  [Mainnet]: (cid) => `https://etherscan.io/tx/${hash(cid)}`,
  [Testnet]: (cid) => `https://sepolia.etherscan.io/tx/${hash(cid)}`,
  [Simnet]: (cid) => `https://etherscan.io/tx/${hash(cid)}`
}

const polygonExplorers: Record<number, txURL> = {
  [Mainnet]: (cid) => `https://polygonscan.com/tx/${hash(cid)}`,
  [Testnet]: (cid) => `https://amoy.polygonscan.com/tx/${hash(cid)}`,
  [Simnet]: (cid) => `https://polygonscan.com/tx/${hash(cid)}`
}

const baseExplorers: Record<number, txURL> = {
  [Mainnet]: (cid) => `https://basescan.org/tx/${hash(cid)}`,
  [Testnet]: (cid) => `https://base-sepolia.blockscout.com/tx/${hash(cid)}`
}

// Explorers keyed by base-chain asset ID, then by network.
const explorers: Record<number, Record<number, txURL>> = {
  42: { // dcr
    [Mainnet]: (cid) => `https://dcrdata.decred.org/tx/${hash(cid)}`,
    [Testnet]: (cid) => `https://testnet.decred.org/tx/${hash(cid)}`,
    [Simnet]: (cid) => `http://127.0.0.1:17779/tx/${hash(cid)}`
  },
  0: { // btc
    [Mainnet]: (cid) => `https://mempool.space/tx/${hash(cid)}`,
    [Testnet]: (cid) => `https://mempool.space/testnet/tx/${hash(cid)}`,
    [Simnet]: (cid) => `https://mempool.space/tx/${hash(cid)}`
  },
  2: { // ltc
    [Mainnet]: (cid) => `https://ltc.bitaps.com/${hash(cid)}`,
    [Testnet]: (cid) => `https://sochain.com/tx/LTCTEST/${hash(cid)}`,
    [Simnet]: (cid) => `https://ltc.bitaps.com/${hash(cid)}`
  },
  20: { // dgb
    [Mainnet]: (cid) => `https://digiexplorer.info/tx/${hash(cid)}`,
    [Testnet]: (cid) => `https://testnetexplorer.digibyteservers.io/tx/${hash(cid)}`,
    [Simnet]: (cid) => `https://digiexplorer.info/tx/${hash(cid)}`
  },
  60: ethExplorers, // eth
  60001: ethExplorers,
  60002: ethExplorers,
  8453: baseExplorers, // base (weth.base)
  61000: baseExplorers,
  61001: baseExplorers,
  3: { // doge
    [Mainnet]: (cid) => `https://dogeblocks.com/tx/${hash(cid)}`,
    [Testnet]: (cid) => `https://blockexplorer.one/dogecoin/testnet/tx/${hash(cid)}`,
    [Simnet]: (cid) => `https://dogeblocks.com/tx/${hash(cid)}`
  },
  5: { // dash
    [Mainnet]: (cid) => `https://blockexplorer.one/dash/mainnet/tx/${hash(cid)}`,
    [Testnet]: (cid) => `https://blockexplorer.one/dash/testnet/tx/${hash(cid)}`,
    [Simnet]: (cid) => `https://blockexplorer.one/dash/mainnet/tx/${hash(cid)}`
  },
  133: { // zec
    [Mainnet]: (cid) => `https://zcashblockexplorer.com/transactions/${hash(cid)}`,
    [Testnet]: (cid) => `https://blockexplorer.one/zcash/testnet/tx/${hash(cid)}`,
    [Simnet]: (cid) => `https://zcashblockexplorer.com/transactions/${hash(cid)}`
  },
  128: { // xmr
    [Mainnet]: (cid) => `https://monerohash.com/explorer/tx/${hash(cid)}`
  },
  136: { // firo
    [Mainnet]: (cid) => `https://explorer.firo.org/tx/${hash(cid)}`,
    [Testnet]: (cid) => `https://testexplorer.firo.org/tx/${hash(cid)}`,
    [Simnet]: (cid) => `https://explorer.firo.org/tx/${hash(cid)}`
  },
  145: { // bch
    [Mainnet]: (cid) => `https://bch.loping.net/tx/${hash(cid)}`,
    [Testnet]: (cid) => `https://tbch4.loping.net/tx/${hash(cid)}`,
    [Simnet]: (cid) => `https://bch.loping.net/tx/${hash(cid)}`
  },
  966: polygonExplorers, // polygon
  966001: polygonExplorers,
  966004: polygonExplorers
}

// baseChainID returns the settlement-network asset ID for an asset: the parent
// network for tokens, or the asset itself for natives.
function baseChainID (assetID: number): number {
  const asset = app.user?.assets[assetID]
  return asset?.token ? asset.token.networkID : assetID
}

// explorerTxURL returns a block-explorer URL for a transaction on the asset's
// network, or null when no explorer is configured for that asset/network.
export function explorerTxURL (assetID: number, txID: string): string | null {
  const net = app.user?.net ?? Mainnet
  const assetExplorer = explorers[baseChainID(assetID)]
  const formatter = assetExplorer?.[net]
  if (!formatter) return null
  return formatter(txID)
}
