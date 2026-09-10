import { useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { AssetVariant, networkAsset } from '@caribou/shared/assets'
import { ApprovalStatus, TrustlineState } from '@caribou/shared/registry'
import PasswordInput from './PasswordInput'
import Spinner from './Spinner'

interface TrustlineGateProps {
  // variant is a token whose wallet reports a trust-line state other than
  // Approved. The gate replaces the deposit address until the line exists.
  variant: AssetVariant
  state: TrustlineState
}

// TrustlineGate is the Receive step for a token that needs an on-chain opt-in
// (an XRPL trust line) before its address can accept deposits. It explains the
// cost, offers the opt-in when the account can afford it, points at the
// network asset when it cannot, and shows progress while the opt-in confirms.
// State changes arrive through the wallet's trust-line note; the parent
// re-renders on them.
export default function TrustlineGate ({ variant, state }: TrustlineGateProps) {
  const network = networkAsset(variant)
  const netUI = network?.unitInfo
  const netTicker = netUI?.conventional.unit ?? ''
  const netName = network?.name ?? ''
  const token = variant.ticker
  const requirePW = app.appSettings.sendRequiresPassword
  const [pw, setPW] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState('')

  const establish = async () => {
    setSubmitting(true)
    setErr('')
    const r = await postJSON('/api/establishtrustline', { assetID: variant.assetID, pass: pw })
    setSubmitting(false)
    setPW('')
    if (!r.ok) setErr(r.msg || t('Failed to establish trust line.'))
    // On success the wallet announces Pending (or Approved, if the line
    // already existed) and the parent re-renders this step.
  }

  const receiveNetwork = () => app.setForm({ form: 'receive', data: { ticker: netTicker } })

  if (state.status === ApprovalStatus.Pending) {
    return (
      <div className="px-3 pb-3 text-center">
        <div className="d-flex align-items-center justify-content-center fs14 pb-2">
          <Spinner size={14} className="me-2" />
          {t('Establishing trust line...')}
        </div>
        <div className="grey fs13">
          {tvar('Your {{ token }} address will appear once the network confirms the trust line.', { token })}
        </div>
      </div>
    )
  }

  const reserve = Doc.formatCoinValue(state.reserve, netUI)
  return (
    <div className="px-3 pb-3">
      <div className="fs13 pb-2">
        {tvar('Receiving {{ token }} on {{ network }} requires a trust line. Establishing one keeps {{ reserve }} {{ ticker }} of your balance reserved for as long as you can hold {{ token }}, plus a small network fee. The reserve is released if you later remove the trust line.',
          { token, network: netName, reserve, ticker: netTicker })}
      </div>
      {state.shortfall > 0
        ? (
          <>
            <div className="fs13 pb-2 gold">
              {tvar('Your {{ network }} wallet needs at least {{ amount }} more {{ ticker }} before it can hold {{ token }}.',
                { network: netName, amount: Doc.formatCoinValue(state.shortfall, netUI), ticker: netTicker, token })}
            </div>
            <div className="flex-stretch-column">
              <button type="button" onClick={receiveNetwork}>
                {tvar('Receive {{ ticker }}', { ticker: netTicker })}
              </button>
            </div>
          </>
          )
        : (
          <>
            {requirePW && (
              <div className="pb-2">
                <label className="fs12 grey">{t('App password')}</label>
                <PasswordInput value={pw} onChange={e => setPW(e.target.value)} disabled={submitting} />
              </div>
            )}
            {err && <div key={err} className="errcolor fs12 pb-2 text-center">{err}</div>}
            <div className="flex-stretch-column">
              <button type="button" className="feature" onClick={establish} disabled={submitting || (requirePW && !pw)}>
                {submitting
                  ? <span className="d-inline-flex align-items-center justify-content-center"><Spinner className="me-2" />{t('Establishing trust line...')}</span>
                  : t('Establish trust line')}
              </button>
            </div>
          </>
          )}
    </div>
  )
}
