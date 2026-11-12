import { useEffect, useMemo, useRef, useState, ChangeEvent } from 'react'
import t from './intl'
import app from './application'
import Doc from './doc'
import State from './state'
import { getJSON, postJSON } from './http'
import { useForceUpdate } from './hooks'
import { IntentsAsset, IntentsPreview, IntentsSwap, conventionalToAtoms, assetTicker } from './intents'
import { availableAtoms, balanceUSD, plainAtoms, plainUSD, unitInfoFor, usdRate, usdValue } from './swap'
import { normalizedTicker } from './assets'

// localStorage keys for sticky swap-form preferences.
const pairLK = 'intentsPair'
const slippageLK = 'intentsSlippageBps'

// Preview quotes refresh on this cadence while a live view (form or review)
// is showing, so displayed pricing tracks the market.
export const previewRefreshMS = 10000
const previewDebounceMS = 350

export type SwapSide = 'sell' | 'buy'
export type SwapUnit = 'asset' | 'usd'

// SwapFormStatus is the form's readiness, in the order the primary action
// reports it: no pair, no amount, insufficient balance, a failed quote
// (retryable), a quote in flight, then ready for review.
export type SwapFormStatus = 'select' | 'amount' | 'balance' | 'retry' | 'quoting' | 'ready'

export interface UseSwapFormOptions {
  // liveView names the view showing live quotes ('form' or 'review'), or
  // null when neither is (e.g. the asset picker or the progress view). While
  // set, the quote and the deposit-fee projection refresh every
  // previewRefreshMS; a change of view restarts both schedules.
  liveView: 'form' | 'review' | null
  // ticker optionally focuses the form on one asset (a launch from that
  // asset's screen): its best-funded ready variant takes the sell side when
  // it holds a balance, else the buy side. Without it the last-used pair is
  // restored.
  ticker?: string
  // onExecuted receives the new swap record after a successful execute.
  onExecuted: (swap: IntentsSwap) => void
}

