import { ReactNode, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import Doc from '@caribou/shared/doc'
import { explorerTxURL } from '@caribou/shared/coinexplorers'
import { useCoreSubscriptions, useFlash, useForceUpdate } from '@caribou/shared/hooks'
import {
  StateSuccess, StateRefunded, StateFailed, StateExpired, StateDepositFailed,
  stateLabel, stateColorClass
} from '@caribou/shared/intents'
import { slippagePct, swapProgressInfo } from '@caribou/shared/swap'
import { useSwapRecord, useDepositTx } from '@caribou/shared/useSwapProgress'
import { canOpenExternal, copyText } from '../bridge'
import { useNav } from '../nav'
import { ExternalLink, Icon, NavHeader, Spinner, Toast } from './ui'
import { SwapLogo } from './Swap'

// Row is a label/value row in the details list.
function Row ({ label, children }: { label: string, children: ReactNode }) {
  return (
    <div className='kv'>
      <span>{label}</span>
      <span>{children}</span>
    </div>
  )
}

// SwapProgressScreen follows one cross-chain swap by its deposit address:
// state, progress steps, and the record's details. It updates live from the
// 'intentsswap' note feed and offers a manual status refresh while the swap
// is active.
export default function SwapProgressScreen ({ depositAddress }: { depositAddress: string }) {
  const nav = useNav()
  const redraw = useForceUpdate()
  // USD figures read app fiat rates directly.
  useCoreSubscriptions({ onRate: redraw }, [])
  const { swap, loaded, loadErr, refresh } = useSwapRecord(depositAddress)
  const depositTx = useDepositTx(swap?.originAssetID, swap?.depositTxID)
  const [copied, flashCopied] = useFlash(1500)
  const [refreshing, setRefreshing] = useState(false)

  const header = <NavHeader title={t('Swap details')} onBack={nav.back} close />

  if (!swap) {
    return (
      <div className='frame'>
        {header}
        <div className='scroll'>
          <div className='center-col'>
            {loaded ? <p className='grey'>{loadErr || t('Swap not found.')}</p> : <Spinner large />}
          </div>
        </div>
        <div className='foot'>
          <button type='button' className='btn' onClick={nav.back}>{t('Done')}</button>
        </div>
      </div>
    )
  }

  const {
    active, depositRejected, step, originTicker, destTicker, amountIn, amountOut,
    rateText, feeBps, refundedText, refundedUSD, feeAsset, feeUSD
  } = swapProgressInfo(swap, depositTx)

  const copy = async (v: string) => {
    if (await copyText(v)) flashCopied()
  }

  const doRefresh = async () => {
    setRefreshing(true)
    await refresh()
    setRefreshing(false)
  }

  const copyButton = (v: string, label: string) => (
    <button type='button' className='swap-cp' onClick={() => copy(v)} aria-label={label}>
      <Icon name='copy' />
    </button>
  )

  // A transaction-leg row: the shortened txid, linked to a block explorer
  // where one exists for the asset, with a copy control.
  const txRow = (label: string, id: string, assetID: number, failed?: boolean) => {
    const url = explorerTxURL(assetID, id)
    const short = Doc.shortenAddress(id, 6)
    return (
      <Row key={label + id} label={label}>
        <span className='swap-id'>
          {failed && <span className='swap-failed'>{t('failed')}</span>}
          {url && canOpenExternal()
            ? (
              <ExternalLink className='swap-txlink' href={url} title={id}>
                <span className='mono'>{short}</span>
                <Icon name='link' />
              </ExternalLink>
              )
            : <span className='mono' title={id}>{short}</span>}
          {copyButton(id, t('Copy transaction ID'))}
        </span>
      </Row>
    )
  }

  const stateIcon = () => {
    if (depositRejected) return <Icon name='close' className='err' />
    if (active) return null
    switch (swap.state) {
      case StateSuccess: return <Icon name='check' className='pos' />
      case StateFailed:
      case StateDepositFailed: return <Icon name='close' className='err' />
      default: return null
    }
  }

  // A healthy in-flight swap reads in the body color; outcomes take the
  // state's tone.
  const stateTone = depositRejected ? 'errcolor' : active ? '' : stateColorClass(swap.state)

  const steps = [t('Deposit'), t('Swapping'), t('Delivered')]

  const started = new Date(swap.createdAt)
  const updated = new Date(swap.updatedAt)
  const deadline = new Date(swap.deadline)
  const when = (d: Date) => isNaN(d.getTime())
    ? '—'
    : d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })

  return (
    <div className='frame'>
      {header}
      <div className='scroll'>
        <div className='body'>
          <section className='swap-hero'>
            <div className='swap-pair'>
              <SwapLogo assetID={swap.originAssetID} ticker={originTicker} className='swap-logo-lg' />
              <span className='swap-pair-ring'>
                <SwapLogo assetID={swap.destAssetID} ticker={destTicker} className='swap-logo-lg' />
              </span>
            </div>
            <h2 className={`swap-state ${stateTone}`}>
              {stateIcon()}
              {depositRejected ? t('Deposit failed') : stateLabel(swap.state)}
            </h2>
            {active && !depositRejected && swap.timeEstimate > 0 && (
              <div className='swap-eta'>{tvar('usually ~{{ eta }}', { eta: Doc.formatDuration(swap.timeEstimate * 1000) })}</div>
            )}
            <div className='swap-amts mono'>
              {amountIn} {originTicker} <span className='grey'>→</span> {swap.state === StateSuccess ? '' : '~'}{amountOut} {destTicker}
            </div>
            {swap.reconstructed && <span className='swap-recovered'>{t('recovered')}</span>}
          </section>

          {active && !depositRejected && (
            <ol className='swap-steps' aria-label={t('Progress')}>
              {steps.map((label, i) => (
                <StepItem key={label} label={label} index={i} step={step} />
              ))}
            </ol>
          )}

          {swap.state === StateRefunded && (
            <div className='callout swap-outcome'>
              {swap.refundReason
                ? tvar('Deposit refunded: {{ reason }}', { reason: swap.refundReason })
                : t('The deposit was refunded to the origin wallet.')}
            </div>
          )}
          {swap.state === StateFailed && (
            <div className='callout swap-outcome'>
              <span>
                {swap.refundReason
                  ? tvar('The swap did not complete: {{ reason }}.', { reason: swap.refundReason })
                  : t('The swap did not complete.')}{' '}
                {t('If funds left the origin wallet and no refund arrives, contact support with the deposit address below.')}
              </span>
            </div>
          )}
          {swap.state === StateExpired && (
            <div className='callout swap-outcome'>{t('The quote expired before the deposit was sent. No funds moved.')}</div>
          )}
          {/* The engine marks the record DEPOSIT_FAILED on its next poll, but
              the fetched deposit tx can prove the rejection first. */}
          {(swap.state === StateDepositFailed || depositRejected) && (
            <div className='err-box swap-outcome'>
              {t('The deposit transaction was rejected on-chain. The deposited funds never left the wallet, though the network fee was spent.')}
            </div>
          )}

          <div className='list'>
            {rateText !== '' && <Row label={t('Rate')}><span className='mono'>{rateText}</span></Row>}
            {swap.depositTxID && (
              <Row label={t('Network fee')}>
                {depositTx === undefined
                  ? <Spinner small />
                  : depositTx === null || !feeAsset || depositTx.fees === 0
                    // A zero fee means the wallet did not record one (the
                    // UTXO wallets' history omits fees today), not a free ride.
                    ? '—'
                    : (
                      <>
                        <span className='mono'>{Doc.formatCoinValue(depositTx.fees, feeAsset.unitInfo)} {feeAsset.unitInfo.conventional.unit}</span>
                        {feeUSD !== '' && <span className='grey'> ({feeUSD})</span>}
                      </>
                      )}
              </Row>
            )}
            {feeBps > 0 && <Row label={t('Service fee')}>{slippagePct(feeBps)}</Row>}
            {swap.state === StateRefunded && refundedText !== '' && (
              <Row label={t('Refunded')}>
                <span className='mono'>{refundedText}</span>
                {refundedUSD !== '' && <span className='grey'> ({refundedUSD})</span>}
              </Row>
            )}
            <Row label={t('Started')}>{when(started)}</Row>
            {!active && <Row label={t('Last update')}>{when(updated)}</Row>}
            {active && !isNaN(deadline.getTime()) && <Row label={t('Refund deadline')}>{when(deadline)}</Row>}
            <Row label={t('Receiving address')}>
              <span className='swap-id'>
                <span className='mono' title={swap.recipient}>{Doc.shortenAddress(swap.recipient, 6)}</span>
                {copyButton(swap.recipient, t('Copy receiving address'))}
              </span>
            </Row>
            <Row label={t('Deposit address')}>
              <span className='swap-id'>
                <span className='mono' title={swap.depositAddress}>{Doc.shortenAddress(swap.depositAddress, 6)}</span>
                {copyButton(swap.depositAddress, t('Copy deposit address'))}
              </span>
            </Row>
            {swap.depositTxID && txRow(t('Deposit tx'), swap.depositTxID, swap.originAssetID,
              swap.state === StateDepositFailed || depositTx?.rejected)}
            {(swap.destinationTxs ?? []).map(id => txRow(t('Received tx'), id, swap.destAssetID))}
            {(swap.refundTxs ?? []).map(id => txRow(t('Refund tx'), id, swap.originAssetID))}
          </div>

          {active && (
            <p className='swap-safe'>{t('Safe to close. The swap continues in the background and shows under Activity.')}</p>
          )}
        </div>
      </div>
      <div className='foot'>
        {active && (
          <button type='button' className='btn quiet' onClick={doRefresh} disabled={refreshing}>
            {refreshing && <Spinner small />}
            {t('Refresh status')}
          </button>
        )}
        <button type='button' className='btn' onClick={nav.back}>{t('Done')}</button>
      </div>
      {copied && <Toast>{t('Copied')}</Toast>}
    </div>
  )
}

// StepItem is one step of the active-swap stepper, preceded by its
// connecting bar after the first.
function StepItem ({ label, index, step }: { label: string, index: number, step: number }) {
  const state = index < step ? 'done' : index === step ? 'current' : ''
  return (
    <>
      {index > 0 && <li className={index <= step ? 'swap-step-bar done' : 'swap-step-bar'} aria-hidden='true' />}
      <li className={`swap-step ${state}`} aria-current={index === step ? 'step' : undefined}>
        <span className='swap-step-dot'>{index < step && <Icon name='check' />}</span>
        {label}
      </li>
    </>
  )
}
