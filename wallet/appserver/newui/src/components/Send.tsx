import { useEffect, useState, FormEvent, KeyboardEvent } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import { postJSON } from '@bossw/shared/http'
import { AssetVariant, networkID, networkAsset, isVariantActive } from '@bossw/shared/assets'
import { useSendForm } from '@bossw/shared/useSendForm'
import Doc from '@bossw/shared/doc'
import CoinLogo from '@bossw/shared/components/CoinLogo'
import Spinner from './Spinner'
import Checkbox from './Checkbox'
import PasswordInput from './PasswordInput'

const languages = navigator.languages as string[]
const usdFmt = new Intl.NumberFormat(languages, {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

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

const noSubmit = (e: FormEvent) => e.preventDefault()

interface SendProps {
  // Optional preselected ticker (e.g. when launched from a portfolio row's
  // Send icon). When set, the asset picker is skipped and we land on the
  // network picker (step 2), which is never skipped — it's a deliberate
  // gut-check on which chain the funds leave.
  ticker?: string
}

export default function Send ({ ticker }: SendProps) {
  const close = () => app.setForm({ form: '', data: {} })

  const {
    ta, setTa, variant, setVariant,
    address, setAddress, amount, subtract, setSubtract,
    addrValid, txFee, feeKnown, feeErr, estimating, pw, setPW, sending, err, setErr, netErr, setNetErr,
    requirePW, cFactor, isWithdrawer, available, feeUI, value,
    nativeFee, totalDebit, overBalance, canSubmit,
    runValidation, amountChanged, pickAsset, pickVariant, setMax, submit
  } = useSendForm({ ticker, onSent: close })

  // Step 2 (network picker) activation state — desktop only.
  const [activating, setActivating] = useState<number | null>(null)
  // Bumped after an activation so the picker re-reads app.user wallet state.
  const [, setActivatedTick] = useState(0)

  // When the address validates, the input collapses to a wrapped read-only
  // display. editingAddr forces the input back open so the user can change it.
  const [editingAddr, setEditingAddr] = useState(false)
  // Collapse to the read-only display when the address validates; reopen the
  // input whenever the variant changes (the form resets).
  useEffect(() => { if (addrValid === true) setEditingAddr(false) }, [addrValid])
  useEffect(() => { setEditingAddr(false) }, [variant])

  // Enter validates immediately rather than waiting for the debounce, and must
  // not submit the surrounding form.
  const onAddrKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'Enter') return
    e.preventDefault()
    runValidation(address)
  }

  // Inline activation of a network wallet, mirroring the Receive picker.
  const inlineActivate = async (v: AssetVariant) => {
    const netID = networkID(v)
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
      (isVariantActive(a) ? 0 : 1) - (isVariantActive(b) ? 0 : 1))
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
            if (isVariantActive(v)) {
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
  const assetRate = app.idRate(variant.assetID) ?? 0
  const feeRate = app.idRate(networkID(variant)) ?? 0
  const feeCFactor = feeUI?.conventional.conversionFactor ?? 1
  // When the network picker was skipped (single-network asset), Back returns
  // to the asset picker rather than the picker we never showed.
  const back = ta.variants.length === 1 ? backToAssets : backToNetwork
  // Total debited and balance-after, in the sent asset's units.
  const recipientGets = subtract ? Math.max(0, value - nativeFee) : value
  const balanceAfter = Math.max(0, available - totalDebit)
  // A validated address collapses to a read-only wrapped display; a failed one
  // keeps the input open with an orange outline.
  const showAddrDisplay = addrValid === true && !editingAddr
  const addrCls = addrValid === false ? 'send-addr--invalid' : ''

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
