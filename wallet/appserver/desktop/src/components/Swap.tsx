import { useRef, useState, ReactNode } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { explorerTxURL } from '@caribou/shared/coinexplorers'
import Doc from '@caribou/shared/doc'
import AssetLogo from './AssetLogo'
import Spinner from './Spinner'
import PasswordInput from './PasswordInput'
import { useModalBusy } from './modalBusy'
import {
  IntentsAsset, IntentsSwap,
  StateSuccess, StateRefunded, StateFailed, StateExpired, StateDepositFailed,
  stateLabel, stateColorClass, assetTicker, assetName
} from '@caribou/shared/intents'
import {
  slippagePresets, slippagePct, bridgeTrust, chainTicker, custodyDisclosure, screeningDisclosure,
  unitInfoFor, usdValue, availableAtoms, displayBalance, swapPickList, fetchWalletTx, swapProgressInfo
} from '@caribou/shared/swap'
import { useSwapForm } from '@caribou/shared/useSwapForm'
import { useSwapRecord, useDepositTx } from '@caribou/shared/useSwapProgress'

interface SwapProps {
  // depositAddress opens the dialog directly on an existing swap's progress
  // view (e.g. from an Activity row).
  depositAddress?: string
}

type View = 'form' | 'pick' | 'review' | 'progress'

