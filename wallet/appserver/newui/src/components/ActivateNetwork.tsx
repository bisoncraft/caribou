import { useState, FormEvent } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import { postJSON } from '@bossw/shared/http'
import { networkSummary } from '@bossw/shared/assets'
import CoinLogo from '@bossw/shared/components/CoinLogo'
import Spinner from './Spinner'

interface ActivateNetworkParams {
  assetID: number
}

const noSubmit = (e: FormEvent) => e.preventDefault()

// ActivateNetwork confirms the user's intent to activate a network, creates the
// network wallet, and — on success — navigates to that network's detail view.
// The wallet is created with the network's default wallet definition (e.g.
// Electrum for BTC/LTC); the SPV/Electrum choice offered by the per-asset
// activation flow is intentionally not surfaced here.
export default function ActivateNetwork ({ assetID }: ActivateNetworkParams) {
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState('')

  const close = () => app.setForm({ form: '', data: {} })

  const n = networkSummary(assetID)
  const asset = app.user?.assets[assetID]
  const walletDef = asset?.info?.availablewallets?.[0]

  if (!n || !walletDef) {
    return (
      <form className="modal form-width-300" onSubmit={noSubmit}>
        <header>
          <span>{t('Activate Network')}</span>
        </header>
        <div className="px-3 pb-3 grey text-center">
          {t('Activation for this network is not yet supported.')}
        </div>
        <div className="flex-stretch-column p-3">
          <button type="button" onClick={close}>{t('Close')}</button>
        </div>
      </form>
    )
  }

  const submit = async () => {
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/newwallet', {
      assetID,
      walletType: walletDef.type,
      config: {}
    })
    if (!r.ok) {
      setSubmitting(false)
      setErr(r.errMsg || r.msg || t('Failed to activate network'))
      return
    }
    close()
    app.loadPage(`networks/${app.asset(assetID).symbol}`)
  }

  return (
    <form className="modal form-width-300" onSubmit={noSubmit}>
      <header>
        <CoinLogo ticker={n.ticker} className="mini-icon me-2" />
        <span>{tvar('Activate {{ name }}', { name: n.name })}</span>
      </header>
      <div className="px-3 pb-3 fs14 grey text-center">
        {tvar('Activate the {{ name }} network to create its wallet?', { name: n.name })}
      </div>
      <div aria-live="polite">
        {err && (
          <div key={err} className="form-err flex-center errcolor px-3 pb-2 text-center fs13">
            {err}
          </div>
        )}
      </div>
      <div className="flex-stretch-column p-3">
        <button type="button" className="feature" onClick={submit} disabled={submitting}>
          {submitting ? <><Spinner className="me-2" />{t('Activating...')}</> : t('Activate Now')}
        </button>
        <button type="button" className="mt-2" onClick={close} disabled={submitting}>{t('Cancel')}</button>
      </div>
    </form>
  )
}
