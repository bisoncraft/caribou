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

  // cFactor is the variant's conventional conversion factor (atomic units per
  // whole unit).
  get cFactor () {
    return this.ui.conventional.conversionFactor
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
  name: string
  hasWallets: boolean
  variants: AssetVariant[]
  variantLookup: Record<number, AssetVariant>
  isMultiNet: boolean
  hasTokens: boolean

  constructor (a: SupportedAsset) {
    this.ticker = normalizedTicker(a)
    this.name = a.name
    this.variants = []
    this.variantLookup = {}
    this.addVariant(a)
  }

  addVariant (a: SupportedAsset) {
    const { wallet: w, token } = a
    this.hasTokens = this.hasTokens || Boolean(token)
    const v = new AssetVariant(a)
    this.hasWallets = this.hasWallets || Boolean(w) || Boolean(v.token?.networkMade)
    this.variants.push(v)
    this.variantLookup[a.id] = v
    this.variants.sort((a: AssetVariant, b: AssetVariant) => {
      if (a.token && !b.token) return 1
      if (!a.token && b.token) return -1
      // Variants of an all-token group share a conventional ticker, so tie-break
      // on assetID for a stable, deterministic ordering across app restarts (the
      // variants render as the per-network balance breakdown).
      const byTicker = a.ticker.localeCompare(b.ticker)
      return byTicker !== 0 ? byTicker : a.assetID - b.assetID
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

  // avail, locked, immature, and total report the group's balance as conventional
  // (whole-unit) numbers, summed across network variants. Each variant is divided
  // by its own conversion factor before summing, so the result is directly
  // displayable and priceable (multiply by xcRate for fiat) and stays correct
  // regardless of whether variants share a conversion factor.
  get avail () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + v.bal.available / v.cFactor, 0)
  }

  get immature () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + v.bal.immature / v.cFactor, 0)
  }

  get locked () {
    return this.variants.reduce((sum: number, v: AssetVariant) => sum + v.bal.locked / v.cFactor, 0)
  }

  get total () {
    return this.variants.reduce((sum: number, v: AssetVariant) => {
      const { available, locked, immature } = v.bal
      return sum + (available + locked + immature) / v.cFactor
    }, 0)
  }

  // precision is the number of fractional digits needed to render the group's
  // conventional balances losslessly: the maximum across variants, since a
  // variant with a larger conversion factor can contribute a quantity the
  // others cannot represent. Pair it with Doc.formatConventionalValue.
  get precision () {
    return this.variants.reduce((max: number, v: AssetVariant) => Math.max(max, Math.round(Math.log10(v.cFactor))), 0)
  }

  get xcRate () {
    return app.tickerRate(this.ticker)
  }
}

export function normalizedTicker (a: SupportedAsset): string {
  return a.tickerGroup || a.unitInfo.conventional.unit
}

/*
 * compareByFiatValue orders TickerAssets by descending fiat value — the
 * portfolio table's row order. Assets without a known rate count as zero;
 * ties keep their original relative order (Array.prototype.sort is stable).
 */
export function compareByFiatValue (a: TickerAsset, b: TickerAsset): number {
  return b.total * (b.xcRate ?? 0) - a.total * (a.xcRate ?? 0)
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

// isVariantActive reports whether the variant's own wallet exists, which is
// what receiving to it or sending from it requires. Token wallets are created
// alongside their network wallet, so for a token this normally coincides with
// the network wallet existing; a token whose own wallet is missing (a creation
// that failed and is self-healed at the next start) must not pass as active,
// since nothing can serve its deposit address or trust-line state.
export function isVariantActive (v: AssetVariant): boolean {
  return Boolean(app.user?.assets[v.assetID]?.wallet)
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
