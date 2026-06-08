import { useCallback, useEffect, useRef, useState, FormEvent, ChangeEvent, KeyboardEvent } from 'react'
import t, { tvar } from '../js/intl'
import app from '../js/application'
import { postJSON } from '../js/http'
import { TickerAsset, AssetVariant } from '../js/assets'
import Doc from '../js/doc'
import CoinLogo from './CoinLogo'
import Spinner from './Spinner'
import Checkbox from './Checkbox'
import PasswordInput from './PasswordInput'

// traitWithdrawer marks wallets that can subtract the network fee from the
// sent amount (Withdraw). Bit value mirrors asset.WalletTrait in Go.
const traitWithdrawer = 1 << 6

const languages = navigator.languages as string[]
const usdFmt = new Intl.NumberFormat(languages, {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

// activationNetworkID is the asset whose wallet must exist to activate the
// variant — the parent network for tokens, the asset itself for natives.
function activationNetworkID (v: AssetVariant): number {
  return v.token ? v.token.networkID : v.assetID
}

// isNetworkActive reports whether the variant's network wallet exists.
function isNetworkActive (v: AssetVariant): boolean {
  return Boolean(app.user?.assets[activationNetworkID(v)]?.wallet)
}

// singleNetworkVariant returns the sole variant when an asset settles on
// exactly one network (active + inactive == 1) and that network is already
// active — letting us skip the network picker and go straight to the send
// form. A lone inactive network returns null so the picker still renders its
// inline Activate affordance.
function singleNetworkVariant (ta: TickerAsset): AssetVariant | null {
  if (ta.variants.length !== 1) return null
  const v = ta.variants[0]
  return isNetworkActive(v) ? v : null
}

// networkAsset returns the SupportedAsset that settles the variant's fees —
// the parent network for tokens, the variant itself for natives.
function networkAsset (v: AssetVariant) {
  return app.user?.assets[activationNetworkID(v)]
}

function networkTicker (v: AssetVariant): string {
  return networkAsset(v)?.unitInfo.conventional.unit ?? ''
}

// cFactorOf returns a variant's conventional conversion factor.
function cFactorOf (v: AssetVariant): number {
  return v.ui.conventional.conversionFactor
}

// usd renders an atomic value at the given conversion factor / rate as a USD
// string, or an em dash when no rate is available.
function usd (atomic: number, cFactor: number, rate: number): string {
  if (!rate) return '—'
  return usdFmt.format((atomic / cFactor) * rate)
}

function parseAmount (s: string): number {
  const v = parseFloat(s)
  return Number.isFinite(v) && v > 0 ? v : 0
}

const noSubmit = (e: FormEvent) => e.preventDefault()

interface SendProps {
  // Optional preselected ticker (e.g. when launched from a portfolio row's
  // Send icon). When set, the asset picker is skipped and we land on the
  // network picker (step 2), which is never skipped — it's a deliberate
  // gut-check on which chain the funds leave.
  ticker?: string
}

export default function Send ({ ticker }: SendProps) {
  const initialTa = ticker ? (app.tickerMap?.[ticker] ?? null) : null
  const [ta, setTa] = useState<TickerAsset | null>(initialTa)
  // Single-network assets skip the picker, so preselect lands on the send form.
  const [variant, setVariant] = useState<AssetVariant | null>(
    initialTa ? singleNetworkVariant(initialTa) : null)

  // Step 2 (network picker) state.
  const [netErr, setNetErr] = useState<Record<number, string>>({})
  const [activating, setActivating] = useState<number | null>(null)
  // Bumped after an activation so the picker re-reads app.user wallet state.
  const [, setActivatedTick] = useState(0)

  // Step 3 (send form) state.
  const [address, setAddress] = useState('')
  const [amount, setAmount] = useState('')
  const [subtract, setSubtract] = useState(false)
  const [addrValid, setAddrValid] = useState<boolean | null>(null)
  // When the address validates, the input collapses to a wrapped read-only
  // display. editingAddr forces the input back open so the user can change it.
  const [editingAddr, setEditingAddr] = useState(false)
  const [txFee, setTxFee] = useState(0)
  const [feeKnown, setFeeKnown] = useState(false)
  const [feeErr, setFeeErr] = useState('')
  const [estimating, setEstimating] = useState(false)
  const [pw, setPW] = useState('')
  const [sending, setSending] = useState(false)
  const [err, setErr] = useState('')

  // Monotonic guards so stale validate/txfee responses can't clobber newer ones.
  const validateRef = useRef(0)
  const feeRef = useRef(0)

  const close = () => app.setForm({ form: '', data: {} })

  const requirePW = app.appSettings.sendRequiresPassword

  // ---- step 3 derived values (safe even when variant is null) ----
  const cFactor = variant?.ui.conventional.conversionFactor ?? 1
  const isToken = Boolean(variant?.token)
  const traits = variant ? (app.wallet(variant.assetID)?.traits ?? 0) : 0
  const isWithdrawer = (traits & traitWithdrawer) !== 0
  const available = variant?.bal.available ?? 0
  const assetRate = variant ? (app.idRate(variant.assetID) ?? 0) : 0
  const feeUI = variant ? (isToken ? app.unitInfo(activationNetworkID(variant)) : variant.ui) : undefined
  const feeRate = variant ? (app.idRate(activationNetworkID(variant)) ?? 0) : 0
  const feeCFactor = feeUI?.conventional.conversionFactor ?? 1
  const value = variant ? Math.round(parseAmount(amount) * cFactor) : 0

  // runValidation validates an address immediately, collapsing the input to
  // the read-only display on success. The monotonic guard drops stale
  // responses, so a debounced call and an Enter-triggered call can't race.
  const runValidation = useCallback(async (addr: string) => {
    if (!variant) return
    const trimmed = addr.trim()
    if (trimmed === '') { setAddrValid(null); return }
    const id = ++validateRef.current
    setAddrValid(null)
    const r = await postJSON('/api/validateaddress', { addr: trimmed, assetID: variant.assetID })
    if (id !== validateRef.current) return
    const ok = Boolean(r.ok)
    setAddrValid(ok)
    if (ok) setEditingAddr(false) // collapse to the read-only display
  }, [variant])

  // Live address validation, debounced.
  useEffect(() => {
    if (!variant) return
    if (address.trim() === '') { setAddrValid(null); return }
    setAddrValid(null)
    const h = setTimeout(() => { runValidation(address) }, 350)
    return () => clearTimeout(h)
  }, [address, variant, runValidation])

  // Enter validates immediately rather than waiting for the debounce, and
  // must not submit the surrounding form.
  const onAddrKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'Enter') return
    e.preventDefault()
    runValidation(address)
  }

  // Live fee estimation, debounced. Only meaningful once we have a valid
  // address and a positive amount. A failed estimate (e.g. a token whose
  // parent chain has no gas balance) surfaces the error and blocks sending —
  // the broadcast would otherwise fail anyway.
  useEffect(() => {
    if (!variant) return
    setFeeKnown(false); setTxFee(0); setFeeErr('')
    if (!addrValid || value <= 0) { setEstimating(false); return }
    const id = ++feeRef.current
    setEstimating(true)
    const h = setTimeout(async () => {
      const r = await postJSON('/api/txfee', {
        addr: address.trim(),
        assetID: variant.assetID,
        value,
        subtract
      })
      if (id !== feeRef.current) return
      setEstimating(false)
      if (r.ok && r.validaddress) { setTxFee(r.txfee); setFeeKnown(true); return }
      // For a token the fee is paid in the parent gas asset, and the amount is
      // already bounded by the balance check, so an estimate failure here means
      // there isn't enough gas — say so plainly. Natives surface the raw error.
      const feeTicker = (variant.token ? app.unitInfo(activationNetworkID(variant)) : variant.ui).conventional.unit
      setFeeErr(variant.token
        ? tvar('Not enough {{ ticker }} to cover the network fee.', { ticker: feeTicker })
        : (r.msg || r.errMsg || tvar('Could not estimate the network fee. You may need {{ ticker }} for gas.', { ticker: feeTicker })))
    }, 350)
    return () => clearTimeout(h)
  }, [address, value, subtract, addrValid, variant])

  // selectVariant advances to the send form for the chosen variant, resetting
  // the form state. No balance guard — that belongs to the explicit picker
  // click (pickVariant); the single-network skip path proceeds regardless.
  const selectVariant = (v: AssetVariant) => {
    setVariant(v)
    setAddress(''); setAmount(''); setSubtract(false)
    setAddrValid(null); setEditingAddr(false); setTxFee(0); setFeeKnown(false); setPW(''); setErr('')
  }

  const pickAsset = (chosen: TickerAsset) => {
    setTa(chosen)
    setNetErr({})
    // Skip the picker when the asset settles on a single, active network.
    const lone = singleNetworkVariant(chosen)
    if (lone) selectVariant(lone)
  }

  const pickVariant = (v: AssetVariant) => {
    if (v.bal.available <= 0) {
      setNetErr(prev => ({
        ...prev,
        [v.assetID]: tvar('No {{ ticker }} balance on {{ network }}.', {
          ticker: v.ticker,
          network: networkAsset(v)?.name ?? networkTicker(v)
        })
      }))
      return
    }
    selectVariant(v)
  }

  // Inline activation of a network wallet, mirroring the Receive picker.
  const inlineActivate = async (v: AssetVariant) => {
    const netID = activationNetworkID(v)
    const walletDef = app.user?.assets[netID]?.info?.availablewallets?.[0]
    if (!walletDef) {
      setNetErr(prev => ({ ...prev, [v.assetID]: t('No wallet definition available') }))
      return
    }
    setActivating(v.assetID)
    setNetErr(prev => { const next = { ...prev }; delete next[v.assetID]; return next })
    const r = await postJSON('/api/newwallet', { assetID: netID, walletType: walletDef.type, config: {} })
    setActivating(null)
    if (!r.ok) {
      setNetErr(prev => ({ ...prev, [v.assetID]: r.errMsg || r.msg || t('Failed to activate') }))
      return
    }
    setActivatedTick(n => n + 1) // re-read wallet state into the picker
  }

  const backToAssets = () => {
    setTa(null); setVariant(null); setNetErr({}); setActivating(null)
  }

  // From the send form, Back returns to the network picker (step 2) rather
  // than all the way to the asset picker.
  const backToNetwork = () => {
    setVariant(null); setErr('')
  }

  const setMax = async () => {
    if (!variant) return
    const fullBalance = () => setAmount(String(Doc.conventionalCoinValue(available, variant.ui)))
    // Withdrawer wallets send the full balance and take the fee out of it, so
    // Max implies "subtract fee from amount".
    if (isWithdrawer) {
      setSubtract(true)
      fullBalance()
      return
    }
    // Tokens pay their fee in the parent asset, so the max is the full token
    // balance with no deduction.
    if (isToken) {
      fullBalance()
      return
    }
    // Non-withdrawer native (e.g. account-based): max is the balance minus the
    // estimated network fee.
    const id = ++feeRef.current // invalidate any in-flight estimate
    setEstimating(true)
    const r = await postJSON('/api/txfee', {
      assetID: variant.assetID,
      subtract: false,
      maxWithdraw: true,
      value: available
    })
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
    if (requirePW && pw === '') { setErr(t('Password required.')); return }
    setSending(true)
    const r = await postJSON('/api/send', {
      assetID: variant.assetID,
      address: address.trim(),
      value,
      subtract,
      pw: requirePW ? pw : undefined
    })
    setSending(false)
    setPW('')
    if (!r.ok) { setErr(r.errMsg || r.msg || t('Send failed.')); return }
    close()
  }

  // ---- Step 1: asset picker ----
  if (!ta) {
    const list = (app.tickerList ?? []).filter(item => item.avail > 0)
    if (list.length === 0) {
      return (
        <form className="modal form-width-300" onSubmit={noSubmit}>
          <header><span>{t('Send')}</span></header>
          <div className="px-3 pb-3 grey text-center">{t('No funds available to send.')}</div>
          <div className="flex-stretch-column p-3">
            <button type="button" onClick={close}>{t('Close')}</button>
          </div>
        </form>
      )
    }
    return (
      <form className="modal form-width-300" onSubmit={noSubmit}>
        <header><span>{t('Send')}</span></header>
        <div className="pb-2">
          {list.map(item => (
            <div key={item.ticker} className="d-flex align-items-center px-3 py-2 hoverbg pointer" onClick={() => pickAsset(item)}>
              <CoinLogo ticker={item.ticker} className="mini-icon me-2" />
              <span className="flex-grow-1">{item.name}</span>
              <span className="grey fs12">{item.ticker}</span>
            </div>
          ))}
        </div>
        <div className="flex-stretch-column p-3">
          <button type="button" onClick={close}>{t('Cancel')}</button>
        </div>
      </form>
    )
  }

  // ---- Step 2: network picker (never skipped) ----
  if (!variant) {
    // Active networks first, then inactive; preserve order within each group.
    const vs = [...ta.variants].sort((a, b) =>
      (isNetworkActive(a) ? 0 : 1) - (isNetworkActive(b) ? 0 : 1))
    const anyActivating = activating !== null
    return (
      <form className="modal form-width-300" onSubmit={noSubmit}>
        <header>
          <CoinLogo ticker={ta.ticker} className="mini-icon me-2" />
          <span>{tvar('Send {{ name }}', { name: ta.name })}</span>
        </header>
        <div className="px-3 pb-2 fs14 grey text-center">{t('Choose the network to send from.')}</div>
        <div className="pb-2">
          {vs.map(v => {
            const nt = networkTicker(v)
            const nName = networkAsset(v)?.name ?? ''
            const variantErr = netErr[v.assetID]
            if (isNetworkActive(v)) {
              const bal = v.bal.available
              return (
                <div key={v.assetID}>
                  <div className="d-flex align-items-center px-3 py-2 hoverbg pointer" onClick={() => pickVariant(v)}>
                    <CoinLogo ticker={nt} className="mini-icon me-2" />
                    <span className="flex-grow-1">{nName}</span>
                    <span className="d-flex flex-column align-items-end">
                      <span className="mono fs14">{Doc.formatCoinValue(bal, v.ui)} {v.ticker}</span>
                      <span className="grey fs12">{usd(bal, cFactorOf(v), app.idRate(v.assetID) ?? 0)}</span>
                    </span>
                  </div>
                  {variantErr && <div className="errcolor fs12 px-3 pb-1 text-end">{variantErr}</div>}
                </div>
              )
            }
            // Inactive network — offer inline activation.
            return (
              <div key={v.assetID} className="receive-inactive-net px-3 py-2">
                <div className="d-flex align-items-center">
                  <CoinLogo ticker={nt} className="mini-icon me-2 receive-inactive-logo" />
                  <span className="flex-grow-1 grey">{nName}</span>
                  <button
                    type="button"
                    className="receive-activate-btn fs12"
                    onClick={() => inlineActivate(v)}
                    disabled={anyActivating}
                  >
                    {activating === v.assetID
                      ? <><Spinner size={12} className="me-1" />{t('Activating...')}</>
                      : t('Activate')}
                  </button>
                </div>
                {variantErr && <div className="errcolor fs12 mt-1">{variantErr}</div>}
              </div>
            )
          })}
        </div>
        <div className="flex-stretch-column p-3">
          <button type="button" onClick={backToAssets}>{t('Back')}</button>
        </div>
      </form>
    )
  }

  // ---- Step 3: send form ----
  const netName = networkAsset(variant)?.name ?? ''
  const netTicker = networkTicker(variant)
  // When the network picker was skipped (single-network asset), Back returns
  // to the asset picker rather than the picker we never showed.
  const back = ta.variants.length === 1 ? backToAssets : backToNetwork
  // Total debited and balance-after, in the sent asset's units. Tokens pay
  // their fee in the parent asset, so the token-side fee is zero here.
  const nativeFee = isToken ? 0 : txFee
  const recipientGets = subtract ? Math.max(0, value - nativeFee) : value
  const totalDebit = subtract ? value : value + nativeFee
  const balanceAfter = Math.max(0, available - totalDebit)
  const overBalance = value > 0 && totalDebit > available
  // A validated address collapses to a read-only wrapped display; a failed
  // one keeps the input open with an orange outline.
  const showAddrDisplay = addrValid === true && !editingAddr
  const addrCls = addrValid === false ? 'send-addr--invalid' : ''
  // Require a confirmed fee estimate before enabling Send. feeKnown is only set
  // on a successful /txfee, which for tokens also verifies the gas asset can
  // cover the fee — so this disables Send when there's insufficient gas, during
  // estimation, and on any estimate failure.
  const canSubmit = Boolean(addrValid) && value > 0 && !overBalance && feeKnown && !sending &&
    (!requirePW || pw !== '')

  const amountChanged = (e: ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value
    if (v === '' || /^\d*\.?\d*$/.test(v)) setAmount(v)
  }

  return (
    <form className="modal form-width-300" onSubmit={(e) => { e.preventDefault(); submit() }}>
      <header>
        <CoinLogo ticker={ta.ticker} className="mini-icon me-2" />
        <span>{tvar('Send {{ name }}', { name: ta.name })}</span>
      </header>

      <div className="px-3 pb-2 fs14 d-flex align-items-center justify-content-center grey">
        <span className="me-2">{t('Network:')}</span>
        <CoinLogo ticker={netTicker} className="mini-icon me-1" />
        <span>{netName}</span>
      </div>

      <div className="px-3 pb-2">
        <div className="d-flex justify-content-between fs12 grey">
          <span>{t('Recipient address')}</span>
          {showAddrDisplay && (
            <span className="pointer send-maxlink" onClick={() => setEditingAddr(true)}>{t('Edit')}</span>
          )}
        </div>
        {showAddrDisplay
          ? (
            <div
              className="form-input-bg br5 mono fs14 send-addr-display pointer has-tip"
              aria-label={t('Click to edit')}
              onClick={() => setEditingAddr(true)}
            >{address.trim()}</div>
            )
          : (
            <input
              type="text"
              className={`send-addr ${addrCls}`}
              value={address}
              spellCheck={false}
              autoComplete="off"
              autoFocus
              placeholder={t('address')}
              onChange={(e) => setAddress(e.target.value)}
              onKeyDown={onAddrKeyDown}
              onBlur={() => { if (addrValid) setEditingAddr(false) }}
            />
            )}
      </div>

      <div className="px-3 pb-2">
        <div className="d-flex justify-content-between fs12 grey">
          <span>{t('Amount')}</span>
          <span className="pointer send-maxlink" onClick={setMax}>
            {t('Available:')} {Doc.formatCoinValue(available, variant.ui)} {variant.ticker}
          </span>
        </div>
        <div className="form-input-bg br5 d-flex align-items-center send-amt">
          <input
            type="text"
            inputMode="decimal"
            value={amount}
            placeholder="0.00"
            onChange={amountChanged}
          />
          <span className="grey fs14 px-2">{variant.ticker}</span>
          <button type="button" className="small subtle me-2" onClick={setMax}>{t('Max')}</button>
        </div>
        <div className="fs12 grey pt-1">{usd(value, cFactor, assetRate)}</div>
      </div>

      {isWithdrawer && (
        <label className="checkbox-row mx-3">
          <Checkbox checked={subtract} onChange={setSubtract} ariaLabel={t('Subtract fee from amount')} />
          <span className="fs14">{t('Subtract fee from amount')}</span>
        </label>
      )}

      <div className="px-3 pb-2 fs13 send-summary">
        <div className="d-flex justify-content-between py-1">
          <span className="grey">{t('Network fee')}</span>
          <span>
            {estimating
              ? <Spinner size={12} />
              : feeKnown
                ? <>{Doc.formatCoinValue(txFee, feeUI)} {feeUI?.conventional.unit} <span className="grey">({usd(txFee, feeCFactor, feeRate)})</span></>
                : '—'}
          </span>
        </div>
        {feeErr && <div className="errcolor fs12 py-1">{feeErr}</div>}
        {subtract && (
          <div className="d-flex justify-content-between py-1">
            <span className="grey">{t('Recipient gets')}</span>
            <span className="mono">{Doc.formatCoinValue(recipientGets, variant.ui)} {variant.ticker}</span>
          </div>
        )}
        <div className="d-flex justify-content-between py-1">
          <span className="grey">{t('Balance after')}</span>
          <span className="mono">{Doc.formatCoinValue(balanceAfter, variant.ui)} {variant.ticker}</span>
        </div>
      </div>

      {requirePW && (
        <div className="px-3 pb-2">
          <label className="fs12 grey">{t('App password')}</label>
          <PasswordInput value={pw} onChange={(e) => setPW(e.target.value)} disabled={sending} />
        </div>
      )}

      {overBalance && <div className="errcolor fs12 px-3 pb-1 text-center">{t('Amount exceeds available balance.')}</div>}
      <div className={`errcolor fs12 px-3 pb-1 text-center ${err ? '' : 'd-none'}`}>{err}</div>

      <div className="flex-stretch-column p-3">
        <button type="submit" className="feature" disabled={!canSubmit}>
          {sending
            ? <span className="d-inline-flex align-items-center justify-content-center"><Spinner className="me-2" />{t('Sending...')}</span>
            : t('Send')}
        </button>
        <button type="button" className="mt-2" onClick={back} disabled={sending}>{t('Back')}</button>
        <button type="button" className="mt-2" onClick={close} disabled={sending}>{t('Cancel')}</button>
      </div>
    </form>
  )
}
