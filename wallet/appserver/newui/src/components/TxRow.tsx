import { MouseEvent } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { WalletTransaction } from '@caribou/shared/registry'
import { TickerAsset, AssetVariant } from '@caribou/shared/assets'
import { TxType, txTypeLabel, txIsIncoming, txShowsAmount, txAmountSign } from '@caribou/shared/tx'
import { SwapRelation } from '../intents'

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
function txTitle (tx: WalletTransaction, ticker: string): string {
  switch (tx.type) {
    case TxType.Send: return `${t('Sent')} ${ticker}`
    case TxType.Receive: return `${t('Received')} ${ticker}`
    case TxType.Swap: return `${t('Swapped')} ${ticker}`
    default: return `${txTypeLabel(tx.type)} ${ticker}`
  }
}

function txIcon (type: number): string {
  if (type === TxType.Swap || type === TxType.SwapOrSend) return 'ico-swap'
  if (txIsIncoming(type)) return 'ico-download'
  return 'ico-upload'
}

// txStatus renders confirmation progress while confirming, then age.
function txStatus (tx: WalletTransaction): string {
  if (!tx.confirmed && tx.confirms) {
    return tvar('{{ current }}/{{ target }} confs', {
      current: String(tx.confirms.current),
      target: String(tx.confirms.target)
    })
  }
  if (tx.timestamp > 0) return tvar('{{ age }} ago', { age: Doc.timeSince(tx.timestamp * 1000) })
  return t('pending')
}

// TxRow is the transaction row shared by the Activity feed and the Asset
// view: type icon, title with recipient/status sub-line, and amount with a
// USD sub-line when a rate is known. Clicking opens the transaction detail.
interface TxRowProps {
  item: TxRowItem
  // onSwapHover reports the deposit address of the hovered row's swap ('' on
  // leave or for unrelated rows); swapHighlight lights the row up alongside
  // a hovered sibling serving the same swap.
  onSwapHover?: (depositAddress: string) => void
  swapHighlight?: boolean
}

export default function TxRow ({ item, onSwapHover, swapHighlight }: TxRowProps) {
  const { tx, ta, variant, swapRelation } = item
  const open = () => app.setForm({ form: 'txdetail', data: { assetID: variant.assetID, tx } })
  const openSwap = (e: MouseEvent) => {
    e.stopPropagation()
    if (swapRelation) app.setForm({ form: 'swap', data: { depositAddress: swapRelation.swap.depositAddress } })
  }

  const sign = txAmountSign(tx.type)
  const amountText = txShowsAmount(tx.type)
    ? `${sign}${Doc.formatCoinValue(tx.amount, variant.ui)} ${variant.ticker}`
    : '—'
  const rate = app.tickerRate(ta.ticker) ?? 0
  const usdText = txShowsAmount(tx.type) && rate > 0
    ? Doc.usdString(tx.amount, variant.ui.conventional.conversionFactor, rate)
    : ''
  const parts: string[] = []
  if (tx.recipient) parts.push(`${t('to')} ${Doc.shortenAddress(tx.recipient)}`)
  parts.push(txStatus(tx))
  const unconfirmed = !tx.confirmed

  return (
    <div
      className={`activity-row d-flex align-items-center pointer mb-2 ${unconfirmed ? 'activity-row-unconfirmed' : ''} ${swapHighlight ? 'swap-hover' : ''}`}
      onClick={open}
      onMouseEnter={() => onSwapHover?.(swapRelation?.swap.depositAddress ?? '')}
      onMouseLeave={() => onSwapHover?.('')}
    >
      <span className={`tx-type-icon icon-32 fs14 flex-center me-3 ${txIcon(tx.type)}`} />
      <div className="flex-grow-1">
        <div className="fs14 demi d-flex align-items-center">
          {txTitle(tx, variant.ticker)}
          {swapRelation && (
            <span
              className="swap-leg-badge ico-swap fs10 ms-2 pointer has-tip"
              aria-label={t('Part of a cross-chain swap — click for details')}
              onClick={openSwap}
            />
          )}
        </div>
        <div className="grey fs12 mono">{parts.join(' · ')}</div>
      </div>
      <div className="text-right">
        <div className={`fs14 mono ${txIsIncoming(tx.type) ? 'tx-amt-pos' : ''}`}>{amountText}</div>
        {usdText !== '' && <div className="grey fs12 mono">{usdText}</div>}
      </div>
    </div>
  )
}
