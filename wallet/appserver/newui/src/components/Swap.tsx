import { useEffect, useMemo, useRef, useState, ChangeEvent, ReactNode } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { explorerTxURL } from '@caribou/shared/coinexplorers'
import { useForceUpdate } from '@caribou/shared/hooks'
import { getJSON, postJSON } from '@caribou/shared/http'
import { CoreNote, UnitInfo, WalletTransaction } from '@caribou/shared/registry'
import State from '@caribou/shared/state'
import Doc from '@caribou/shared/doc'
import AssetLogo from './AssetLogo'
import Spinner from './Spinner'
import PasswordInput from './PasswordInput'
import { useModalBusy } from './modalBusy'
import {
  IntentsAsset, IntentsPreview, IntentsSwap,
  StateProcessing, StateSuccess, StateRefunded, StateFailed, StateExpired, StateDepositFailed,
  swapIsActive, stateLabel, stateColorClass,
  conventionalToAtoms, assetTicker, assetName
} from '../intents'

// localStorage keys for sticky swap-form preferences.
const pairLK = 'intentsPair'
const slippageLK = 'intentsSlippageBps'

const slippagePresets = [50, 100, 200] // basis points

// Preview quotes refresh on this cadence while the form or review view is
// showing, so displayed pricing tracks the market.
const previewRefreshMS = 10000
const previewDebounceMS = 350

function slippagePct (bps: number): string {
  return `${(bps / 100).toLocaleString()}%`
}