export default function Swap ({ depositAddress }: SwapProps) {
  const close = () => app.setForm({ form: '', data: {} })

  const [view, setView] = useState<View>(depositAddress ? 'progress' : 'form')
  const [showSlippage, setShowSlippage] = useState(false)
  const [showCustody, setShowCustody] = useState(false)

  // Asset picker.
  const [pickSide, setPickSide] = useState<'sell' | 'buy'>('sell')
  const [query, setQuery] = useState('')

  const amtInputRef = useRef<HTMLInputElement>(null)
  const buyInputRef = useRef<HTMLInputElement>(null)

  // The swap on display in the progress view: loaded by deposit address, or
  // installed by a successful execute.
  const { swap, setSwap, refresh: refreshSwap } = useSwapRecord(depositAddress)

  const {
    assets, assetsErr, sellID, buyID, sellAsset, buyAsset, sellAvail, buyAvail,
    exactOutput, slippageBps, setSlippageBps,
    preview, previewErr, previewing, depositFeeText,
    requirePW, pw, setPW, executing, execErr, setExecErr, canExecute,
    sellUSD, buyUSD, sellAssetValue, buyAssetValue, sellUSDValue, buyUSDValue,
    sellFieldText, buyFieldText, rateText, minOutText, maxInText, etaText, feeBps,
    overBalance, formStatus,
    fetchPreview, flip, pick: pickFor, setMax, amountChanged, execute
  } = useSwapForm({
    liveView: view === 'form' || view === 'review' ? view : null,
    onExecuted: s => {
      setSwap(s)
      setView('progress')
    }
  })

  useModalBusy(executing)

  // ---- Actions ----

  const openPicker = (side: 'sell' | 'buy') => {
    setPickSide(side)
    setQuery('')
    setView('pick')
  }

  const pick = (a: IntentsAsset) => {
    pickFor(pickSide, a)
    setView('form')
  }

  // ---- Unavailable / loading states ----

  if (assetsErr && !swap) {
    return (
      <form className="modal swap-modal">
        <header><span>{t('Swap')}</span></header>
        <div className="px-3 pb-3 grey text-center">{assetsErr}</div>
        <div className="flex-stretch-column p-3">
          <button type="button" onClick={close}>{t('Close')}</button>
        </div>
      </form>
    )
  }

  if (assets === null && !swap) {
    return (
      <form className="modal swap-modal">
        <header><span>{t('Swap')}</span></header>
        <div className="flex-center py-5"><Spinner size={24} /></div>
      </form>
    )
  }

  // ---- Progress view ----

  if (view === 'progress' && swap) {
    return <SwapProgress swap={swap} onRefresh={refreshSwap} onClose={close} />
  }
  if (view === 'progress') { // opened by depositAddress, record still loading
    return (
      <form className="modal swap-modal">
        <header><span>{t('Swap')}</span></header>
        <div className="flex-center py-5"><Spinner size={24} /></div>
      </form>
    )
  }

  // ---- Asset picker view ----

  if (view === 'pick') {
    const list = swapPickList(assets ?? [], query)
    const selectedID = pickSide === 'sell' ? sellID : buyID
    return (
      <form className="modal swap-modal">
        <header>
          <span className="ico-arrow-left fs16 pointer me-2" onClick={() => setView('form')} />
          <span>{pickSide === 'sell' ? t('Sell which asset?') : t('Buy which asset?')}</span>
        </header>
        <div className="px-3 pb-2">
          <div className="d-flex align-items-center form-input-bg br5">
            <span className="ico-search fs16 p-2" />
            <input
              type="text"
              value={query}
              placeholder={t('search')}
              onChange={e => setQuery(e.target.value)}
              autoFocus
            />
          </div>
        </div>
        <div className="swap-pick-list overflow-y-auto mb-2">
          {list.map(a => {
            const ticker = assetTicker(a)
            const avail = availableAtoms(a.assetID)
            const conv = avail / Math.pow(10, a.atomDecimals)
            const usd = usdValue(a, conv)
            return (
              <div
                key={a.assetID}
                className={`d-flex align-items-center px-3 py-2 ${a.walletReady ? 'hoverbg pointer' : 'swap-pick-inactive'}`}
                onClick={() => { if (a.walletReady) pick(a) }}
              >
                <AssetLogo assetID={a.assetID} ticker={ticker} className="icon-24 me-2" />
                <div className="flex-grow-1">
                  <div className="fs14 demi">
                    {ticker}
                    {a.assetID === selectedID && <span className="ico-check fs11 ms-2 grey" />}
                  </div>
                  <div className="grey fs12">{assetName(a)}</div>
                </div>
                {a.walletReady
                  ? (
                    <div className="text-right">
                      <div className="fs14 mono">{avail > 0 ? Doc.formatCoinValue(avail, unitInfoFor(a)) : ''}</div>
                      <div className="grey fs12">{avail > 0 ? Doc.formatUSD(usd) : ''}</div>
                    </div>
                    )
                  : <span className="grey fs12">{t('wallet inactive')}</span>}
              </div>
            )
          })}
          {list.length === 0 && (
            <div className="px-3 py-2 grey fs14 text-center">{t('No matching assets.')}</div>
          )}
        </div>
      </form>
    )
  }

  // ---- Review view ----

  if (view === 'review' && sellAsset && buyAsset) {
    return (
      <form className="modal swap-modal" onSubmit={e => { e.preventDefault(); execute() }}>
        <header>
          <span className="ico-arrow-left fs16 pointer me-2" onClick={() => { if (!executing) setView('form') }} />
          <span>{t('Review swap')}</span>
        </header>

        <div className="px-3 pb-2">
          <div className="d-flex align-items-center py-2">
            <AssetLogo assetID={sellAsset.assetID} ticker={assetTicker(sellAsset)} className="icon-32 me-3" />
            <div className="flex-grow-1">
              <div className="grey fs12">{exactOutput ? t('Sell (estimated)') : t('Sell')}</div>
              <div className="fs18 demi mono">
                {preview ? `${sellFieldText} ${assetTicker(sellAsset)}` : <Spinner size={16} />}
              </div>
            </div>
            <div className="grey fs13">{sellUSD}</div>
          </div>
          <div className="d-flex align-items-center py-2">
            <AssetLogo assetID={buyAsset.assetID} ticker={assetTicker(buyAsset)} className="icon-32 me-3" />
            <div className="flex-grow-1">
              <div className="grey fs12">{exactOutput ? t('Buy') : t('Buy (estimated)')}</div>
              <div className="fs18 demi mono">
                {preview ? `${buyFieldText} ${assetTicker(buyAsset)}` : <Spinner size={16} />}
              </div>
            </div>
            <div className="grey fs13">{buyUSD}</div>
          </div>
        </div>

        <div className="px-3 pb-2 fs13">
          <div className="d-flex justify-content-between py-1">
            <span className="grey">{t('Rate')}</span>
            <span className="mono">{rateText || '—'}</span>
          </div>
          {exactOutput
            ? (
              <div className="d-flex justify-content-between py-1">
                <span className="grey has-tip" aria-label={t('The swap is rejected before any funds move if it would cost more than this.')}>
                  {t('Maximum sent')}
                </span>
                <span className="mono">{maxInText || '—'}</span>
              </div>
              )
            : (
              <div className="d-flex justify-content-between py-1">
                <span className="grey has-tip" aria-label={t('The swap is rejected before any funds move if it would deliver less than this.')}>
                  {t('Minimum received')}
                </span>
                <span className="mono">{minOutText || '—'}</span>
              </div>
              )}
          <div className="d-flex justify-content-between py-1">
            <span className="grey">{t('Max slippage')}</span>
            <span>{slippagePct(slippageBps)}</span>
          </div>
          <div className="d-flex justify-content-between py-1">
            <span className="grey has-tip" aria-label={t('Estimated time from deposit to delivery.')}>{t('Settlement time')}</span>
            <span>{etaText || '—'}</span>
          </div>
          <div className="d-flex justify-content-between py-1">
            <span className="grey has-tip" aria-label={t('Projected fee for the deposit transaction, paid from the wallet in addition to the amount sold.')}>{t('Network fee')}</span>
            <span className="mono">{depositFeeText || '—'}</span>
          </div>
          {feeBps > 0 && (
            <div className="d-flex justify-content-between py-1">
              <span className="grey has-tip" aria-label={t('Fee paid to the swap service, included in the quoted output.')}>{t('Service fee')}</span>
              <span>{slippagePct(feeBps)}</span>
            </div>
          )}
          <div className="d-flex justify-content-between py-1">
            <span className="grey">{t('Custody')}</span>
            <button
              type="button"
              className="d-flex align-items-center pointer p-0 border-0 fs13"
              onClick={() => setShowCustody(!showCustody)}
              aria-label={t('Custody and trust details')}
              aria-expanded={showCustody}
            >
              {t('Service-held in transit')}
              <span className="ico-info fs13 ms-1 grey" />
            </button>
          </div>
          {showCustody && (
            <div className="form-input-bg br5 p-2 my-1 fs12">
              <div className="grey mb-2">{custodyDisclosure()}</div>
              <div className="mb-1"><span className="demi">{chainTicker(sellAsset)}:</span> {bridgeTrust(chainTicker(sellAsset))}.</div>
              <div className="mb-2"><span className="demi">{chainTicker(buyAsset)}:</span> {bridgeTrust(chainTicker(buyAsset))}.</div>
              <div className="grey">{screeningDisclosure()}</div>
            </div>
          )}
          <div className="grey fs11 py-1 d-flex align-items-center">
            {previewing ? <Spinner size={10} className="me-1" /> : <span className="ico-check fs10 me-1" />}
            {t('Quote refreshes automatically.')}
          </div>
        </div>

        {requirePW && (
          <div className="px-3 pb-2">
            <label className="fs12 grey">{t('App password')}</label>
            <PasswordInput value={pw} onChange={e => setPW(e.target.value)} disabled={executing} autoFocus />
          </div>
        )}

        {previewErr && <div role="alert" className="warncolor fs12 px-3 pb-1 text-center">{previewErr}</div>}
        {execErr && <div role="alert" className="errcolor fs12 px-3 pb-1 text-center">{execErr}</div>}

        <div className="flex-stretch-column p-3">
          <button type="submit" className="feature swap-cta" disabled={!canExecute}>
            {executing
              ? <span className="d-inline-flex align-items-center justify-content-center"><Spinner className="me-2" />{t('Swapping...')}</span>
              : t('Swap')}
          </button>
          <button type="button" className="mt-2" onClick={() => setView('form')} disabled={executing}>{t('Back')}</button>
        </div>
      </form>
    )
  }

  // ---- Form view ----

  const actionButton = () => {
    switch (formStatus) {
      case 'select':
        return <button type="button" className="feature swap-cta" disabled>{t('Select assets')}</button>
      case 'amount':
        return <button type="button" className="feature swap-cta" disabled>{t('Enter an amount')}</button>
      case 'balance':
        return (
          <button type="button" className="feature swap-cta" disabled>
            {tvar('Not enough {{ ticker }}', { ticker: sellAsset ? assetTicker(sellAsset) : '' })}
          </button>
        )
      case 'retry':
        return <button type="button" className="feature swap-cta" onClick={fetchPreview}>{t('Retry quote')}</button>
      case 'quoting':
        return (
          <button type="button" className="feature swap-cta" disabled>
            <span className="d-inline-flex align-items-center justify-content-center"><Spinner className="me-2" />{t('Fetching quote...')}</span>
          </button>
        )
      case 'ready':
        return <button type="button" className="feature swap-cta" onClick={() => { setExecErr(''); setView('review') }}>{t('Review swap')}</button>
    }
  }

  const assetChip = (a: IntentsAsset | null, side: 'sell' | 'buy') => (
    <button type="button" className="swap-chip d-flex align-items-center pointer" onClick={() => openPicker(side)}>
      {a
        ? <><AssetLogo assetID={a.assetID} ticker={assetTicker(a)} className="icon-32 me-2" /><span className="fs18 demi">{assetTicker(a)}</span></>
        : <span className="fs16">{t('Select asset')}</span>}
      <span className="ico-arrow-down fs12 ms-2 grey" />
    </button>
  )

  return (
    <form className="modal swap-modal" onSubmit={e => e.preventDefault()}>
      <header className="position-relative">
        <span>{t('Swap')}</span>
        <button
          type="button"
          className="swap-gear flex-center pointer"
          onClick={() => setShowSlippage(!showSlippage)}
          aria-label={t('Slippage settings')}
        >
          <span className="ico-gear fs16" />
        </button>
      </header>

      {showSlippage && (
        <div className="swap-slip-pop d-flex align-items-center fs13">
          <span className="grey me-2">{t('Max slippage')}</span>
          <span>
            {slippagePresets.map(bps => (
              <button
                key={bps}
                type="button"
                className={`filter-pill fs12 grey ms-1 ${bps === slippageBps ? 'selected' : ''}`}
                onClick={() => setSlippageBps(bps)}
              >
                {slippagePct(bps)}
              </button>
            ))}
          </span>
        </div>
      )}

      <div className="px-3">
        <div>
          {/* Sell card: value story on the left (label, amount, USD),
              asset story on the right (chip with balance/Max beneath). */}
          <div className="swap-card p-3 d-flex" onClick={() => amtInputRef.current?.focus()}>
            <div className="swap-card-values flex-grow-1">
              <div className="section-label fs11 grey demi pb-1">{t('Sell')}</div>
              <input
                ref={amtInputRef}
                type="text"
                inputMode="decimal"
                className="swap-amt-input mono"
                value={sellAssetValue}
                placeholder="0.0"
                onChange={amountChanged('sell', 'asset')}
                autoFocus
              />
              <div className="swap-usd-line d-flex align-items-center grey fs12 pt-1">
                <span>$</span>
                <input
                  type="text"
                  inputMode="decimal"
                  className="swap-usd-input"
                  value={sellUSDValue}
                  placeholder="0.00"
                  onChange={amountChanged('sell', 'usd')}
                  onClick={e => e.stopPropagation()}
                />
              </div>
            </div>
            <div className="d-flex flex-column align-items-end justify-content-between ms-2">
              {assetChip(sellAsset, 'sell')}
              {sellAsset && (
                <span className={`swap-balance pointer fs12 pt-1 ${overBalance ? 'errcolor' : 'grey'}`} onClick={setMax}>
                  {tvar('Balance: {{ amt }}', { amt: displayBalance(sellAvail / Math.pow(10, sellAsset.atomDecimals)) })}
                  {sellAvail > 0 && <span className="swap-max-link ms-1">{t('Max')}</span>}
                </span>
              )}
            </div>
          </div>

          {/* Direction-flip control, floating on the seam between the
              cards; its body-bg border notches it out of both. */}
          <div className="swap-flip-anchor flex-center">
            <button type="button" className="swap-flip flex-center pointer" onClick={flip} aria-label={t('Swap direction')}>
              <span className="ico-swap rot90 fs16" />
            </button>
          </div>

          {/* Buy card, mirroring the sell card's two columns. */}
          <div className="swap-card p-3 mt-1 d-flex" onClick={() => buyInputRef.current?.focus()}>
            <div className="swap-card-values flex-grow-1">
              <div className="section-label fs11 grey demi pb-1">{t('Buy')}</div>
              <input
                ref={buyInputRef}
                type="text"
                inputMode="decimal"
                className="swap-amt-input mono"
                value={buyAssetValue}
                placeholder="0.0"
                onChange={amountChanged('buy', 'asset')}
              />
              <div className="swap-usd-line d-flex align-items-center grey fs12 pt-1">
                <span>$</span>
                <input
                  type="text"
                  inputMode="decimal"
                  className="swap-usd-input"
                  value={buyUSDValue}
                  placeholder="0.00"
                  onChange={amountChanged('buy', 'usd')}
                  onClick={e => e.stopPropagation()}
                />
              </div>
            </div>
            <div className="d-flex flex-column align-items-end justify-content-between ms-2">
              {assetChip(buyAsset, 'buy')}
              {buyAsset && (
                <span className="swap-balance grey fs12 pt-1">
                  {tvar('Balance: {{ amt }}', { amt: displayBalance(buyAvail / Math.pow(10, buyAsset.atomDecimals)) })}
                </span>
              )}
            </div>
          </div>
        </div>

        {/* Quote details, always visible: the rate rests on the
            indicative display-rate ratio, everything else on em-dashes
            until a quote fills them in. */}
        <div className="fs13 pt-2">
          <div className="d-flex align-items-center justify-content-between py-1">
            <span className="grey">{t('Rate')}</span>
            <span className="d-flex align-items-center">
              {previewing && <Spinner size={10} className="me-2" />}
              <span className="mono">{rateText || '—'}</span>
            </span>
          </div>
          <div className="d-flex justify-content-between py-1">
            <span className="grey has-tip" aria-label={t('Estimated time from deposit to delivery.')}>
              {t('Settlement time')}
            </span>
            <span>{etaText || '—'}</span>
          </div>
          {exactOutput
            ? (
              <div className="d-flex justify-content-between py-1">
                <span className="grey has-tip" aria-label={t('The swap is rejected before any funds move if it would cost more than this.')}>
                  {t('Maximum sent')}
                </span>
                <span className="mono">{maxInText || '—'}</span>
              </div>
              )
            : (
              <div className="d-flex justify-content-between py-1">
                <span className="grey has-tip" aria-label={t('The swap is rejected before any funds move if it would deliver less than this.')}>
                  {t('Minimum received')}
                </span>
                <span className="mono">{minOutText || '—'}</span>
              </div>
              )}
          <div className="d-flex justify-content-between py-1">
            <span className="grey has-tip" aria-label={t('Projected fee for the deposit transaction, paid from the wallet in addition to the amount sold.')}>{t('Network fee')}</span>
            <span className="mono">{depositFeeText || '—'}</span>
          </div>
          <div className="d-flex justify-content-between py-1">
            <span className="grey has-tip" aria-label={t('Fee paid to the swap service, included in the quoted output.')}>{t('Service fee')}</span>
            <span>{feeBps > 0 ? slippagePct(feeBps) : '—'}</span>
          </div>
        </div>
        {previewErr && <div role="alert" className="warncolor fs12 pt-2 text-center">{previewErr}</div>}
      </div>

      <div className="flex-stretch-column px-3 pb-3 pt-2">
        {actionButton()}
      </div>
    </form>
  )
}

