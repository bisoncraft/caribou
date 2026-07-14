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
  bestID: number
  name: string
  variants: AssetVariant[]
  variantLookup: Record<number, AssetVariant>
  haveAllFiatRates: boolean
  isMultiNet: boolean
  hasTokens: boolean
  ui: UnitInfo

  constructor (a: SupportedAsset) {
    const { id: assetID, name, unitInfo: ui } = a
    this.ticker = normalizedTicker(a)
    this.variants = []
    this.variantLookup = {}
    this.bestID = assetID
    this.name = name
    this.ui = ui
    this.addVariant(a)
  }

  // cFactor is the conversion factor of the group's preferred (native) unit
  // info — the denomination in which the aggregate balance getters report.
  get cFactor () {
    return this.ui.conventional.conversionFactor
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

  /*
   * toGroupAtoms converts a variant's atomic amount into the group's preferred
   * atomic denomination (see cFactor), so cross-variant sums stay meaningful
   * even if variants denominate with different conversion factors. For every
   * current group the factors match and this is the identity.
   */
  toGroupAtoms (v: AssetVariant, atomic: number): number {
    return atomic * (this.cFactor / v.ui.conventional.conversionFactor)
  }

  get avail () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + this.toGroupAtoms(v, v.bal.available), 0)
  }

  get immature () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + this.toGroupAtoms(v, v.bal.immature), 0)
  }

  get locked () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + this.toGroupAtoms(v, v.bal.locked), 0)
  }

  get total () {
    return this.variants.reduce((sum: number, v: AssetVariant) => {
      const { available, locked, immature } = v.bal
      return sum + this.toGroupAtoms(v, available + locked + immature)
    }, 0)
  }

  get xcRate () {
    return app.idRate(this.bestID)
  }
}

export function normalizedTicker (a: SupportedAsset): string {
  return a.tickerGroup || a.unitInfo.conventional.unit
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

// NetworkAssetRef identifies one asset that settles on a network, carrying just
// enough to render a logo and label.
export interface NetworkAssetRef {
  assetID: number
  ticker: string
}

// NetworkSummary describes one settlement network — a native blockchain asset —
// and the assets that settle on it. supportedAssets lists the native asset
// first, followed by its tokens sorted by ticker; assetCount is its length.
export interface NetworkSummary {
  nativeAssetID: number
  name: string
  ticker: string
  // symbol is the native asset's bip symbol (e.g. "polygon", "hype"), used as
  // the URL path for the network view.
  symbol: string
  assetCount: number
  activated: boolean
  supportedAssets: NetworkAssetRef[]
}

// summarizeNetwork builds the NetworkSummary for one native asset, collecting
// the tokens that settle on it from the full asset map. supportedAssets lists
// the native asset first, then its tokens sorted by ticker.
function summarizeNetwork (a: SupportedAsset, assets: Record<number, SupportedAsset>): NetworkSummary {
  const tokens: NetworkAssetRef[] = []
  for (const t of Object.values(assets)) {
    if (t.token?.networkID === a.id) tokens.push({ assetID: t.id, ticker: t.unitInfo.conventional.unit })
  }
  tokens.sort((x, y) => x.ticker.localeCompare(y.ticker))
  const supportedAssets = [{ assetID: a.id, ticker: a.unitInfo.conventional.unit }, ...tokens]
  return {
    nativeAssetID: a.id,
    name: a.name,
    ticker: a.unitInfo.conventional.unit,
    symbol: a.symbol,
    assetCount: supportedAssets.length,
    activated: Boolean(a.wallet),
    supportedAssets
  }
}

// networkList enumerates the settlement networks. A network corresponds to a
// native asset (one without a token); tokens are grouped under their parent
// network rather than listed as networks of their own. The list is sorted
// alphabetically by name for a stable ordering across renders.
export function networkList (): NetworkSummary[] {
  const assets = app.user?.assets ?? {}
  const networks = Object.values(assets).filter(a => !a.token).map(a => summarizeNetwork(a, assets))
  networks.sort((x, y) => x.name.localeCompare(y.name))
  return networks
}

// networkSummary returns the summary for a single network by its native asset
// ID, or undefined when the ID is not a known native asset.
export function networkSummary (nativeAssetID: number): NetworkSummary | undefined {
  const assets = app.user?.assets ?? {}
  const a = assets[nativeAssetID]
  return a && !a.token ? summarizeNetwork(a, assets) : undefined
}

// networkBySymbol returns the summary for a network by its native asset's bip
// symbol (e.g. "polygon", "hype"), or undefined when no such network exists.
export function networkBySymbol (symbol: string): NetworkSummary | undefined {
  const assets = app.user?.assets ?? {}
  const a = Object.values(assets).find(x => !x.token && x.symbol === symbol)
  return a ? summarizeNetwork(a, assets) : undefined
}
