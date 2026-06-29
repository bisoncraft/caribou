import {
  UnitInfo,
  SupportedAsset,
  WalletInfo
} from './registry'
import app from './application'

// AssetVariant is one (asset, network) deployment — e.g. USDC-on-Ethereum.
// A TickerAsset groups all variants sharing a normalized ticker.
export class AssetVariant {
  assetID: number
  symbol: string
  ui: UnitInfo
  ticker: string
  token?: {
    networkMade: boolean
    networkID: number
    feeUI: UnitInfo
  }

  constructor (a: SupportedAsset) {
    const { id: assetID, symbol, token, unitInfo: ui, unitInfo: { conventional: { unit: ticker } } } = a
    this.assetID = assetID
    this.ticker = ticker
    this.symbol = symbol
    this.ui = ui
    if (token) {
      this.token = {
        networkID: token.networkID,
        feeUI: app.unitInfo(token.networkID),
        networkMade: Boolean(app.user.assets[token.networkID]?.wallet)
      }
    }
  }

  get bal () {
    const w = app.user.assets[this.assetID].wallet
    return w?.balance ?? { available: 0, locked: 0, immature: 0 }
  }

  updateTokenNetworkMade () {
    if (!this.token) return false
    this.token.networkMade = Boolean(app.user.assets[this.token.networkID].wallet)
  }
}

export class TickerAsset {
  ticker: string // normalized e.g. WETH -> ETH
  hasWallets: boolean
  cFactor: number
  bestID: number
  name: string
  variants: AssetVariant[]
  variantLookup: Record<number, AssetVariant>
  haveAllFiatRates: boolean
  isMultiNet: boolean
  hasTokens: boolean
  ui: UnitInfo

  constructor (a: SupportedAsset) {
    const { id: assetID, name, unitInfo: ui, unitInfo: { conventional: { conversionFactor: cFactor } } } = a
    this.ticker = normalizedTicker(a)
    this.cFactor = cFactor
    this.variants = []
    this.variantLookup = {}
    this.bestID = assetID
    this.name = name
    this.ui = ui
    this.addVariant(a)
  }

  addVariant (a: SupportedAsset) {
    const { id: assetID, name, wallet: w, token, unitInfo: ui } = a
    const xcRate = app.idRate(assetID)
    if (!xcRate) this.haveAllFiatRates = false
    this.hasTokens = this.hasTokens || Boolean(token)
    if (!token) { // prefer the native asset data, e.g. weth.polygon -> eth}
      this.bestID = assetID
      this.name = name
      this.ui = ui
    }
    const v = new AssetVariant(a)
    this.hasWallets = this.hasWallets || Boolean(w) || Boolean(v.token?.networkMade)
    this.variants.push(v)
    this.variantLookup[a.id] = v
    this.variants.sort((a: AssetVariant, b: AssetVariant) => {
      if (a.token && !b.token) return 1
      if (!a.token && b.token) return -1
      return a.ticker.localeCompare(b.ticker)
    })
    this.isMultiNet = this.variants.length > 1
  }

  walletInfo (): WalletInfo | undefined {
    for (const { assetID } of this.variants) {
      const { info } = app.asset(assetID)
      if (info) return info
    }
  }

  updateHasWallets () {
    for (const v of this.variants) {
      v.updateTokenNetworkMade()
      const { assetID, token } = v
      if (app.wallet(assetID) || token?.networkMade) {
        this.hasWallets = true
        return
      }
    }
  }

  /*
   * blockchainWallet returns the assetID and wallet for the blockchain for
   * which this ticker is a native asset, if it exists.
   */
  blockchainWallet () {
    for (const { assetID } of this.variants) {
      const { wallet, token } = app.asset(assetID)
      if (!token) return { assetID, wallet }
    }
  }

  get avail () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + v.bal.available, 0)
  }

  get immature () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + v.bal.immature, 0)
  }

  get locked () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + v.bal.locked, 0)
  }

  get total () {
    return this.variants.reduce((sum: number, { bal: { available, locked, immature } }: AssetVariant) => {
      return sum + available + locked + immature
    }, 0)
  }

  get xcRate () {
    return app.idRate(this.bestID)
  }
}

export function normalizedTicker (a: SupportedAsset): string {
  const ticker = a.unitInfo.conventional.unit
  return ticker === 'WETH' ? 'ETH' : ticker === 'WBTC' ? 'BTC' : ticker
}

// networkID is the asset whose wallet settles a variant: the parent network for
// tokens, the asset itself for natives. Token activation, deposit addresses and
// fee assets all resolve through it.
export function networkID (v: AssetVariant): number {
  return v.token ? v.token.networkID : v.assetID
}

// networkAsset is the SupportedAsset for a variant's settlement network, or
// undefined when user state is not loaded.
export function networkAsset (v: AssetVariant) {
  return app.user?.assets[networkID(v)]
}

// isVariantActive reports whether the variant's settlement-network wallet exists.
export function isVariantActive (v: AssetVariant): boolean {
  return Boolean(networkAsset(v)?.wallet)
}