// ---- Progress subview ----

// Row is the bordered label/value row shared with the transaction detail
// dialog.
function Row ({ label, tip, children }: { label: string, tip?: string, children: ReactNode }) {
  return (
    <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
      {tip
        ? <span className="grey fs13 has-tip" aria-label={tip}>{label}</span>
        : <span className="grey fs13">{label}</span>}
      <span className="text-right">{children}</span>
    </div>
  )
}

interface SwapProgressProps {
  swap: IntentsSwap
  onRefresh: () => void
  onClose: () => void
}

function SwapProgress ({ swap, onRefresh, onClose }: SwapProgressProps) {
  // The deposit leg, for its network fee and rejection status.
  const depositTx = useDepositTx(swap.originAssetID, swap.depositTxID)
  const {
    active, depositRejected, step, originTicker, destTicker, amountIn, amountOut, usdIn, usdOut,
    rateText, feeBps, refundedText, refundedUSD, feeAsset, feeUSD
  } = swapProgressInfo(swap, depositTx)

  // One shared "Copied!" flash keyed by the copied string.
  const [copiedID, setCopiedID] = useState('')
  const copy = async (v: string) => {
    try {
      await navigator.clipboard.writeText(v)
      setCopiedID(v)
      window.setTimeout(() => setCopiedID(c => (c === v ? '' : c)), 1500)
    } catch { /* clipboard unavailable */ }
  }

  const statusIcon = () => {
    if (depositRejected) return <span className="ico-close fs16 errcolor" />
    if (active) return <Spinner size={16} />
    switch (swap.state) {
      case StateSuccess: return <span className="ico-check fs16 tx-amt-pos" />
      case StateFailed:
      case StateDepositFailed: return <span className="ico-close fs16 errcolor" />
      default: return <span className="ico-info fs16 grey" />
    }
  }

  // openLeg swaps this dialog for the leg transaction's detail dialog.
  const openLeg = async (assetID: number, txID: string) => {
    const tx = await fetchWalletTx(assetID, txID)
    if (tx) app.setForm({ form: 'txdetail', data: { assetID, tx } })
  }

  // idActions renders the copy control, and a block-explorer link when one
  // exists for the asset.
  const idActions = (id: string, assetID: number) => {
    const explorer = explorerTxURL(assetID, id)
    return (
      <>
        <span
          className={`swap-id-action ms-2 pointer fs12 ${copiedID === id ? 'tx-amt-pos' : 'grey'}`}
          onClick={e => { e.stopPropagation(); copy(id) }}
        >
          {copiedID === id ? t('Copied!') : <span className="ico-copy" />}
        </span>
        {explorer && (
          <a
            href={explorer} target="_blank" rel="noopener noreferrer"
            className="swap-id-action ms-2 fs12" aria-label={t('View in block explorer')}
            onClick={e => e.stopPropagation()}
          >
            <span className="ico-link" />
          </a>
        )}
      </>
    )
  }

  // A transaction-leg row: click opens the leg's own detail dialog.
  const txRow = (label: string, id: string, assetID: number, failed?: boolean) => (
    <Row key={label + id} label={label} tip={t('Click to view the transaction.')}>
      <span className="d-flex align-items-center pointer" onClick={() => openLeg(assetID, id)}>
        {failed && <span className="errcolor fs11 me-2">{t('failed')}</span>}
        <span className="mono fs13 has-tip" aria-label={id}>{Doc.shortenAddress(id)}</span>
        {idActions(id, assetID)}
      </span>
    </Row>
  )

  const steps = [t('Deposit'), t('Swapping'), t('Delivered')]

  const started = new Date(swap.createdAt)
  const updated = new Date(swap.updatedAt)
  const deadline = new Date(swap.deadline)

  return (
    <form className="modal swap-modal" onSubmit={e => e.preventDefault()}>
      <header><span>{t('Swap details')}</span></header>

      <div className="flex-center px-3 pt-1">
        {statusIcon()}
        <span className={`fs16 demi ms-2 ${depositRejected ? 'errcolor' : stateColorClass(swap.state)}`}>
          {depositRejected ? t('Deposit failed') : stateLabel(swap.state)}
        </span>
        {active && !depositRejected && swap.timeEstimate > 0 && (
          <span className="grey fs12 ms-2">{tvar('usually ~{{ eta }}', { eta: Doc.formatDuration(swap.timeEstimate * 1000) })}</span>
        )}
        {swap.reconstructed && <span className="swap-recovered fs11 grey ms-2">{t('recovered')}</span>}
      </div>

      {active && !depositRejected && (
        <div className="flex-center px-3 pt-3">
          {steps.map((label, i) => (
            <div key={label} className="d-flex align-items-center">
              {i > 0 && <div className={`swap-step-bar ${i <= step ? 'done' : ''}`} />}
              <div className="d-flex flex-column align-items-center">
                <div className={`swap-step-dot ${i < step ? 'done' : i === step ? 'current' : ''}`} />
                <div className={`fs11 pt-1 ${i === step ? '' : 'grey'}`}>{label}</div>
              </div>
            </div>
          ))}
        </div>
      )}

      <div className="flex-center px-3 py-3">
        <div className="d-flex align-items-center">
          <AssetLogo assetID={swap.originAssetID} ticker={originTicker} className="icon-32 me-2" />
          <div>
            <div className="mono demi fs18">{amountIn} {originTicker}</div>
            <div className="grey fs12">{usdIn}</div>
          </div>
        </div>
        <span className="ico-arrow-right fs14 grey mx-3" />
        <div className="d-flex align-items-center">
          <AssetLogo assetID={swap.destAssetID} ticker={destTicker} className="icon-32 me-2" />
          <div>
            <div className="mono demi fs18">
              {swap.state === StateSuccess ? '' : '~'}{amountOut} {destTicker}
            </div>
            <div className="grey fs12">{usdOut}</div>
          </div>
        </div>
      </div>

      {swap.state === StateRefunded && (
        <div className="px-3 pb-2 fs13 grey text-center">
          {swap.refundReason
            ? tvar('Deposit refunded: {{ reason }}', { reason: swap.refundReason })
            : t('The deposit was refunded to the origin wallet.')}
        </div>
      )}
      {swap.state === StateFailed && (
        <div className="px-3 pb-2 fs13 grey text-center">
          {swap.refundReason
            ? tvar('The swap did not complete: {{ reason }}.', { reason: swap.refundReason })
            : t('The swap did not complete.')}{' '}
          {t('If funds left the origin wallet and no refund arrives, contact support with the deposit address below.')}
        </div>
      )}
      {swap.state === StateExpired && (
        <div className="px-3 pb-2 fs13 grey text-center">{t('The quote expired before the deposit was sent. No funds moved.')}</div>
      )}
      {/* The engine marks the record DEPOSIT_FAILED on its next poll, but the
          deposit tx fetched for the fee row can prove the rejection first —
          say so immediately rather than showing a healthy in-flight swap. */}
      {(swap.state === StateDepositFailed || depositRejected) && (
        <div className="px-3 pb-2 fs13 errcolor text-center">
          {t('The deposit transaction was rejected on-chain. The deposited funds never left the wallet, though the network fee was spent.')}
        </div>
      )}

      <div className="px-3">
        {rateText !== '' && <Row label={t('Rate')}><span className="mono fs13">{rateText}</span></Row>}
        {swap.depositTxID && (
          <Row label={t('Network fee')} tip={t("The deposit transaction's fee on the origin chain.")}>
            {depositTx === undefined
              ? <Spinner size={12} />
              : depositTx === null || !feeAsset || depositTx.fees === 0
                // A zero fee means the wallet did not record one (the UTXO
                // wallets' history omits fees today), not a free ride.
                ? '—'
                : (
                  <>
                    <span className="mono">{Doc.formatCoinValue(depositTx.fees, feeAsset.unitInfo)} {feeAsset.unitInfo.conventional.unit}</span>
                    {feeUSD !== '' && <span className="grey fs12 ms-1">({feeUSD})</span>}
                  </>
                  )}
          </Row>
        )}
        {feeBps > 0 && (
          <Row label={t('Service fee')} tip={t('Fee paid to the swap service, included in the quoted output.')}>
            {slippagePct(feeBps)}
          </Row>
        )}
        {swap.state === StateRefunded && refundedText !== '' && (
          <Row label={t('Refunded')} tip={t("The amount returned to the origin wallet, net of the service's refund fee.")}>
            <span className="mono">{refundedText}</span>
            {refundedUSD !== '' && <span className="grey fs12 ms-1">({refundedUSD})</span>}
          </Row>
        )}
        <Row label={t('Started')}>{isNaN(started.getTime()) ? '—' : started.toLocaleString()}</Row>
        {!active && (
          <Row label={t('Last update')}>{isNaN(updated.getTime()) ? '—' : updated.toLocaleString()}</Row>
        )}
        {active && !isNaN(deadline.getTime()) && (
          <Row label={t('Refund deadline')} tip={t('If the swap has not completed by the deadline, the deposit is refunded automatically.')}>
            {deadline.toLocaleString()}
          </Row>
        )}
        <Row label={t('Receiving address')} tip={t('The destination-wallet address receiving the funds.')}>
          <span className="d-flex align-items-center">
            <span className="mono fs13 has-tip" aria-label={swap.recipient}>{Doc.shortenAddress(swap.recipient)}</span>
            <span
              className={`swap-id-action ms-2 pointer fs12 ${copiedID === swap.recipient ? 'tx-amt-pos' : 'grey'}`}
              onClick={() => copy(swap.recipient)}
            >
              {copiedID === swap.recipient ? t('Copied!') : <span className="ico-copy" />}
            </span>
          </span>
        </Row>
        <Row label={t('Deposit address')}>
          <span className="d-flex align-items-center">
            <span className="mono fs13 has-tip" aria-label={swap.depositAddress}>{Doc.shortenAddress(swap.depositAddress)}</span>
            <span
              className={`swap-id-action ms-2 pointer fs12 ${copiedID === swap.depositAddress ? 'tx-amt-pos' : 'grey'}`}
              onClick={() => copy(swap.depositAddress)}
            >
              {copiedID === swap.depositAddress ? t('Copied!') : <span className="ico-copy" />}
            </span>
          </span>
        </Row>
        {swap.depositTxID && txRow(t('Deposit tx'), swap.depositTxID, swap.originAssetID,
          swap.state === StateDepositFailed || depositTx?.rejected)}
        {(swap.destinationTxs ?? []).map(id => txRow(t('Received tx'), id, swap.destAssetID))}
        {(swap.refundTxs ?? []).map(id => txRow(t('Refund tx'), id, swap.originAssetID))}
      </div>

      {active && (
        <div className="grey fs11 px-3 pt-2 text-center">
          {t('Safe to close — the swap continues in the background and shows under Activity.')}
        </div>
      )}

      <div className="flex-stretch-column px-3 pb-3 pt-2">
        {active && <button type="button" onClick={onRefresh}>{t('Refresh status')}</button>}
        <button type="button" className={active ? 'mt-2' : ''} onClick={onClose}>{t('Close')}</button>
      </div>
    </form>
  )
}
