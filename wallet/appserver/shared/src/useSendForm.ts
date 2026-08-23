import { useCallback, useEffect, useRef, useState, ChangeEvent } from 'react'
import t, { tvar } from './intl'
import app from './application'
import { postJSON } from './http'
import { TickerAsset, AssetVariant, networkID, networkAsset, isVariantActive } from './assets'
import { traitWithdrawer, traitDestinationTagger } from './registry'
import Doc from './doc'

// singleNetworkVariant returns the sole variant when an asset settles on exactly
// one network and that network is already active — letting the caller skip the
// network picker and land on the send form. A lone inactive network returns
// null so the picker still renders (with its inline Activate affordance).
export function singleNetworkVariant (ta: TickerAsset): AssetVariant | null {
  if (ta.variants.length !== 1) return null
  const v = ta.variants[0]
  return isVariantActive(v) ? v : null
}

// parseSendAmount parses a conventional amount string, returning 0 for anything
// that is not a positive finite number.
export function parseSendAmount (s: string): number {
  const v = parseFloat(s)
  return Number.isFinite(v) && v > 0 ? v : 0
}

export interface UseSendFormOptions {
  // Optional preselected ticker (e.g. launched from a portfolio row). When set,
  // the asset picker is skipped; single-network assets skip the network picker
  // too and land directly on the send form.
  ticker?: string
  // onSent is invoked after a successful /api/send (each shell closes its own
  // surface: the desktop modal, the mobile overlay).
  onSent: () => void
}

