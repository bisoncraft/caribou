import { ReactNode, useRef, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import NetworkedTokenLogo from '@caribou/shared/components/NetworkedTokenLogo'
import { IntentsAsset, assetTicker, assetName } from '@caribou/shared/intents'
import {
  slippagePresets, slippagePct, bridgeTrust, chainTicker, custodyDisclosure, screeningDisclosure,
  unitInfoFor, usdValue, availableAtoms, displayBalance, swapPickList
} from '@caribou/shared/swap'
import { useSwapForm, SwapSide } from '@caribou/shared/useSwapForm'
import { seedSwapRecord } from '@caribou/shared/useSwapProgress'
import { useNav } from '../nav'
import { Icon, NavHeader, PasswordField, RefreshIcon, Sheet, SlideToConfirm, Spinner, useNow } from './ui'

interface SwapLogoProps {
  assetID: number
  ticker: string
  className: string
}

// SwapLogo renders an asset's logo by wallet asset ID: a token gets its
// settlement network's logo as a badge, a chain native (or an asset unknown
// to this build) the plain coin logo. className sizes it.
export function SwapLogo ({ assetID, ticker, className }: SwapLogoProps) {
  const sa = app.user?.assets[assetID]
  const network = sa?.token ? app.user?.assets[sa.token.networkID]?.unitInfo.conventional.unit : undefined
  if (!network) return <CoinLogo ticker={ticker} className={className} alt='' />
  return <NetworkedTokenLogo token={ticker} network={network} className={className} />
}

// Explanations behind the dotted row labels.
const tips = {
  minOut: () => t('The swap is rejected before any funds move if it would deliver less than this.'),
  maxIn: () => t('The swap is rejected before any funds move if it would cost more than this.'),
  eta: () => t('Estimated time from deposit to delivery.'),
  netFee: () => t('Projected fee for the deposit transaction, paid from the wallet in addition to the amount sold.'),
  svcFee: () => t('Fee paid to the swap service, included in the quoted output.')
}

type TipKey = keyof typeof tips

interface QuoteRowProps {
  label: string
  tip?: TipKey
  openTip: TipKey | null
  onTip: (k: TipKey | null) => void
  children: ReactNode
}

// QuoteRow is a label/value row. A row with a tip has a dotted label that
// toggles a short explanation beneath it.
function QuoteRow ({ label, tip, openTip, onTip, children }: QuoteRowProps) {
  const open = tip !== undefined && openTip === tip
  return (
    <>
      <div className='kv'>
        {tip
          ? <button type='button' className='swap-dl' aria-expanded={open} onClick={() => onTip(open ? null : tip)}>{label}</button>
          : <span>{label}</span>}
        <span className='swap-val'>{children}</span>
      </div>
      {open && tip && <p className='swap-tip'>{tips[tip]()}</p>}
    </>
  )
}

// QuoteCountdown counts down to the next scheduled quote refresh.
function QuoteCountdown ({ at, busy }: { at: number, busy: boolean }) {
  const now = useNow(500)
  if (busy) return <>{t('Refreshing quote...')}</>
  if (at === 0) return null
  const secs = Math.max(0, Math.ceil((at - now) / 1000))
  return <>{tvar('Quote refreshes in {{ n }} s', { n: String(secs) })}</>
}

// amtSizeClass steps the amount font down for long entries.
function amtSizeClass (v: string): string {
  if (v.length > 13) return ' xlong'
  if (v.length > 9) return ' long'
  return ''
}

type SheetKind = 'pick' | 'slippage' | 'review'

// Swap is the full-screen cross-chain swap flow: sell and buy cards over a
// live quote, with the asset picker, slippage presets, and the review in
// bottom sheets. The shared useSwapForm hook owns quoting and execution; on
// success the progress screen replaces this one.
export default function Swap ({ ticker }: { ticker?: string }) {
  const nav = useNav()
  const [sheet, setSheet] = useState<SheetKind | null>(null)
  const [pickSide, setPickSide] = useState<SwapSide>('sell')
  const [query, setQuery] = useState('')
  const [focus, setFocus] = useState<SwapSide | null>(null)
  const [openTip, setOpenTip] = useState<TipKey | null>(null)
  const sellRef = useRef<HTMLInputElement>(null)
  const buyRef = useRef<HTMLInputElement>(null)

  const {
    assets, assetsErr, sellID, buyID, sellAsset, buyAsset, sellAvail, buyAvail,
    exactOutput, editAtoms, slippageBps, setSlippageBps,
    preview, previewErr, previewing, nextRefreshAt, depositFeeText,
    requirePW, pw, setPW, executing, execErr, setExecErr, canExecute,
    sellUSD, buyUSD, sellAssetValue, buyAssetValue, sellFieldText, buyFieldText,
    rateText, minOutText, maxInText, etaText, feeBps, overBalance, formStatus,
    fetchPreview, flip, pick, setMax, amountChanged, execute
  } = useSwapForm({
    // The picker suspends live quoting, as the desktop picker view does.
    liveView: sheet === 'pick' ? null : sheet === 'review' ? 'review' : 'form',
    ticker,
    onExecuted: s => {
      seedSwapRecord(s)
      nav.replace({ kind: 'swapProgress', depositAddress: s.depositAddress })
    }
  })

  const closeSheet = () => setSheet(null)

  const header = (right?: ReactNode) => <NavHeader title={t('Swap')} onBack={nav.back} close right={right} />

  // ---- Unavailable / loading states ----

  if (assetsErr) {
    return (
      <div className='frame'>
        {header()}
        <div className='scroll'>
          <div className='center-col'>
            <p className='grey'>{assetsErr}</p>
          </div>
        </div>
        <div className='foot'>
          <button type='button' className='btn secondary' onClick={nav.back}>{t('Close')}</button>
        </div>
      </div>
    )
  }

  if (assets === null) {
    return (
      <div className='frame'>
        {header()}
        <div className='scroll'>
          <div className='center-col'><Spinner large /></div>
        </div>
      </div>
    )
  }

  // ---- Form ----

  const openPicker = (side: SwapSide) => {
    setPickSide(side)
    setQuery('')
    setSheet('pick')
  }

  const choose = (a: IntentsAsset) => {
    pick(pickSide, a)
    closeSheet()
  }

  const card = (side: SwapSide) => {
    const sell = side === 'sell'
    const a = sell ? sellAsset : buyAsset
    const value = sell ? sellAssetValue : buyAssetValue
    const usd = sell ? sellUSD : buyUSD
    const avail = sell ? sellAvail : buyAvail
    const ref = sell ? sellRef : buyRef
    const cls = `swap-card${sell ? '' : ' buy'}${focus === side ? ' focus' : ''}`
    return (
      <div className={cls} onClick={() => ref.current?.focus()}>
        <span className='lbl'>{sell ? t('Sell') : t('Buy')}</span>
        <div className='swap-card-mid'>
          <input
            ref={ref}
            type='text'
            inputMode='decimal'
            autoComplete='off'
            className={`swap-amt mono${amtSizeClass(value)}`}
            value={value}
            placeholder='0'
            aria-label={sell ? t('Amount to sell') : t('Amount to buy')}
            onChange={amountChanged(side, 'asset')}
            onFocus={() => setFocus(side)}
            onBlur={() => setFocus(f => (f === side ? null : f))}
          />
          <button
            type='button'
            className={a ? 'swap-chip' : 'swap-chip empty'}
            onClick={e => { e.stopPropagation(); openPicker(side) }}
            aria-label={sell ? t('Change asset to sell') : t('Change asset to buy')}
          >
            {a
              ? <><SwapLogo assetID={a.assetID} ticker={assetTicker(a)} className='swap-logo-sm' />{assetTicker(a)}</>
              : t('Select asset')}
            <Icon name='chevron-left' className='swap-chip-chev' />
          </button>
        </div>
        <div className='swap-card-foot'>
          <span>{usd}</span>
          {a && (
            <span className={sell && overBalance ? 'err' : undefined}>
              {tvar('Balance: {{ amt }}', { amt: displayBalance(avail / Math.pow(10, a.atomDecimals)) })}
              {sell && avail > 0 && (
                <button type='button' className='swap-max' onClick={e => { e.stopPropagation(); setMax() }}>{t('Max')}</button>
              )}
            </span>
          )}
        </div>
      </div>
    )
  }

  const action = () => {
    switch (formStatus) {
      case 'select':
        return <button type='button' className='btn' disabled>{t('Select assets')}</button>
      case 'amount':
        return <button type='button' className='btn' disabled>{t('Enter an amount')}</button>
      case 'balance':
        return (
          <button type='button' className='btn' disabled>
            {tvar('Not enough {{ ticker }}', { ticker: sellAsset ? assetTicker(sellAsset) : '' })}
          </button>
        )
      case 'retry':
        return <button type='button' className='btn' onClick={() => { fetchPreview() }}>{t('Retry quote')}</button>
      case 'quoting':
        return <button type='button' className='btn' disabled><Spinner small />{t('Fetching quote...')}</button>
      case 'ready':
        return <button type='button' className='btn' onClick={() => { setExecErr(''); setSheet('review') }}>{t('Review swap')}</button>
    }
  }

  const boundRow = (withTip: boolean) => exactOutput
    ? (
      <QuoteRow label={t('Maximum sent')} tip={withTip ? 'maxIn' : undefined} openTip={openTip} onTip={setOpenTip}>
        <span className='mono'>{maxInText || '—'}</span>
      </QuoteRow>
      )
    : (
      <QuoteRow label={t('Minimum received')} tip={withTip ? 'minOut' : undefined} openTip={openTip} onTip={setOpenTip}>
        <span className='mono'>{minOutText || '—'}</span>
      </QuoteRow>
      )

  // ---- Sheets ----

  let sheetEl: ReactNode = null
  if (sheet === 'pick') {
    const list = swapPickList(assets, query)
    const selectedID = pickSide === 'sell' ? sellID : buyID
    sheetEl = (
      <Sheet title={pickSide === 'sell' ? t('Sell which asset?') : t('Buy which asset?')} onClose={closeSheet} tall>
        <label className='field search'>
          <Icon name='search' />
          <input
            type='text'
            value={query}
            placeholder={t('search')}
            aria-label={t('search')}
            autoComplete='off'
            autoCapitalize='off'
            autoCorrect='off'
            spellCheck={false}
            onChange={e => setQuery(e.target.value)}
          />
        </label>
        {list.length > 0
          ? (
            <div className='list swap-pick'>
              {list.map(a => {
                const tk = assetTicker(a)
                const avail = availableAtoms(a.assetID)
                const conv = avail / Math.pow(10, a.atomDecimals)
                return (
                  <button key={a.assetID} type='button' className='row tappable' disabled={!a.walletReady} onClick={() => choose(a)}>
                    <SwapLogo assetID={a.assetID} ticker={tk} className='coin' />
                    <span className='grow'>
                      <span className='t1'>
                        {tk}
                        {a.assetID === selectedID && <Icon name='check' className='accent swap-pick-check' />}
                      </span>
                      <span className='t2'>{assetName(a)}</span>
                    </span>
                    {a.walletReady
                      ? (
                        <span className='num'>
                          <span className='mono'>{avail > 0 ? Doc.formatCoinValue(avail, unitInfoFor(a)) : ''}</span>
                          <span className='t2'>{avail > 0 ? Doc.formatUSD(usdValue(a, conv)) : ''}</span>
                        </span>
                        )
                      : <span className='t2'>{t('wallet inactive')}</span>}
                  </button>
                )
              })}
            </div>
            )
          : <div className='empty'>{t('No matching assets.')}</div>}
      </Sheet>
    )
  } else if (sheet === 'slippage') {
    sheetEl = (
      <Sheet title={t('Max slippage')} onClose={closeSheet}>
        <div className='swap-slip' role='radiogroup' aria-label={t('Max slippage')}>
          {slippagePresets.map(bps => (
            <button
              key={bps}
              type='button'
              role='radio'
              aria-checked={bps === slippageBps}
              className={bps === slippageBps ? 'on' : undefined}
              onClick={() => { setSlippageBps(bps); closeSheet() }}
            >
              {slippagePct(bps)}
            </button>
          ))}
        </div>
      </Sheet>
    )
  } else if (sheet === 'review' && sellAsset && buyAsset) {
    const sellChain = chainTicker(sellAsset)
    const buyChain = chainTicker(buyAsset)
    const foot = (
      <div className='swap-confirm'>
        {previewErr && <div role='alert' className='warn swap-msg'>{previewErr}</div>}
        {execErr && <div role='alert' className='err-box'>{execErr}</div>}
        {requirePW && (
          <PasswordField value={pw} onChange={setPW} placeholder={t('App password')} disabled={executing} />
        )}
        <SlideToConfirm
          label={t('Slide to swap')}
          busyLabel={t('Swapping...')}
          onCommit={execute}
          disabled={!canExecute}
          busy={executing}
        />
        <div className='swap-by' aria-live='polite'>
          <QuoteCountdown at={nextRefreshAt} busy={previewing} />
        </div>
      </div>
    )
    sheetEl = (
      <Sheet title={t('Review swap')} onClose={() => { if (!executing) closeSheet() }} tall foot={foot}>
        <div className='swap-sum'>
          <div className='swap-sum-row'>
            <SwapLogo assetID={sellAsset.assetID} ticker={assetTicker(sellAsset)} className='coin' />
            <div className='grow'>
              <div className='swap-sum-lbl'>{exactOutput ? t('Sell (estimated)') : t('Sell')}</div>
              <div className='swap-sum-amt mono'>{preview ? `${sellFieldText} ${assetTicker(sellAsset)}` : <Spinner small />}</div>
            </div>
            <span className='swap-sum-usd'>{sellUSD}</span>
          </div>
          <div className='swap-sum-sep' aria-hidden='true'>
            <span><Icon name='arrowdown' /></span>
            <i />
          </div>
          <div className='swap-sum-row'>
            <SwapLogo assetID={buyAsset.assetID} ticker={assetTicker(buyAsset)} className='coin' />
            <div className='grow'>
              <div className='swap-sum-lbl'>{exactOutput ? t('Buy') : t('Buy (estimated)')}</div>
              <div className='swap-sum-amt mono'>{preview ? `${buyFieldText} ${assetTicker(buyAsset)}` : <Spinner small />}</div>
            </div>
            <span className='swap-sum-usd'>{buyUSD}</span>
          </div>
        </div>

        <div className='kvs swap-rev'>
          <QuoteRow label={t('Rate')} openTip={openTip} onTip={setOpenTip}>
            <span className='mono'>{rateText || '—'}</span>
          </QuoteRow>
          {boundRow(false)}
          <QuoteRow label={t('Max slippage')} openTip={openTip} onTip={setOpenTip}>{slippagePct(slippageBps)}</QuoteRow>
          <QuoteRow label={t('Settlement time')} openTip={openTip} onTip={setOpenTip}>{etaText || '—'}</QuoteRow>
          <QuoteRow label={t('Network fee')} openTip={openTip} onTip={setOpenTip}>
            <span className='mono'>{depositFeeText || '—'}</span>
          </QuoteRow>
          {feeBps > 0 && (
            <QuoteRow label={t('Service fee')} openTip={openTip} onTip={setOpenTip}>{slippagePct(feeBps)}</QuoteRow>
          )}
          <QuoteRow label={t('Custody')} openTip={openTip} onTip={setOpenTip}>
            <span className='gold demi'>{t('Service-held in transit')}</span>
          </QuoteRow>
        </div>

        <div className='callout caution swap-custody'>
          <Icon name='info' />
          <div>
            <p>{custodyDisclosure()}</p>
            <p className='swap-trust'><span className='demi'>{sellChain}:</span> {bridgeTrust(sellChain)}.</p>
            {buyChain !== sellChain && (
              <p className='swap-trust'><span className='demi'>{buyChain}:</span> {bridgeTrust(buyChain)}.</p>
            )}
            <p>{screeningDisclosure()}</p>
          </div>
        </div>
      </Sheet>
    )
  }

  return (
    <div className='frame'>
      {header(
        <button type='button' className='ib grey' onClick={() => setSheet('slippage')} aria-label={t('Slippage settings')}>
          <Icon name='knobs' />
        </button>
      )}
      <div className='scroll'>
        <div className='body'>
          {card('sell')}
          <div className='swap-flip-anchor'>
            <button type='button' className='swap-flip' onClick={flip} aria-label={t('Swap direction')}>
              <Icon name='swap' />
            </button>
          </div>
          {card('buy')}

          <div className='kvs swap-quote'>
            <QuoteRow label={t('Rate')} openTip={openTip} onTip={setOpenTip}>
              {previewing && <Spinner small />}
              <span className='mono'>{rateText || '—'}</span>
            </QuoteRow>
            {boundRow(true)}
            <QuoteRow label={t('Settlement time')} tip='eta' openTip={openTip} onTip={setOpenTip}>{etaText || '—'}</QuoteRow>
            <QuoteRow label={t('Network fee')} tip='netFee' openTip={openTip} onTip={setOpenTip}>
              <span className='mono'>{depositFeeText || '—'}</span>
            </QuoteRow>
            <QuoteRow label={t('Service fee')} tip='svcFee' openTip={openTip} onTip={setOpenTip}>
              {feeBps > 0 ? slippagePct(feeBps) : '—'}
            </QuoteRow>
          </div>

          {editAtoms > 0 && (
            <div className='swap-refresh'>
              <span className={previewing ? 'spinning' : undefined}><RefreshIcon /></span>
              {t('Quote refreshes automatically.')}
            </div>
          )}
          {previewErr && <div role='alert' className='warn swap-msg'>{previewErr}</div>}
        </div>
      </div>
      <div className='foot'>
        {action()}
        <div className='swap-by'>{t('Cross-chain swaps by NEAR Intents')}</div>
      </div>
      {sheetEl}
    </div>
  )
}
