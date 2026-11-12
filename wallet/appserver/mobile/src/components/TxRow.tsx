import { useEffect, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { useFlash } from '@caribou/shared/hooks'
import { WalletTransaction } from '@caribou/shared/registry'
import {
  TxRowItem, txTitle, txIcon, txStatus, txIsIncoming, txShowsAmount, txAmountSign
} from '@caribou/shared/tx'
import { explorerTxURL } from '@caribou/shared/coinexplorers'
import {
  IntentsSwap, SwapRelation, StateSuccess, StateRefunded, StateExpired, StateDepositFailed,
  swapIsActive, stateLabel, stateColorClass, fetchSwaps, relateTxToSwaps, swapRelationLabel
} from '@caribou/shared/intents'
import { swapTime } from '@caribou/shared/activity'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import { useNav } from '../nav'
import { canOpenExternal, copyText } from '../bridge'
import { ExternalLink, Icon, Sheet, Spinner } from './ui'

// Mobile renders a minus sign rather than a hyphen for outgoing amounts.
function signOf (txType: number): string {
  const s = txAmountSign(txType)
  return s === '-' ? '−' : s
}

interface TxRowProps {
  item: TxRowItem
  onOpen: (item: TxRowItem) => void
  // showNetwork prefixes the status line with the network name, for feeds
  // that mix networks.
  showNetwork?: boolean
}

// TxRow is a transaction card: type icon, title with recipient and status,
// and the amount with its USD value. Unconfirmed transactions are drawn with
// a dashed outline.
export function TxRow ({ item, onOpen, showNetwork }: TxRowProps) {
  const nav = useNav()
  const { tx, ta, variant, swapRelation } = item
  // A rejected transaction was mined but had no effect (only fees were
  // spent), so no amount moved in either direction.
  const rejected = tx.rejected === true
  const shows = !rejected && txShowsAmount(tx.type)
  const amountText = shows ? `${signOf(tx.type)}${Doc.formatCoinValue(tx.amount, variant.ui)} ${variant.ticker}` : '—'
  const rate = app.tickerRate(ta.ticker) ?? 0
  const usdText = shows && rate > 0 ? Doc.usdString(tx.amount, variant.ui.conventional.conversionFactor, rate) : ''
  const parts: string[] = []
  if (showNetwork && ta.variants.length > 1) {
    const netID = variant.token ? variant.token.networkID : variant.assetID
    const name = app.user?.assets[netID]?.name
    if (name) parts.push(name)
  }
  // An incoming transaction's recipient is this wallet's own address.
  if (tx.recipient && !txIsIncoming(tx.type)) parts.push(`${t('to')} ${Doc.shortenAddress(tx.recipient)}`)
  parts.push(txStatus(tx))

  return (
    <div className={tx.confirmed ? 'txr' : 'txr unc'} role='button' tabIndex={0} onClick={() => onOpen(item)}>
      <span className='txi'><span className={txIcon(tx.type)} aria-hidden='true' /></span>
      <div className='grow'>
        <div className='tt'>
          {txTitle(tx, variant.ticker)}
          {swapRelation && (
            <button
              type='button'
              className='swap-badge'
              aria-label={t('Part of a cross-chain swap')}
              onClick={e => { e.stopPropagation(); nav.push({ kind: 'swapProgress', depositAddress: swapRelation.swap.depositAddress }) }}
            >
              <Icon name='swap' />
            </button>
          )}
        </div>
        <div className='ts mono ellipsis'>
          {rejected && <span className='err'>{t('failed')} · </span>}
          {parts.join(' · ')}
        </div>
      </div>
      <div className='num'>
        <div className={txIsIncoming(tx.type) && shows ? 'ta mono pos' : 'ta mono'}>{amountText}</div>
        {usdText && <div className='ts mono'>{usdText}</div>}
      </div>
    </div>
  )
}

// swapAmounts formats a swap's legs for display.
function swapLegs (swap: IntentsSwap) {
  const originAsset = app.user?.assets[swap.originAssetID]
  const destAsset = app.user?.assets[swap.destAssetID]
  return {
    originTicker: originAsset?.unitInfo.conventional.unit ?? swap.originAsset,
    destTicker: destAsset?.unitInfo.conventional.unit ?? swap.destAsset,
    amountIn: originAsset ? Doc.formatCoinValue(swap.atomsIn, originAsset.unitInfo) : swap.amountIn,
    amountOut: destAsset ? Doc.formatCoinValue(swap.atomsOut, destAsset.unitInfo) : swap.amountOut,
    originAsset,
    destAsset
  }
}

// SwapLogos overlaps the origin and destination coin logos.
function SwapLogos ({ swap }: { swap: IntentsSwap }) {
  const { originTicker, destTicker } = swapLegs(swap)
  return (
    <span className='swap-logos' aria-hidden='true'>
      <CoinLogo ticker={originTicker} alt='' className='sl-a' />
      <span className='sl-b'><CoinLogo ticker={destTicker} alt='' /></span>
    </span>
  )
}

// SwapRow is a cross-chain swap's card in the Activity feed. An in-flight
// swap carries a progress strip. Tapping opens the swap details.
export function SwapRow ({ swap }: { swap: IntentsSwap }) {
  const nav = useNav()
  const active = swapIsActive(swap)
  const { originTicker, destTicker, amountIn, amountOut, originAsset } = swapLegs(swap)
  const age = tvar('{{ age }} ago', { age: Doc.timeSince(swapTime(swap) * 1000) })

  // The amount column reflects the outcome: the received (or expected)
  // destination amount for successful and in-flight swaps, the amount
  // returned for refunds, nothing for an expired quote or rejected deposit,
  // and an em dash for failures.
  let amounts
  if (swap.state === StateSuccess || active) {
    const approx = swap.state === StateSuccess ? '' : '~'
    amounts = (
      <>
        <div className='ta mono'>−{amountIn} {originTicker}</div>
        <div className='ts mono pos'>+{approx}{amountOut} {destTicker}</div>
      </>
    )
  } else if (swap.state === StateRefunded) {
    const returnedAtoms = swap.refundedAtoms || swap.atomsIn
    const returned = originAsset ? Doc.formatCoinValue(returnedAtoms, originAsset.unitInfo) : (swap.refundedAmount || swap.amountIn)
    amounts = <div className='ta mono grey'>{tvar('{{ amt }} {{ ticker }} returned', { amt: returned, ticker: originTicker })}</div>
  } else if (swap.state === StateExpired || swap.state === StateDepositFailed) {
    amounts = <div className='ta mono grey'>—</div>
  } else {
    amounts = (
      <>
        <div className='ta mono grey'>—</div>
        <div className='ts mono'>−{amountIn} {originTicker}</div>
      </>
    )
  }

  // In-flight progress: elapsed time against the service's estimate, capped
  // short of full until the swap completes.
  let pct = 0
  if (active) {
    const elapsed = Date.now() / 1000 - swapTime(swap)
    const est = swap.timeEstimate > 0 ? swap.timeEstimate : 180
    pct = Math.min(92, Math.max(8, elapsed / est * 100))
  }

  return (
    <div
      className={active ? 'swapr active' : 'txr'}
      role='button'
      tabIndex={0}
      onClick={() => nav.push({ kind: 'swapProgress', depositAddress: swap.depositAddress })}
    >
      <div className='swapr-main'>
        <SwapLogos swap={swap} />
        <div className='grow'>
          <div className='tt'>{tvar('Swap {{ from }} → {{ to }}', { from: originTicker, to: destTicker })}</div>
          <div className='ts swapr-status'>
            {active && <Spinner small />}
            <span className={active ? 'accent' : stateColorClass(swap.state)}>{stateLabel(swap.state)}</span>
            <span>·</span>
            <span>{age}</span>
            {swap.reconstructed && <><span>·</span><span>{t('recovered')}</span></>}
          </div>
        </div>
        <div className='num'>{amounts}</div>
      </div>
      {active && <div className='bar accent flat'><span style={{ width: `${pct}%` }} /></div>}
    </div>
  )
}

// TxDetailSheet shows a single transaction: amount, status with confirmation
// progress, network, date, fee, recipient, cross-chain swap leg, the txid with
// a copy control, and a block explorer link.
export function TxDetailSheet ({ assetID, tx, onClose }: { assetID: number, tx: WalletTransaction, onClose: () => void }) {
  const nav = useNav()
  const [copied, flashCopied] = useFlash(1500)
  const [swapRelation, setSwapRelation] = useState<SwapRelation | undefined>()
  useEffect(() => {
    let stale = false
    fetchSwaps().then(swaps => { if (!stale) setSwapRelation(relateTxToSwaps(swaps, tx)) })
    return () => { stale = true }
  }, [tx])

  const asset = app.asset(assetID)
  const ui = asset.unitInfo
  const ticker = ui.conventional.unit
  // Fees are paid in the settlement-network asset: the parent network for
  // tokens, the asset itself for natives.
  const feeAssetID = asset.token ? asset.token.networkID : assetID
  const feeUI = app.unitInfo(feeAssetID)
  const network = asset.token ? app.asset(asset.token.networkID) : asset
  const rate = app.idRate(assetID) ?? 0
  const feeRate = app.idRate(feeAssetID) ?? 0
  const shows = txShowsAmount(tx.type) && !tx.rejected
  // An incoming transaction pays no fee from this wallet, and its recipient
  // is this wallet's own address, so neither row applies.
  const incoming = txIsIncoming(tx.type)
  const amountUsd = rate > 0 ? Doc.usdString(tx.amount, ui.conventional.conversionFactor, rate) : ''
  const feeUsd = feeRate > 0 ? Doc.usdString(tx.fees, feeUI.conventional.conversionFactor, feeRate) : ''
  const explorer = explorerTxURL(assetID, tx.id)
  const dateStr = tx.timestamp > 0
    ? new Date(tx.timestamp * 1000).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
    : t('Pending')

  let status
  if (tx.rejected) {
    status = <span className='err'>{t('Failed: no funds moved, network fee spent')}</span>
  } else if (!tx.confirmed && tx.confirms) {
    const { current, target } = tx.confirms
    status = (
      <span className='status-confs'>
        <span className='bar'><span style={{ width: `${Math.min(100, current / Math.max(1, target) * 100)}%` }} /></span>
        <span className='mono gold'>{tvar('{{ current }}/{{ target }} confs', { current: String(current), target: String(target) })}</span>
      </span>
    )
  } else {
    status = tx.confirmed ? <span className='pos'>{t('Confirmed')}</span> : <span className='gold'>{t('Pending')}</span>
  }

  const copy = async () => { if (await copyText(tx.id)) flashCopied() }

  return (
    <Sheet
      label={t('Transaction details')}
      onClose={onClose}
      foot={explorer && canOpenExternal()
        ? (
          <ExternalLink className='btn secondary' href={explorer}>
            {t('View in block explorer')}<Icon name='chevron-left' className='mirror fs12' />
          </ExternalLink>
          )
        : undefined}
    >
      <div className='txd-hero'>
        <span className='txi lg'><span className={txIcon(tx.type)} aria-hidden='true' /></span>
        <h1>{txTitle(tx, ticker)}</h1>
        {shows && (
          <>
            <div className={txIsIncoming(tx.type) ? 'txd-amt mono pos' : 'txd-amt mono'}>
              {signOf(tx.type)}{Doc.formatCoinValue(tx.amount, ui)} {ticker}
            </div>
            {amountUsd && <div className='grey fs15 mt4'>{amountUsd}</div>}
          </>
        )}
      </div>
      <div className='list'>
        <div className='kv'><span>{t('Status')}</span>{status}</div>
        <div className='kv'>
          <span>{t('Network')}</span>
          <span className='kv-logo'><CoinLogo ticker={network.unitInfo.conventional.unit} alt='' className='coin-xs' />{network.name}</span>
        </div>
        <div className='kv'><span>{t('Date')}</span><span>{dateStr}</span></div>
        {(tx.fees > 0 || !incoming) && (
          <div className='kv'>
            <span>{t('Network fee')}</span>
            <span className='mono'>
              {Doc.formatCoinValue(tx.fees, feeUI)} {feeUI.conventional.unit}
              {feeUsd && <><br /><span className='grey'>{feeUsd}</span></>}
            </span>
          </div>
        )}
        {tx.recipient && !incoming && (
          <div className='kv stack'>
            <span>{t('Recipient')}</span>
            <span className='mono fs14 break-all'>{tx.recipient}</span>
          </div>
        )}
        {swapRelation && (
          <button
            type='button'
            className='kv tappable'
            onClick={() => { onClose(); nav.push({ kind: 'swapProgress', depositAddress: swapRelation.swap.depositAddress }) }}
          >
            <span>{t('Cross-chain swap')}</span>
            <span className='accent'>
              {tvar('{{ role }} leg of {{ from }} → {{ to }}', {
                role: swapRelationLabel(swapRelation.role),
                from: app.user?.assets[swapRelation.swap.originAssetID]?.unitInfo.conventional.unit ?? swapRelation.swap.originAsset,
                to: app.user?.assets[swapRelation.swap.destAssetID]?.unitInfo.conventional.unit ?? swapRelation.swap.destAsset
              })}
              <Icon name='chevron-left' className='mirror fs12 ms4' />
            </span>
          </button>
        )}
        <div className='kv stack'>
          <span>{t('Transaction ID')}</span>
          <span className='txid-line'>
            <span className='mono fs14 break-all grow'>{tx.id}</span>
            <button type='button' className='ib tile' onClick={copy} aria-label={t('Copy transaction ID')}>
              <Icon name={copied ? 'check' : 'copy'} />
            </button>
          </span>
        </div>
      </div>
    </Sheet>
  )
}