// useSendForm is the shared engine behind the desktop and mobile Send flows. It
// owns the asset/network/amount state, debounced address validation and fee
// estimation (both with monotonic guards so stale responses can't clobber newer
// ones), Max, and submit. The two shells differ only in layout and a few
// shell-specific extras (address-collapse, inline activation, fiat display),
// which they layer on top of the values returned here.
export function useSendForm ({ ticker, onSent }: UseSendFormOptions) {
  const initialTa = ticker ? (app.tickerMap?.[ticker] ?? null) : null
  const [ta, setTa] = useState<TickerAsset | null>(initialTa)
  const [variant, setVariant] = useState<AssetVariant | null>(
    initialTa ? singleNetworkVariant(initialTa) : null)

  const [address, setAddress] = useState('')
  const [amount, setAmount] = useState('')
  const [subtract, setSubtract] = useState(false)
  const [destTag, setDestTag] = useState('')
  const [addrValid, setAddrValid] = useState<boolean | null>(null)
  const [txFee, setTxFee] = useState(0)
  const [feeKnown, setFeeKnown] = useState(false)
  const [feeErr, setFeeErr] = useState('')
  const [estimating, setEstimating] = useState(false)
  const [pw, setPW] = useState('')
  const [sending, setSending] = useState(false)
  const [err, setErr] = useState('')
  const [netErr, setNetErr] = useState<Record<number, string>>({})
  // estimateNonce forces the fee-estimate effect to re-run without any form
  // input changing. Bumped when a send is rejected: core validates the
  // submitted rate against its own current estimate, so a rejection can mean
  // the market moved since our estimate, and retrying with the same stale rate
  // would fail identically. A fresh estimate re-arms the form with a current
  // rate.
  const [estimateNonce, setEstimateNonce] = useState(0)

  // Monotonic guards so stale validate/txfee responses can't clobber newer ones.
  const validateRef = useRef(0)
  const feeRef = useRef(0)
  // feeRateRef holds the fee rate from the latest estimate. It is submitted with
  // the send so the rate the user approved is the rate that is charged (UTXO);
  // account/EVM wallets ignore it and compute their own.
  const feeRateRef = useRef(0)

  const requirePW = app.appSettings.sendRequiresPassword

  // Derived values, safe even when variant is null.
  const cFactor = variant?.ui.conventional.conversionFactor ?? 1
  const isToken = Boolean(variant?.token)
  const traits = variant ? (app.wallet(variant.assetID)?.traits ?? 0) : 0
  const isWithdrawer = (traits & traitWithdrawer) !== 0
  const supportsDestTag = (traits & traitDestinationTagger) !== 0
  // Bound client-side so an oversized value never reaches Go's *uint32 decode,
  // which would surface as an opaque form-parse error instead of a clear one.
  const destTagValid = destTag === '' || Number(destTag) <= 4294967295
  const available = variant?.bal.available ?? 0
  const feeUI = variant ? (isToken ? app.unitInfo(networkID(variant)) : variant.ui) : undefined
  const value = variant ? Math.round(parseSendAmount(amount) * cFactor) : 0

  // runValidation validates an address immediately. The monotonic guard drops
  // stale responses, so a debounced call and an Enter-triggered call can't race.
  const runValidation = useCallback(async (addr: string) => {
    if (!variant) return
    const trimmed = addr.trim()
    if (trimmed === '') { setAddrValid(null); return }
    const id = ++validateRef.current
    setAddrValid(null)
    const r = await postJSON('/api/validateaddress', { addr: trimmed, assetID: variant.assetID })
    if (id !== validateRef.current) return
    setAddrValid(Boolean(r.ok))
  }, [variant])

  // Live address validation, debounced.
  useEffect(() => {
    if (!variant) return
    if (address.trim() === '') { setAddrValid(null); return }
    setAddrValid(null)
    const h = setTimeout(() => { runValidation(address) }, 350)
    return () => clearTimeout(h)
  }, [address, variant, runValidation])

  // Live fee estimation, debounced. Only meaningful once we have a valid address
  // and a positive amount. A failed estimate (e.g. a token whose parent chain
  // has no gas balance) surfaces the error and blocks sending — the broadcast
  // would otherwise fail anyway.
  useEffect(() => {
    if (!variant) return
    setFeeKnown(false); setTxFee(0); setFeeErr(''); feeRateRef.current = 0
    if (!addrValid || value <= 0) { setEstimating(false); return }
    const id = ++feeRef.current
    setEstimating(true)
    const h = setTimeout(async () => {
      const r = await postJSON('/api/txfee', { addr: address.trim(), assetID: variant.assetID, value, subtract })
      if (id !== feeRef.current) return
      setEstimating(false)
      if (r.ok && r.validaddress) { setTxFee(r.txfee); feeRateRef.current = r.feeRate ?? 0; setFeeKnown(true); return }
      // For a token the fee is paid in the parent gas asset, and the amount is
      // already bounded by the balance check, so an estimate failure here means
      // there isn't enough gas — say so plainly. Natives surface the raw error.
      const feeTicker = (variant.token ? app.unitInfo(networkID(variant)) : variant.ui).conventional.unit
      setFeeErr(variant.token
        ? tvar('Not enough {{ ticker }} to cover the network fee.', { ticker: feeTicker })
        : (r.msg || r.errMsg || tvar('Could not estimate the network fee. You may need {{ ticker }} for gas.', { ticker: feeTicker })))
    }, 350)
    return () => clearTimeout(h)
  }, [address, value, subtract, addrValid, variant, estimateNonce])

  // selectVariant advances to the send form for the chosen variant, resetting
  // the form state. No balance guard — that belongs to the explicit picker
  // click (pickVariant); the single-network skip path proceeds regardless.
  const selectVariant = useCallback((v: AssetVariant) => {
    setVariant(v)
    setAddress(''); setAmount(''); setSubtract(false); setDestTag('')
    setAddrValid(null); setTxFee(0); setFeeKnown(false); setPW(''); setErr('')
  }, [])

  const pickAsset = (chosen: TickerAsset) => {
    setTa(chosen)
    setNetErr({})
    // Skip the picker when the asset settles on a single, active network.
    const lone = singleNetworkVariant(chosen)
    if (lone) selectVariant(lone)
  }

  // pickVariant guards the explicit network tap: a variant with no available
  // balance would lead to a form that can never satisfy value>0, so block it
  // with an inline reason instead of stranding the user. The single-active-
  // network skip path in pickAsset proceeds regardless.
  const pickVariant = (v: AssetVariant) => {
    if (v.bal.available <= 0) {
      setNetErr(prev => ({
        ...prev,
        [v.assetID]: tvar('No {{ ticker }} balance on {{ network }}.', {
          ticker: v.ticker,
          network: networkAsset(v)?.name ?? v.ticker
        })
      }))
      return
    }
    selectVariant(v)
  }

  const setMax = async () => {
    if (!variant) return
    const full = () => setAmount(String(Doc.conventionalCoinValue(available, variant.ui)))
    // Withdrawer wallets send the full balance and take the fee out of it, so
    // Max implies "subtract fee from amount".
    if (isWithdrawer) { setSubtract(true); full(); return }
    // Tokens pay their fee in the parent asset, so the max is the full token
    // balance with no deduction.
    if (isToken) { full(); return }
    // Non-withdrawer native: max is the balance minus the estimated network fee.
    const id = ++feeRef.current // invalidate any in-flight estimate
    setEstimating(true)
    const r = await postJSON('/api/txfee', { assetID: variant.assetID, subtract: false, maxWithdraw: true, value: available })
    if (id !== feeRef.current) return
    setEstimating(false)
    const maxAtomic = r.ok ? Math.max(0, available - r.txfee) : available
    setAmount(String(Doc.conventionalCoinValue(maxAtomic, variant.ui)))
  }

  const submit = async () => {
    if (!variant || sending) return
    setErr('')
    if (!addrValid) { setErr(t('Enter a valid address.')); return }
    if (value <= 0) { setErr(t('Enter an amount greater than zero.')); return }
    // The submitted fee rate is the one behind the fee just estimated and shown,
    // so wait for a pending estimate rather than send at a stale/zero rate.
    if (estimating) { setErr(t('Waiting for the fee estimate.')); return }
    // The submit buttons are disabled without a confirmed estimate, but guard
    // here too so no path can send at a zero rate after a failed estimate.
    if (!feeKnown) { setErr(t('A fee estimate is required.')); return }
    if (requirePW && pw === '') { setErr(t('Password required.')); return }
    setSending(true)
    const r = await postJSON('/api/send', {
      assetID: variant.assetID,
      address: address.trim(),
      value,
      subtract,
      feeRate: feeRateRef.current,
      destTag: supportsDestTag && destTag !== '' ? Number(destTag) : undefined,
      pw: requirePW ? pw : undefined
    })
    setSending(false)
    setPW('')
    if (!r.ok) {
      setErr(r.errMsg || r.msg || t('Send failed.'))
      setEstimateNonce(n => n + 1)
      return
    }
    onSent()
  }

  const amountChanged = (e: ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value
    if (v === '' || /^\d*\.?\d*$/.test(v)) setAmount(v)
  }

  const destTagChanged = (e: ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value
    if (v === '' || /^\d+$/.test(v)) setDestTag(v)
  }

  // Tokens pay their fee in the parent asset, so the sent-asset fee is zero.
  const nativeFee = isToken ? 0 : txFee
  const totalDebit = subtract ? value : value + nativeFee
  const overBalance = value > 0 && totalDebit > available
  // Require a confirmed fee estimate before enabling Send. feeKnown is only set
  // on a successful /txfee, which for tokens also verifies the gas asset can
  // cover the fee — so this disables Send when there's insufficient gas, during
  // estimation, and on any estimate failure.
  const canSubmit = Boolean(addrValid) && value > 0 && !overBalance && feeKnown && !sending &&
    (!requirePW || pw !== '') && destTagValid

  return {
    // state
    ta, setTa, variant, setVariant,
    address, setAddress, amount, subtract, setSubtract, destTag,
    addrValid, txFee, feeKnown, feeErr, estimating,
    pw, setPW, sending, err, setErr, netErr, setNetErr,
    // derived
    requirePW, cFactor, isToken, isWithdrawer, available, feeUI, value,
    nativeFee, totalDebit, overBalance, canSubmit, supportsDestTag, destTagValid,
    // handlers
    runValidation, amountChanged, destTagChanged, pickAsset, pickVariant, selectVariant, setMax, submit
  }
}
