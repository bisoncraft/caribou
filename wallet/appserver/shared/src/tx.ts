import t from './intl'

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
