import t from './intl'
import app from './application'
import Doc from './doc'
import { postJSON } from './http'
import { SupportedAsset, UnitInfo, WalletTransaction } from './registry'
import { IntentsAsset, IntentsSwap, StateProcessing, swapIsActive, assetTicker, assetName } from './intents'

// Pure helpers behind the cross-chain swap flow, shared by the desktop and
// mobile shells. The stateful engine is useSwapForm (form, preview, execute)
// and useSwapProgress (a swap record and its deposit transaction).

export const slippagePresets = [50, 100, 200] // basis points

// slippagePct renders basis points as a percentage string.
export function slippagePct (bps: number): string {
  return `${(bps / 100).toLocaleString()}%`
}

// ---- Custody and bridge trust ----

// bridgeTrustMap classifies settlement chains by the custody model of their
// Intents bridge leg. Data-driven rather than baked copy because the
// PoA→Omni migration is ongoing; an unlisted chain falls back to a generic
// third-party label. Values are translation keys, resolved at call time so
// the active locale applies.
const bridgeTrustOmni = 'bridge custody by a distributed MPC signing network (Omni)'
const bridgeTrustHot = 'bridge custody by a distributed MPC signing network (HOT)'
const bridgeTrustPoA = 'bridge custody by a validator federation operated for the service'
const bridgeTrustMap: Record<string, string> = {
  BTC: bridgeTrustOmni,
  ETH: bridgeTrustOmni,
  SOL: bridgeTrustOmni,
  BNB: bridgeTrustOmni,
  POL: bridgeTrustHot,
  LTC: bridgeTrustPoA,
  DOGE: bridgeTrustPoA,
  XRP: bridgeTrustPoA,
  ZEC: bridgeTrustPoA,
  BCH: bridgeTrustPoA,
  DASH: bridgeTrustPoA,
  NEAR: 'settles natively on the NEAR network, with no bridge leg'
}

// bridgeTrust describes the custody model of a settlement chain's bridge leg.
export function bridgeTrust (chain: string): string {
  return t(bridgeTrustMap[chain.toUpperCase()] ?? 'bridge custody by a third-party bridge operator')
}

// custodyDisclosure is the lead paragraph of the custody disclosure shown
// before a swap is confirmed.
export function custodyDisclosure (): string {
  return t('Swaps are executed by NEAR Intents, operated by Defuse Labs Ltd (Gibraltar). This is not an atomic swap: while it completes, typically within a few minutes, the service and its bridge operators hold the funds.')
}

// screeningDisclosure closes the custody disclosure: compliance screening,
// automatic refunds, and seed-derived addresses.
export function screeningDisclosure (): string {
  return t('Deposits are screened for sanctions compliance and may rarely be delayed or refused. If a swap cannot complete, funds are returned automatically. All refund and destination addresses are derived from the wallet seed, so funds at rest are never in third-party custody.')
}

// chainTicker resolves the settlement network's native ticker for an Intents
// asset. Trust classification is per chain, not per token: a token's bridge
// leg is custodied by its network's bridge.
export function chainTicker (a: IntentsAsset): string {
  const wa = app.user?.assets[a.assetID]
  const chainID = wa?.token ? wa.token.networkID : a.assetID
  return app.user?.assets[chainID]?.unitInfo.conventional.unit ?? a.symbol
}

// ---- Amounts and pricing ----

// unitInfoFor returns the wallet UnitInfo for a swappable asset, or a
// minimal stand-in built from the catalog scale when the asset is unknown
// to this build's wallet map (walletReady is false for those, so the
// stand-in only ever formats display amounts).
export function unitInfoFor (a: IntentsAsset): UnitInfo {
  const sa = app.user?.assets[a.assetID]
  if (sa) return sa.unitInfo
  const unit = a.symbol.toUpperCase()
  return {
    atomicUnit: unit,
    conventional: { unit, conversionFactor: Math.pow(10, a.atomDecimals) },
    denominations: [],
    feeRateDenom: ''
  }
}

// usdRate is an asset's USD display rate, preferring the app's live rate
// over the catalog price.
export function usdRate (a: IntentsAsset): number {
  return app.idRate(a.assetID) || a.priceUSD
}

// usdValue prices conventional units of a swappable asset.
export function usdValue (a: IntentsAsset, conventional: number): number {
  return conventional * usdRate(a)
}

// availableAtoms is the available balance of an asset's wallet, or zero
// without one.
export function availableAtoms (assetID: number | null): number {
  return assetID === null ? 0 : app.wallet(assetID)?.balance?.available ?? 0
}

// balanceUSD prices a swappable asset's available wallet balance.
export function balanceUSD (a: IntentsAsset): number {
  return usdValue(a, availableAtoms(a.assetID) / Math.pow(10, a.atomDecimals))
}

// plainAtoms renders atoms as a plain decimal string for use as an input
// value — no locale grouping, which would fail the decimal validation on
// the next keystroke.
export function plainAtoms (atoms: number, decimals: number): string {
  if (atoms <= 0) return ''
  return (atoms / Math.pow(10, decimals)).toFixed(decimals).replace(/\.?0+$/, '')
}

// plainUSD renders a dollar value as a plain input string.
export function plainUSD (v: number): string {
  if (v <= 0) return ''
  return v.toFixed(2)
}

// displayBalance floors a conventional balance to four significant digits
// (capped at eight decimals) so the line stays short and never overstates
// holdings. Max still spends full precision.
export function displayBalance (v: number): string {
  if (v <= 0) return '0'
  const digits = Math.min(8, Math.max(0, 4 - Math.floor(Math.log10(v)) - 1))
  const f = Math.pow(10, digits)
  return (Math.floor(v * f) / f).toLocaleString(undefined, { maximumFractionDigits: digits })
}