// useSwapForm is the shared engine behind the desktop and mobile cross-chain
// swap forms. It owns the asset catalog, the pair and amount entry (either
// side, in asset units or USD), slippage, sequenced preview quoting with
// debounce and periodic refresh, the deposit network-fee projection, and
// execution. The shells own layout and view routing.
export function useSwapForm ({ liveView, ticker, onExecuted }: UseSwapFormOptions) {
  const redraw = useForceUpdate()

  const [assets, setAssets] = useState<IntentsAsset[] | null>(null)
  const [assetsErr, setAssetsErr] = useState('')
  const [sellID, setSellID] = useState<number | null>(null)
  const [buyID, setBuyID] = useState<number | null>(null)
  const [amount, setAmount] = useState('')
  // The side and unit the typed amount belongs to. 'buy' quotes
  // exact-output: the service computes the required input for the
  // requested output. A 'usd' unit converts to the side's asset amount at
  // the display rate before quoting.
  const [editSide, setEditSide] = useState<SwapSide>('sell')
  const [editUnit, setEditUnit] = useState<SwapUnit>('asset')
  const [slippageBps, setSlippageBps] = useState<number>(State.fetchLocal(slippageLK) || 100)

  // Preview state. previewSeq orders requests: a response is applied only if
  // no newer request has been issued since, so a slow stale quote can never
  // overwrite a fresher one.
  const [preview, setPreview] = useState<IntentsPreview | null>(null)
  const [previewErr, setPreviewErr] = useState('')
  const [previewing, setPreviewing] = useState(false)
  const previewSeq = useRef(0)
  // nextRefreshAt is when the periodic refresh next fires (ms since the
  // epoch), or 0 when no refresh is scheduled. It backs countdown labels.
  const [nextRefreshAt, setNextRefreshAt] = useState(0)

  // Execution.
  const [pw, setPW] = useState('')
  const [executing, setExecuting] = useState(false)
  const [execErr, setExecErr] = useState('')

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

  const sellAvail = availableAtoms(sellID)
  const buyAvail = availableAtoms(buyID)

  // Projected origin-chain network fee for the deposit transaction, in the
  // fee asset's atoms (the parent chain's for tokens). Refreshed on the
  // preview cadence while a live view is showing.
  const [depositFee, setDepositFee] = useState(0)
  const sellWalletAsset = sellID === null ? undefined : app.user?.assets[sellID]
  const depositFeeAssetID = sellWalletAsset?.token ? sellWalletAsset.token.networkID : sellID
  useEffect(() => {
    setDepositFee(0)
    if (sellID === null || liveView === null || !app.wallet(sellID)) return
    let stale = false
    const fetchFee = async () => {
      const r = await postJSON('/api/feestate', { assetID: sellID })
      if (!stale && r.ok) setDepositFee(r.feeState?.send ?? 0)
    }
    fetchFee()
    const ticker = window.setInterval(fetchFee, previewRefreshMS)
    return () => { stale = true; window.clearInterval(ticker) }
  }, [sellID, liveView])
  // A native sell asset pays the deposit's network fee from the same
  // balance, so only the balance net of that fee can be swapped. A token's
  // fee comes from its parent chain.
  const sellSpendable = sellWalletAsset && !sellWalletAsset.token ? Math.max(0, sellAvail - depositFee) : sellAvail
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
      const byValue = [...ready].sort((a, b) => balanceUSD(b) - balanceUSD(a))
      const stored = State.fetchLocal(pairLK) as { sellID: number, buyID: number } | null
      let sell = stored && list.some(a => a.assetID === stored.sellID) ? stored.sellID : null
      let buy = stored && list.some(a => a.assetID === stored.buyID) ? stored.buyID : null
      if (sell === null) sell = byValue[0]?.assetID ?? null
      if (buy === null || buy === sell) buy = ready.find(a => a.assetID !== sell)?.assetID ?? null
      // A requested ticker displaces the restored pair on one side.
      // Match on the ticker group, not the unit: a group can hold variants
      // with another unit (WETH under ETH), and the most valuable member
      // wins.
      const want = ticker?.toUpperCase()
      const groupOf = (a: IntentsAsset) => {
        const sa = app.user?.assets[a.assetID]
        return (sa ? normalizedTicker(sa) : assetTicker(a)).toUpperCase()
      }
      const focus = want ? byValue.find(a => groupOf(a) === want) : undefined
      if (focus) {
        if (availableAtoms(focus.assetID) > 0) {
          if (buy === focus.assetID) buy = sell
          sell = focus.assetID
        } else {
          if (sell === focus.assetID) sell = buy
          buy = focus.assetID
        }
        if (sell === null) sell = byValue.find(a => a.assetID !== buy)?.assetID ?? null
        if (buy === null) buy = ready.find(a => a.assetID !== sell)?.assetID ?? null
      }
      setSellID(sell)
      setBuyID(buy)
    }
    loadAssets()
  }, [])

  // Balance and rate changes arrive as notes; the balance lines and USD
  // estimates read app state directly, so re-render on either.
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

  // Periodic refresh keeps the displayed quote current in the live views,
  // including the review's totals.
  useEffect(() => {
    setNextRefreshAt(0)
    if (liveView === null) return
    if (sellID === null || buyID === null || editAtoms === 0) return
    setNextRefreshAt(Date.now() + previewRefreshMS)
    const ticker = window.setInterval(() => {
      setNextRefreshAt(Date.now() + previewRefreshMS)
      fetchPreview()
    }, previewRefreshMS)
    return () => window.clearInterval(ticker)
  }, [liveView, sellID, buyID, editAtoms, slippageBps, exactOutput])

  // ---- Actions ----

  const flip = () => {
    setSellID(buyID)
    setBuyID(sellID)
  }

  // pick assigns an asset to a side. Choosing the asset already on the
  // other side swaps the pair.
  const pick = (side: SwapSide, a: IntentsAsset) => {
    if (side === 'sell') {
      if (a.assetID === buyID) setBuyID(sellID)
      setSellID(a.assetID)
    } else {
      if (a.assetID === sellID) setSellID(buyID)
      setBuyID(a.assetID)
    }
  }

  const setMax = () => {
    if (!sellAsset || sellSpendable === 0) return
    const conv = sellSpendable / Math.pow(10, sellAsset.atomDecimals)
    setEditSide('sell')
    setEditUnit('asset')
    setAmount(conv.toFixed(sellAsset.atomDecimals).replace(/\.?0+$/, ''))
  }

  // amountChanged builds the change handler for one of the four amount
  // fields. Typing in a field claims it: the typed field drives the quote
  // and the others display derived values.
  const amountChanged = (side: SwapSide, unit: SwapUnit) => (e: ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value.replace(',', '.')
    if (!/^\d*\.?\d*$/.test(v)) return
    setEditSide(side)
    setEditUnit(unit)
    setAmount(v)
  }

  // A synchronous re-entry latch alongside the `executing` render state:
  // the state only takes effect on the next render, so a second click or
  // Enter landing before the re-render would fire a second real
  // execution — a second committed quote and a second deposit — for the
  // same funds.
  const executingRef = useRef(false)
  const execute = async () => {
    if (!preview || executingRef.current) return
    executingRef.current = true
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
    executingRef.current = false
    setExecuting(false)
    if (!r.ok) {
      setExecErr(r.msg || r.errMsg || t('swap failed'))
      return
    }
    setPW('')
    onExecuted(r.swap)
  }

  // ---- Quote-derived display values ----

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
  // others show plain derived strings.
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
  // Review amounts, straight from the quote.
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
  const overBalance = sellAtoms > sellSpendable

  let formStatus: SwapFormStatus = 'ready'
  if (sellID === null || buyID === null) formStatus = 'select'
  else if (editAtoms === 0) formStatus = 'amount'
  else if (overBalance) formStatus = 'balance'
  else if (previewErr) formStatus = 'retry'
  else if (!preview) formStatus = 'quoting'

  // canExecute gates the review's confirm action.
  // A quote refresh can raise an exact-output swap's input past the balance
  // while the review is open, so the balance is checked here too.
  const canExecute = Boolean(preview) && !executing && !overBalance && (!requirePW || pw !== '')

  return {
    // catalog and pair
    assets, assetsErr, assetMap, sellID, buyID, sellAsset, buyAsset, sellAvail, buyAvail,
    // entry
    amount, editSide, editUnit, exactOutput, editAsset, editAtoms, slippageBps, setSlippageBps,
    // quote
    preview, previewErr, previewing, nextRefreshAt, depositFee, depositFeeText,
    // execution
    requirePW, pw, setPW, executing, execErr, setExecErr, canExecute,
    // derived display
    sellUI, buyUI, sellAtomsShown, buyAtomsShown, sellConv, buyConv, sellUSD, buyUSD,
    sellAssetValue, buyAssetValue, sellUSDValue, buyUSDValue, sellFieldText, buyFieldText,
    rateText, minOutText, maxInText, etaText, feeBps, sellAtoms, overBalance, formStatus,
    // actions
    fetchPreview, flip, pick, setMax, amountChanged, execute
  }
}
