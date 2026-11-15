import { MouseEvent } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { TxRowItem, txTitle, txIcon, txStatus, txIsIncoming, txShowsAmount, txAmountSign } from '@caribou/shared/tx'

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

  // A rejected transaction was mined but had no effect (only fees were
  // spent), so no amount moved in either direction.
  const rejected = tx.rejected === true
  const sign = txAmountSign(tx.type)
  const amountText = !rejected && txShowsAmount(tx.type)
    ? `${sign}${Doc.formatCoinValue(tx.amount, variant.ui)} ${variant.ticker}`
    : '—'
  const rate = app.tickerRate(ta.ticker) ?? 0
  const usdText = !rejected && txShowsAmount(tx.type) && rate > 0
    ? Doc.usdString(tx.amount, variant.ui.conventional.conversionFactor, rate)
    : ''
  const parts: string[] = []
  if (tx.recipient) parts.push(`${t('to')} ${Doc.shortenAddress(tx.recipient)}`)
  parts.push(txStatus(tx))
  const unconfirmed = !tx.confirmed

  return (
    <div
      className={`activity-row d-flex align-items-center pointer mb-2 ${unconfirmed ? 'activity-row-unconfirmed' : ''} ${swapHighlight ? 'swap-hover' : ''}`}
      role="button"
      tabIndex={0}
      onClick={open}
      onKeyDown={(e) => {
        // A key pressed on the nested swap-leg badge belongs to the badge:
        // its keydown bubbles here, and preventing default would cancel the
        // badge's own activation and open the transaction instead.
        if (e.target !== e.currentTarget) return
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open() }
      }}
      onMouseEnter={() => onSwapHover?.(swapRelation?.swap.depositAddress ?? '')}
      onMouseLeave={() => onSwapHover?.('')}
    >
      <span className={`tx-type-icon icon-32 fs14 flex-center me-3 ${txIcon(tx.type)}`} />
      <div className="flex-grow-1">
        <div className="fs14 demi d-flex align-items-center">
          {txTitle(tx, variant.ticker)}
          {swapRelation && (
            <button
              type="button"
              className="swap-leg-badge ico-swap fs10 ms-2 p-0 has-tip"
              aria-label={t('Part of a cross-chain swap — click for details')}
              onClick={openSwap}
            />
          )}
        </div>
        <div className="grey fs12 mono">
          {rejected && <span className="errcolor">{t('failed')} · </span>}
          {parts.join(' · ')}
        </div>
      </div>
      <div className="text-right">
        <div className={`fs14 mono ${txIsIncoming(tx.type) ? 'tx-amt-pos' : ''}`}>{amountText}</div>
        {usdText !== '' && <div className="grey fs12 mono">{usdText}</div>}
      </div>
    </div>
  )
}