// swapPickList filters the swappable assets by a search query (ticker, name,
// or catalog symbol) and orders them for the asset picker: ready wallets
// first, by USD balance, with inactive entries trailing.
export function swapPickList (assets: IntentsAsset[], query: string): IntentsAsset[] {
  const q = query.trim().toLowerCase()
  const list = assets.filter(a => {
    if (q === '') return true
    return assetTicker(a).toLowerCase().includes(q) ||
      assetName(a).toLowerCase().includes(q) ||
      a.symbol.toLowerCase().includes(q)
  })
  list.sort((a, b) => {
    if (a.walletReady !== b.walletReady) return a.walletReady ? -1 : 1
    return balanceUSD(b) - balanceUSD(a)
  })
  return list
}

// ---- Progress ----

// activeStep is the progress step of an active swap: 0 deposit, 1 swapping
// (2, delivered, is terminal). Every pre-PROCESSING state is the deposit
// phase.
export function activeStep (state: string): number {
  return state === StateProcessing ? 1 : 0
}

// fetchWalletTx loads a single transaction from a wallet's history, or
// null when the wallet has no record of it.
export async function fetchWalletTx (assetID: number, txID: string): Promise<WalletTransaction | null> {
  const r = await postJSON('/api/wallettx', { assetID, txID })
  return r.ok && r.tx ? r.tx as WalletTransaction : null
}

// SwapProgressInfo is the display data derived from a swap record and its
// deposit transaction.
export interface SwapProgressInfo {
  active: boolean
  // depositRejected: the record is still active (the engine has not yet
  // polled), but the fetched deposit tx already proves an on-chain
  // rejection. The header and step indicator show the failure rather than
  // a healthy in-flight swap.
  depositRejected: boolean
  step: number
  originTicker: string
  destTicker: string
  amountIn: string
  amountOut: string
  usdIn: string
  usdOut: string
  rateText: string
  feeBps: number
  // refundedText is the refund in origin units, or '' when the record has
  // no resolved refund amount. refundedUSD may be '' independently.
  refundedText: string
  refundedUSD: string
  // feeAsset is the origin chain's native asset, which pays the deposit's
  // network fee. feeUSD prices the deposit tx's fee, or '' when unknown.
  feeAsset?: SupportedAsset
  feeUSD: string
}

// swapProgressInfo derives a swap record's display values. depositTx is the
// deposit leg as fetched by useDepositTx: undefined while loading, null when
// the wallet has no record of it.
export function swapProgressInfo (swap: IntentsSwap, depositTx: WalletTransaction | null | undefined): SwapProgressInfo {
  const active = swapIsActive(swap)
  const originAsset = app.user?.assets[swap.originAssetID]
  const destAsset = app.user?.assets[swap.destAssetID]
  const originTicker = originAsset?.unitInfo.conventional.unit ?? swap.originAsset
  const destTicker = destAsset?.unitInfo.conventional.unit ?? swap.destAsset
  const amountIn = originAsset ? Doc.formatCoinValue(swap.atomsIn, originAsset.unitInfo) : swap.amountIn
  const amountOut = destAsset ? Doc.formatCoinValue(swap.atomsOut, destAsset.unitInfo) : swap.amountOut

  const inFactor = originAsset?.unitInfo.conventional.conversionFactor ?? 0
  const outFactor = destAsset?.unitInfo.conventional.conversionFactor ?? 0
  const usdIn = inFactor ? Doc.usdString(swap.atomsIn, inFactor, app.idRate(swap.originAssetID) ?? 0) : ''
  const usdOut = outFactor ? Doc.usdString(swap.atomsOut, outFactor, app.idRate(swap.destAssetID) ?? 0) : ''
  const inConv = inFactor ? swap.atomsIn / inFactor : 0
  const outConv = outFactor ? swap.atomsOut / outFactor : 0
  const rateText = inConv > 0 && outConv > 0
    ? `1 ${originTicker} = ${Doc.formatFourSigFigs(outConv / inConv)} ${destTicker}`
    : ''
  const feeBps = (swap.appFees ?? []).reduce((sum, f) => sum + f.fee, 0)
  // The refund's origin-atoms conversion and USD pricing are captured on the
  // record at refund time; an older or unresolvable record leaves them zero
  // and the row is omitted.
  const refundedAtoms = swap.refundedAtoms ?? 0
  const refundedText = originAsset && refundedAtoms > 0
    ? `${Doc.formatCoinValue(refundedAtoms, originAsset.unitInfo)} ${originTicker}`
    : ''
  const refundedUSD = Doc.formatUsd(swap.usdRefunded ?? 0)

  // The deposit's network fee is paid in the origin chain's native asset.
  const feeAssetID = originAsset?.token ? originAsset.token.networkID : swap.originAssetID
  const feeAsset = app.user?.assets[feeAssetID]
  const feeUSD = depositTx && feeAsset
    ? Doc.usdString(depositTx.fees, feeAsset.unitInfo.conventional.conversionFactor, app.idRate(feeAssetID) ?? 0)
    : ''

  return {
    active,
    depositRejected: active && depositTx?.rejected === true,
    step: activeStep(swap.state),
    originTicker,
    destTicker,
    amountIn,
    amountOut,
    usdIn,
    usdOut,
    rateText,
    feeBps,
    refundedText,
    refundedUSD,
    feeAsset,
    feeUSD
  }
}
