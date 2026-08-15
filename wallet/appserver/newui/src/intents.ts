import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { getJSON } from '@caribou/shared/http'

// Types mirroring the Go structs served by the /api/intents endpoints
// (wallet/core/near-intents.go, wallet/db/near-intents.go).

export interface AppFee {
  recipient: string
  fee: number
}

export interface IntentsAsset {
  assetID: number
  symbol: string
  intentsID: string
  // decimals is the Intents-side scale; NOT used for atoms conversion.
  decimals: number
  // atomDecimals is the wallet atomic-unit exponent. All atoms fields in
  // previews and swap records (and the value posted to preview/execute)
  // are expressed at this scale. It can differ from decimals (e.g. NEAR:
  // 8 wallet drops vs 24 Intents decimals).
  atomDecimals: number
  priceUSD: number
  walletReady: boolean
}

export interface IntentsPreview {
  atomsIn: number
  atomsOut: number
  minAtomsOut: number
  minAtomsIn: number
  amountInUSD: string
  amountOutUSD: string
  timeEstimate: number // seconds
  appFees: AppFee[]
  refundFee: string
  withdrawFee: string
}

export interface IntentsSwap {
  depositAddress: string
  depositMemo?: string
  state: string
  originAssetID: number
  destAssetID: number
  originAsset: string
  destAsset: string
  atomsIn: number
  amountIn: string
  amountOut: string
  atomsOut: number
  recipient: string
  refundTo: string
  deadline: string
  timeEstimate: number
  depositTxID?: string
  nearTxHashes?: string[]
  destinationTxs?: string[]
  refundTxs?: string[]
  refundedAmount?: string
  refundReason?: string
  appFees?: AppFee[]
  reconstructed?: boolean
  createdAt: string
  updatedAt: string
}

// Swap states: the engine-local states plus the service statuses
// (wallet/db/near-intents.go, wallet/intents/types.go).
export const StateQuoted = 'QUOTED'
export const StateDepositSent = 'DEPOSIT_SENT'
export const StatePendingDeposit = 'PENDING_DEPOSIT'
export const StateKnownDepositTx = 'KNOWN_DEPOSIT_TX'
export const StateIncompleteDeposit = 'INCOMPLETE_DEPOSIT'
export const StateProcessing = 'PROCESSING'
export const StateSuccess = 'SUCCESS'
export const StateRefunded = 'REFUNDED'
export const StateFailed = 'FAILED'
export const StateExpired = 'EXPIRED'

const terminalStates = new Set([StateSuccess, StateRefunded, StateFailed, StateExpired])

// swapIsActive reports whether a swap still needs watching: any state the
// engine's poller has not retired.
export function swapIsActive (s: IntentsSwap): boolean {
  return !terminalStates.has(s.state)
}

// stateLabel is the short, user-facing name of a swap state.
export function stateLabel (state: string): string {
  switch (state) {
    case StateQuoted: return t('Awaiting deposit')
    case StateDepositSent:
    case StatePendingDeposit: return t('Deposit sent')
    case StateKnownDepositTx: return t('Deposit detected')
    case StateIncompleteDeposit: return t('Partial deposit')
    case StateProcessing: return t('Swapping')
    case StateSuccess: return t('Completed')
    case StateRefunded: return t('Refunded')
    case StateFailed: return t('Failed')
    case StateExpired: return t('Expired')
    default: return state
  }
}

// stateColorClass maps a swap state to a text-color utility for status text.
export function stateColorClass (state: string): string {
  switch (state) {
    case StateSuccess: return 'tx-amt-pos'
    case StateFailed: return 'errcolor'
    case StateRefunded:
    case StateExpired: return 'grey'
    default: return 'warncolor'
  }
}

// conventionalToAtoms converts a conventional (whole-unit) quantity to wallet
// atoms at the asset's atomDecimals scale.
export function conventionalToAtoms (conventional: number, atomDecimals: number): number {
  return Math.round(conventional * Math.pow(10, atomDecimals))
}

// atomsToConventional converts wallet atoms to conventional units.
export function atomsToConventional (atoms: number, atomDecimals: number): number {
  return atoms / Math.pow(10, atomDecimals)
}

// fetchIntentsAssets loads the assets available for cross-chain swaps.
// The endpoint errors when the swap engine is unavailable this session, in
// which case null lets callers keep their optimistic default rather than
// treating every asset as unswappable.
export async function fetchIntentsAssets (): Promise<IntentsAsset[] | null> {
  const r = await getJSON('/api/intents/assets')
  return r.ok ? (r.assets as IntentsAsset[]) ?? [] : null
}

// fetchSwaps loads the persisted cross-chain swap records, newest first.
// The endpoint errors when the swap engine is unavailable this session, in
// which case there are simply no swaps to show.
export async function fetchSwaps (): Promise<IntentsSwap[]> {
  const r = await getJSON('/api/intents/swaps')
  return r.ok ? (r.swaps as IntentsSwap[]) ?? [] : []
}

// SwapRelation identifies the role an on-chain transaction plays in a
// cross-chain swap.
export interface SwapRelation {
  swap: IntentsSwap
  role: 'deposit' | 'payout' | 'refund'
}

// swapRelationLabel is the short user-facing name of a leg role.
export function swapRelationLabel (role: SwapRelation['role']): string {
  switch (role) {
    case 'deposit': return t('Deposit')
    case 'payout': return t('Payout')
    case 'refund': return t('Refund')
  }
}

// relateTxToSwaps matches a wallet transaction to the swap it serves: the
// deposit leg by its recorded txid or the deposit address it pays, the
// payout and refund legs by the txids the service reported.
export function relateTxToSwaps (swaps: IntentsSwap[], tx: { id: string, recipient?: string }): SwapRelation | undefined {
  const id = tx.id.toLowerCase()
  const recipient = tx.recipient?.toLowerCase()
  for (const swap of swaps) {
    if (swap.depositTxID?.toLowerCase() === id) return { swap, role: 'deposit' }
    if (recipient && swap.depositAddress.toLowerCase() === recipient) return { swap, role: 'deposit' }
    if (swap.destinationTxs?.some(txid => txid.toLowerCase() === id)) return { swap, role: 'payout' }
    if (swap.refundTxs?.some(txid => txid.toLowerCase() === id)) return { swap, role: 'refund' }
  }
  return undefined
}

// assetTicker returns the display ticker for a swappable asset: the wallet's
// conventional unit when the asset is known to this build, else the catalog
// symbol upper-cased.
export function assetTicker (a: IntentsAsset): string {
  const sa = app.user?.assets[a.assetID]
  return sa ? sa.unitInfo.conventional.unit : a.symbol.toUpperCase()
}

// assetName returns the display name for a swappable asset. Token names
// carry their settlement network ("USDC on Ethereum") so same-ticker
// deployments are distinguishable in pickers.
export function assetName (a: IntentsAsset): string {
  const sa = app.user?.assets[a.assetID]
  if (!sa) return a.symbol
  const netName = sa.token ? app.user?.assets[sa.token.networkID]?.name : undefined
  return netName ? tvar('{{ name }} on {{ network }}', { name: sa.name, network: netName }) : sa.name
}
