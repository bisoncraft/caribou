import t, { tvar } from './intl'
import Doc from './doc'
import { WalletTransaction } from './registry'
import { TickerAsset, AssetVariant } from './assets'
import { SwapRelation } from './intents'

// TxType mirrors wallet/txdb/types.go TransactionType. Keep in sync with the Go
// enum — the ordering is the wire value.
export const TxType = {
  Unknown: 0,
  Send: 1,
  Receive: 2,
  Swap: 3,
  Redeem: 4,
  Refund: 5,
  Split: 6,
  ApproveToken: 7,
  Acceleration: 8,
  SelfSend: 9,
  RevokeTokenApproval: 10,
  TicketPurchase: 11,
  TicketVote: 12,
  TicketRevocation: 13,
  SwapOrSend: 14,
  Mix: 15,
  InitiateBridge: 16,
  CompleteBridge: 17,
  DeployContract: 18
} as const

// English labels indexed by TransactionType value.
const txTypeLabels: string[] = [
  'Unknown',
  'Send',
  'Receive',
  'Swap',
  'Redeem',
  'Refund',
  'Split',
  'Approve Token',
  'Acceleration',
  'Self Send',
  'Revoke Token Approval',
  'Ticket Purchase',
  'Ticket Vote',
  'Ticket Revocation',
  'Send',
  'Mix',
  'Bridge Initiation',
  'Bridge Completion',
  'Deploy Contract'
]

// Types whose balance effect is an increase.
const incomingTypes = new Set<number>([
  TxType.Receive,
  TxType.Redeem,
  TxType.Refund,
  TxType.TicketVote,
  TxType.TicketRevocation,
  TxType.CompleteBridge
])

// Types with no meaningful transfer amount (fee-only or approvals).
const noAmountTypes = new Set<number>([
  TxType.Split,
  TxType.ApproveToken,
  TxType.Acceleration,
  TxType.RevokeTokenApproval,
  TxType.DeployContract
])

// Types that decrease the balance.
const outgoingTypes = new Set<number>([
  TxType.Send,
  TxType.Swap,
  TxType.SwapOrSend,
  TxType.TicketPurchase,
  TxType.InitiateBridge
])

// txTypeLabel returns the translated display label for a transaction type.
export function txTypeLabel (txType: number): string {
  return t(txTypeLabels[txType] ?? txTypeLabels[TxType.Unknown])
}

// txIsIncoming reports whether the transaction increases the wallet balance.
export function txIsIncoming (txType: number): boolean {
  return incomingTypes.has(txType)
}

// txShowsAmount reports whether a transfer amount is meaningful for the type.
export function txShowsAmount (txType: number): boolean {
  return !noAmountTypes.has(txType)
}

// txAmountSign returns the leading sign for a transaction amount: '+' for
// incoming, '-' for outgoing, or '' for neutral (e.g. self-send) or
// no-amount types.
export function txAmountSign (txType: number): string {
  if (incomingTypes.has(txType)) return '+'
  if (outgoingTypes.has(txType)) return '-'
  return ''
}

// TxRowItem pairs a transaction with the ticker group and network variant it
// belongs to, which carry the units, rate, and asset ID needed to render and
// link it. swapRelation is set when the transaction is a leg of a
// cross-chain swap (its deposit, payout, or refund).
export interface TxRowItem {
  tx: WalletTransaction
  ta: TickerAsset
  variant: AssetVariant
  swapRelation?: SwapRelation
}

// variantForTx maps a history transaction back to the network variant it
// belongs to. Every transaction carries its exact asset ID.
export function variantForTx (ta: TickerAsset, tx: WalletTransaction): AssetVariant | undefined {
  return ta.variantLookup[tx.assetID]
}

// txTitle names a row: past tense for the common types, the generic type
// label otherwise.
export function txTitle (tx: WalletTransaction, ticker: string): string {
  switch (tx.type) {
    case TxType.Send: return `${t('Sent')} ${ticker}`
    case TxType.Receive: return `${t('Received')} ${ticker}`
    case TxType.Swap: return `${t('Swapped')} ${ticker}`
    default: return `${txTypeLabel(tx.type)} ${ticker}`
  }
}

// txIcon is the ico-* glyph class for a transaction type's row icon.
export function txIcon (type: number): string {
  if (type === TxType.Swap || type === TxType.SwapOrSend) return 'ico-swap'
  if (txIsIncoming(type)) return 'ico-download'
  return 'ico-upload'
}

// txStatus renders confirmation progress while confirming, then age.
export function txStatus (tx: WalletTransaction): string {
  if (!tx.confirmed && tx.confirms) {
    return tvar('{{ current }}/{{ target }} confs', {
      current: String(tx.confirms.current),
      target: String(tx.confirms.target)
    })
  }
  if (tx.timestamp > 0) return tvar('{{ age }} ago', { age: Doc.timeSince(tx.timestamp * 1000) })
  return t('pending')
}