// unitInfoFor returns the wallet UnitInfo for a swappable asset, or a
// minimal stand-in built from the catalog scale when the asset is unknown
// to this build's wallet map (walletReady is false for those, so the
// stand-in only ever formats display amounts).
function unitInfoFor (a: IntentsAsset): UnitInfo {
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
function usdRate (a: IntentsAsset): number {
  return app.idRate(a.assetID) || a.priceUSD
}

// usdValue prices conventional units of a swappable asset.
function usdValue (a: IntentsAsset, conventional: number): number {
  return conventional * usdRate(a)
}

// plainAtoms renders atoms as a plain decimal string for use as an input
// value — no locale grouping, which would fail the decimal validation on
// the next keystroke.
function plainAtoms (atoms: number, decimals: number): string {
  if (atoms <= 0) return ''
  return (atoms / Math.pow(10, decimals)).toFixed(decimals).replace(/\.?0+$/, '')
}

// plainUSD renders a dollar value as a plain input string.
function plainUSD (v: number): string {
  if (v <= 0) return ''
  return v.toFixed(2)
}

// displayBalance floors a conventional balance to four significant digits
// (capped at eight decimals) so the line stays short and never overstates
// holdings. Max still spends full precision.
function displayBalance (v: number): string {
  if (v <= 0) return '0'
  const digits = Math.min(8, Math.max(0, 4 - Math.floor(Math.log10(v)) - 1))
  const f = Math.pow(10, digits)
  return (Math.floor(v * f) / f).toLocaleString(undefined, { maximumFractionDigits: digits })
}

interface SwapProps {
  // depositAddress opens the dialog directly on an existing swap's progress
  // view (e.g. from an Activity row).
  depositAddress?: string
}

type View = 'form' | 'pick' | 'review' | 'progress'

export default function Swap ({ depositAddress }: SwapProps) {
  const close = () => app.setForm({ form: '', data: {} })
  const redraw = useForceUpdate()

  const [view, setView] = useState<View>(depositAddress ? 'progress' : 'form')
  const [assets, setAssets] = useState<IntentsAsset[] | null>(null)
  const [assetsErr, setAssetsErr] = useState('')
  const [sellID, setSellID] = useState<number | null>(null)
  const [buyID, setBuyID] = useState<number | null>(null)
  const [amount, setAmount] = useState('')
  // The side and unit the typed amount belongs to. 'buy' quotes
  // exact-output: the service computes the required input for the
  // requested output. A 'usd' unit converts to the side's asset amount at
  // the display rate before quoting.
  const [editSide, setEditSide] = useState<'sell' | 'buy'>('sell')
  const [editUnit, setEditUnit] = useState<'asset' | 'usd'>('asset')
  const [slippageBps, setSlippageBps] = useState<number>(State.fetchLocal(slippageLK) || 100)
  const [showSlippage, setShowSlippage] = useState(false)

  // Asset picker.
  const [pickSide, setPickSide] = useState<'sell' | 'buy'>('sell')
  const [query, setQuery] = useState('')

  // Preview state. previewSeq orders requests: a response is applied only if
  // no newer request has been issued since, so a slow stale quote can never
  // overwrite a fresher one.
  const [preview, setPreview] = useState<IntentsPreview | null>(null)
  const [previewErr, setPreviewErr] = useState('')
  const [previewing, setPreviewing] = useState(false)
  const previewSeq = useRef(0)
  const amtInputRef = useRef<HTMLInputElement>(null)
  const buyInputRef = useRef<HTMLInputElement>(null)

  // Execution / progress.
  const [pw, setPW] = useState('')
  const [executing, setExecuting] = useState(false)
  const [execErr, setExecErr] = useState('')
  const [swap, setSwap] = useState<IntentsSwap | null>(null)

  useModalBusy(executing)

  const requirePW = app.appSettings.sendRequiresPassword

  const assetMap = useMemo(() => {
    const m = new Map<number, IntentsAsset>()
    for (const a of assets ?? []) m.set(a.assetID, a)
    return m
  }, [assets])

  const sellAsset = sellID === null ? null : assetMap.get(sellID) ?? null
  const buyAsset = buyID === null ? null : assetMap.get(buyID) ?? null

  const exactOutput = editSide === 'buy'
  const editAsset = exactOutput ? buyAsset : sellAsset

  // The typed amount in the edited side's wallet atoms — the preview and
  // execute `value` for either direction. A USD entry converts at the
  // display rate; the quote then prices the derived asset amount.
  const editAtoms = useMemo(() => {
    if (!editAsset) return 0
    const v = parseFloat(amount)
    if (!isFinite(v) || v <= 0) return 0
    if (editUnit === 'usd') {
      const rate = usdRate(editAsset)
      if (rate <= 0) return 0
      return conventionalToAtoms(v / rate, editAsset.atomDecimals)
    }
    return conventionalToAtoms(v, editAsset.atomDecimals)
  }, [amount, editAsset, editUnit])

  const sellAvail = sellID === null ? 0 : app.wallet(sellID)?.balance?.available ?? 0
  const buyAvail = buyID === null ? 0 : app.wallet(buyID)?.balance?.available ?? 0

  // Projected origin-chain network fee for the deposit transaction, in the
  // fee asset's atoms (the parent chain's for tokens). Refreshed on the
  // preview cadence while the form or review view is showing.
  const [depositFee, setDepositFee] = useState(0)
  const sellWalletAsset = sellID === null ? undefined : app.user?.assets[sellID]
  const depositFeeAssetID = sellWalletAsset?.token ? sellWalletAsset.token.networkID : sellID
  useEffect(() => {
    setDepositFee(0)
    if (sellID === null || (view !== 'form' && view !== 'review') || !app.wallet(sellID)) return
    let stale = false
    const fetchFee = async () => {
      const r = await postJSON('/api/feestate', { assetID: sellID })
      if (!stale && r.ok) setDepositFee(r.feeState?.send ?? 0)
    }
    fetchFee()
    const ticker = window.setInterval(fetchFee, previewRefreshMS)
    return () => { stale = true; window.clearInterval(ticker) }
  }, [sellID, view])
  const depositFeeAsset = depositFeeAssetID == null ? undefined : app.user?.assets[depositFeeAssetID]
  const depositFeeUSD = depositFeeAsset && depositFeeAssetID != null
    ? Doc.usdString(depositFee, depositFeeAsset.unitInfo.conventional.conversionFactor, app.idRate(depositFeeAssetID) ?? 0)
    : ''
  const depositFeeText = depositFee > 0 && depositFeeAsset
    ? `${Doc.formatCoinValue(depositFee, depositFeeAsset.unitInfo)} ${depositFeeAsset.unitInfo.conventional.unit}${depositFeeUSD !== '' ? ` (${depositFeeUSD})` : ''}`
    : ''

  // ---- Data loading ----

  useEffect(() => {
    const loadAssets = async () => {
      const r = await getJSON('/api/intents/assets')
      if (!r.ok) {
        setAssetsErr(r.msg || r.errMsg || t('Cross-chain swaps are unavailable.'))
        return
      }
      const list: IntentsAsset[] = r.assets ?? []
      setAssets(list)
      // Restore the last-used pair, falling back to the two most valuable
      // ready wallets.
      const ready = list.filter(a => a.walletReady)
      const byValue = [...ready].sort((a, b) => {
        const bal = (x: IntentsAsset) => usdValue(x, (app.wallet(x.assetID)?.balance?.available ?? 0) / Math.pow(10, x.atomDecimals))
        return bal(b) - bal(a)
      })
      const stored = State.fetchLocal(pairLK) as { sellID: number, buyID: number } | null
      let sell = stored && list.some(a => a.assetID === stored.sellID) ? stored.sellID : null
      let buy = stored && list.some(a => a.assetID === stored.buyID) ? stored.buyID : null
      if (sell === null) sell = byValue[0]?.assetID ?? null
      if (buy === null || buy === sell) buy = ready.find(a => a.assetID !== sell)?.assetID ?? null
      setSellID(sell)
      setBuyID(buy)
    }
    loadAssets()
  }, [])

  // Opened on an existing swap: pull the record by deposit address.
  useEffect(() => {
    if (!depositAddress) return
    const load = async () => {
      const r = await getJSON('/api/intents/swaps')
      if (!r.ok) return
      const s = (r.swaps as IntentsSwap[] ?? []).find(s => s.depositAddress === depositAddress)
      if (s) setSwap(s)
    }
    load()
  }, [depositAddress])

  // Live swap updates ride the notification feed; apply any that match the
  // record on display.
  useEffect(() => {
    const k = app.registerNoteFeed((n: CoreNote) => {
      if (n.type !== 'intentsswap') return
      const s = (n as CoreNote & { swap: IntentsSwap }).swap
      if (!s) return
      setSwap(prev => (prev && prev.depositAddress === s.depositAddress) ? s : prev)
    })
    return () => app.unregisterNoteFeed(k)
  }, [])

  // Balance and rate changes arrive as notes; the sell-card balance line
  // and USD estimates read app state directly, so re-render on either.
  useEffect(() => {
    const balK = app.registerBalanceUpdater(redraw)
    const rateK = app.registerFiatRateUpdater(redraw)
    return () => {
      app.unregisterBalanceUpdater(balK)
      app.unregisterFiatRateUpdater(rateK)
    }
  }, [])

  // Persist the pair and slippage choices.
  useEffect(() => {
    if (sellID !== null && buyID !== null) State.storeLocal(pairLK, { sellID, buyID })
  }, [sellID, buyID])
  useEffect(() => { State.storeLocal(slippageLK, slippageBps) }, [slippageBps])

  // ---- Preview scheduling ----

  const fetchPreview = async () => {
    if (sellID === null || buyID === null || editAtoms === 0) return
    const seq = ++previewSeq.current
    setPreviewing(true)
    const r = await postJSON('/api/intents/preview', {
      originAssetID: sellID,
      destAssetID: buyID,
      value: editAtoms,
      slippageBps,
      exactOutput
    })
    if (seq !== previewSeq.current) return // superseded
    setPreviewing(false)
    if (!r.ok) {
      setPreview(null)
      setPreviewErr(r.msg || r.errMsg || t('quote failed'))
      return
    }
    setPreviewErr('')
    setPreview(r.preview)
  }

  // On any parameter change: invalidate the current quote immediately (it
  // priced different terms), then fetch behind a short debounce. The cleanup
  // seq bump also retires any in-flight response.
  useEffect(() => {
    setPreview(null)
    setPreviewErr('')
    if (sellID === null || buyID === null || editAtoms === 0) {
      setPreviewing(false)
      return
    }
    const timer = window.setTimeout(fetchPreview, previewDebounceMS)
    return () => {
      window.clearTimeout(timer)
      previewSeq.current++
    }
  }, [sellID, buyID, editAtoms, slippageBps, exactOutput])

  // Periodic refresh keeps the displayed quote current in the form and
  // review views, including the review dialog's live totals.
  useEffect(() => {
    if (view !== 'form' && view !== 'review') return
    if (sellID === null || buyID === null || editAtoms === 0) return
    const ticker = window.setInterval(fetchPreview, previewRefreshMS)
    return () => window.clearInterval(ticker)
  }, [view, sellID, buyID, editAtoms, slippageBps, exactOutput])

  // ---- Actions ----

  const flip = () => {
    setSellID(buyID)
    setBuyID(sellID)
  }

  const openPicker = (side: 'sell' | 'buy') => {
    setPickSide(side)
    setQuery('')
    setView('pick')
  }

  const pick = (a: IntentsAsset) => {
    if (pickSide === 'sell') {
      if (a.assetID === buyID) setBuyID(sellID) // selected the other side: swap
      setSellID(a.assetID)
    } else {
      if (a.assetID === sellID) setSellID(buyID)
      setBuyID(a.assetID)
    }
    setView('form')
  }

  const setMax = () => {
    if (!sellAsset || sellAvail === 0) return
    const conv = sellAvail / Math.pow(10, sellAsset.atomDecimals)
    setEditSide('sell')
    setEditUnit('asset')
    setAmount(conv.toFixed(sellAsset.atomDecimals).replace(/\.?0+$/, ''))
  }

  // Typing in any of the four fields claims it: the typed field drives
  // the quote and the other three display derived values.
  const amountChanged = (side: 'sell' | 'buy', unit: 'asset' | 'usd') => (e: ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value.replace(',', '.')
    if (!/^\d*\.?\d*$/.test(v)) return
    setEditSide(side)
    setEditUnit(unit)
    setAmount(v)
  }

  const execute = async () => {
    if (!preview || executing) return
    setExecuting(true)
    setExecErr('')
    const r = await postJSON('/api/intents/execute', {
      originAssetID: sellID,
      destAssetID: buyID,
      value: editAtoms,
      slippageBps,
      exactOutput,
      // The bound the user accepted: min received for exact-input, the
      // previewed cost ceiling for exact-output.
      minAtomsOut: exactOutput ? 0 : preview.minAtomsOut,
      maxAtomsIn: exactOutput ? preview.atomsIn : 0,
      pass: pw
    })
    setExecuting(false)
    if (!r.ok) {
      setExecErr(r.msg || r.errMsg || t('swap failed'))
      return
    }
    setPW('')
    setSwap(r.swap)
    setView('progress')
  }

  const refreshSwap = async () => {
    if (!swap) return
    const r = await postJSON('/api/intents/refresh', { depositAddress: swap.depositAddress })
    if (r.ok && r.swap) setSwap(r.swap)
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
    const q = query.trim().toLowerCase()
    const list = (assets ?? []).filter(a => {
      if (q === '') return true
      return assetTicker(a).toLowerCase().includes(q) ||
        assetName(a).toLowerCase().includes(q) ||
        a.symbol.toLowerCase().includes(q)
    })
    // Ready wallets first, ordered by USD balance; inactive entries trail.
    const balUSD = (a: IntentsAsset) =>
      usdValue(a, (app.wallet(a.assetID)?.balance?.available ?? 0) / Math.pow(10, a.atomDecimals))
    list.sort((a, b) => {
      if (a.walletReady !== b.walletReady) return a.walletReady ? -1 : 1
      return balUSD(b) - balUSD(a)
    })
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
            const avail = app.wallet(a.assetID)?.balance?.available ?? 0
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

  // ---- Shared quote-derived display values (form + review) ----

  const buyUI = buyAsset ? unitInfoFor(buyAsset) : undefined
  const sellUI = sellAsset ? unitInfoFor(sellAsset) : undefined
  // Each side's amount in atoms: the typed side derives from the entry
  // (asset or USD), the other from the quote.
  const sellAtomsShown = editSide === 'sell' ? editAtoms : (preview?.atomsIn ?? 0)
  const buyAtomsShown = editSide === 'buy' ? editAtoms : (preview?.atomsOut ?? 0)
  const sellConv = sellAsset ? sellAtomsShown / Math.pow(10, sellAsset.atomDecimals) : 0
  const buyConv = buyAsset ? buyAtomsShown / Math.pow(10, buyAsset.atomDecimals) : 0
  const sellUSD = sellAsset && sellConv > 0 ? Doc.formatUSD(usdValue(sellAsset, sellConv)) : ''
  const buyUSD = buyAsset && buyConv > 0 ? Doc.formatUSD(usdValue(buyAsset, buyConv)) : ''
  // Form-field values. The field being typed in shows the raw entry; the
  // other three show plain derived strings.
  const sellAssetValue = editSide === 'sell' && editUnit === 'asset'
    ? amount
    : sellAsset ? plainAtoms(sellAtomsShown, sellAsset.atomDecimals) : ''
  const buyAssetValue = editSide === 'buy' && editUnit === 'asset'
    ? amount
    : buyAsset ? plainAtoms(buyAtomsShown, buyAsset.atomDecimals) : ''
  const sellUSDValue = editSide === 'sell' && editUnit === 'usd'
    ? amount
    : sellAsset ? plainUSD(usdValue(sellAsset, sellConv)) : ''
  const buyUSDValue = editSide === 'buy' && editUnit === 'usd'
    ? amount
    : buyAsset ? plainUSD(usdValue(buyAsset, buyConv)) : ''
  // Review-view amounts, straight from the quote.
  const sellFieldText = preview && sellUI ? Doc.formatCoinValue(preview.atomsIn, sellUI) : ''
  const buyFieldText = preview && buyUI ? Doc.formatCoinValue(preview.atomsOut, buyUI) : ''
  const quoteRate = preview && sellConv > 0 && buyConv > 0 ? buyConv / sellConv : 0
  const indicativeRate = sellAsset && buyAsset && usdRate(buyAsset) > 0
    ? usdRate(sellAsset) / usdRate(buyAsset)
    : 0
  const rateShown = quoteRate || indicativeRate
  const rateText = rateShown && sellAsset && buyAsset
    ? `1 ${assetTicker(sellAsset)} = ${Doc.formatFourSigFigs(rateShown)} ${assetTicker(buyAsset)}`
    : ''
  const minOutText = preview && buyAsset && buyUI
    ? `${Doc.formatCoinValue(preview.minAtomsOut, buyUI)} ${assetTicker(buyAsset)}`
    : ''
  const maxInText = preview && sellAsset && sellUI
    ? `${Doc.formatCoinValue(preview.atomsIn, sellUI)} ${assetTicker(sellAsset)}`
    : ''
  const etaText = preview ? `~${Doc.formatDuration(preview.timeEstimate * 1000)}` : ''
  const feeBps = preview ? preview.appFees.reduce((sum, f) => sum + f.fee, 0) : 0
  const sellAtoms = exactOutput ? (preview?.atomsIn ?? 0) : editAtoms
  const overBalance = sellAtoms > sellAvail

  // ---- Review view ----

  if (view === 'review' && sellAsset && buyAsset) {
    const canSwap = Boolean(preview) && !executing && (!requirePW || pw !== '')
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

        {previewErr && <div className="warncolor fs12 px-3 pb-1 text-center">{previewErr}</div>}
        {execErr && <div className="errcolor fs12 px-3 pb-1 text-center">{execErr}</div>}

        <div className="flex-stretch-column p-3">
          <button type="submit" className="feature swap-cta" disabled={!canSwap}>
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
    if (sellID === null || buyID === null) {
      return <button type="button" className="feature swap-cta" disabled>{t('Select assets')}</button>
    }
    if (editAtoms === 0) {
      return <button type="button" className="feature swap-cta" disabled>{t('Enter an amount')}</button>
    }
    if (overBalance) {
      return (
        <button type="button" className="feature swap-cta" disabled>
          {tvar('Not enough {{ ticker }}', { ticker: sellAsset ? assetTicker(sellAsset) : '' })}
        </button>
      )
    }
    if (previewErr) {
      return <button type="button" className="feature swap-cta" onClick={fetchPreview}>{t('Retry quote')}</button>
    }
    if (!preview) {
      return (
        <button type="button" className="feature swap-cta" disabled>
          <span className="d-inline-flex align-items-center justify-content-center"><Spinner className="me-2" />{t('Fetching quote...')}</span>
        </button>
      )
    }
    return <button type="button" className="feature swap-cta" onClick={() => { setExecErr(''); setView('review') }}>{t('Review swap')}</button>
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
        {previewErr && <div className="warncolor fs12 pt-2 text-center">{previewErr}</div>}
      </div>

      <div className="flex-stretch-column px-3 pb-3 pt-2">
        {actionButton()}
      </div>
    </form>
  )
}

// ---- Progress subview ----

// fetchWalletTx loads a single transaction from a wallet's history, or
// null when the wallet has no record of it.
async function fetchWalletTx (assetID: number, txID: string): Promise<WalletTransaction | null> {
  const r = await postJSON('/api/wallettx', { assetID, txID })
  return r.ok && r.tx ? r.tx as WalletTransaction : null
}

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

// Progress steps for an active swap. Every pre-PROCESSING state is the
// deposit phase.
function activeStep (state: string): number {
  return state === StateProcessing ? 1 : 0
}

function SwapProgress ({ swap, onRefresh, onClose }: SwapProgressProps) {
  const active = swapIsActive(swap)
  // depositRejected: the record is still active (the engine has not yet
  // polled), but the fetched deposit tx already proves an on-chain
  // rejection. The header and step indicator show the failure rather than
  // a healthy in-flight swap. Declared here, assigned after depositTx.
  let depositRejected = false
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

  // The deposit leg, for its network fee: undefined while loading, null
  // when the wallet has no record of it.
  const [depositTx, setDepositTx] = useState<WalletTransaction | null | undefined>(undefined)
  useEffect(() => {
    if (!swap.depositTxID) {
      setDepositTx(null)
      return
    }
    let stale = false
    setDepositTx(undefined)
    fetchWalletTx(swap.originAssetID, swap.depositTxID).then(tx => {
      if (!stale) setDepositTx(tx)
    })
    return () => { stale = true }
  }, [swap.originAssetID, swap.depositTxID])

  depositRejected = active && depositTx?.rejected === true

  // The deposit's network fee is paid in the origin chain's native asset.
  const feeAssetID = originAsset?.token ? originAsset.token.networkID : swap.originAssetID
  const feeAsset = app.user?.assets[feeAssetID]
  const feeUSD = depositTx && feeAsset
    ? Doc.usdString(depositTx.fees, feeAsset.unitInfo.conventional.conversionFactor, app.idRate(feeAssetID) ?? 0)
    : ''

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
  const step = activeStep(swap.state)

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
