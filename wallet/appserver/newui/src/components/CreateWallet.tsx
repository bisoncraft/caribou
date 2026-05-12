import { useState } from 'react'
import t from '../js/intl'
import app from '../js/application'
import Doc from '../js/doc'
import { postJSON } from '../js/http'

interface CreateWalletParams {
  assetID: number
}

const BTC_ASSET_ID = 0

export default function CreateWallet ({ assetID }: CreateWalletParams) {
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState('')

  const asset = app.user?.assets[assetID]
  if (!asset) {
    return (
      <form className="modal form-width-300">
        <div className="p-3 errcolor flex-center">{t('Unknown asset')}</div>
      </form>
    )
  }

  const close = () => app.setForm({ form: '', data: {} })

  if (assetID !== BTC_ASSET_ID) {
    return (
      <form className="modal form-width-300">
        <header>
          <img src={Doc.logoPath(asset.symbol)} className="mini-icon me-2" />
          <span>{asset.name}</span>
        </header>
        <div className="px-3 pb-3 grey text-center">
          {t('Wallet activation for this asset is not yet supported.')}
        </div>
        <div className="flex-stretch-column p-3">
          <button onClick={close}>{t('Close')}</button>
        </div>
      </form>
    )
  }

  const submit = async () => {
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/newwallet', {
      assetID,
      walletType: 'SPV',
      config: {}
    })
    setSubmitting(false)
    if (!r.ok) {
      setErr(r.msg || t('Failed to create wallet'))
      return
    }
    close()
  }

  return (
    <form className="modal form-width-300">
      <header>
        <img src={Doc.logoPath(asset.symbol)} className="mini-icon me-2" />
        <span>{t('Activate Bitcoin Wallet')}</span>
      </header>
      <div className="px-3 pb-2 fs14 grey">
        {t('A native SPV wallet will be created and begin syncing in the background.')}
      </div>
      <div className={`flex-center errcolor px-3 pb-2 ${err ? '' : 'd-none'}`}>{err}</div>
      <div className="flex-stretch-column p-3">
        <button className="feature" onClick={submit} disabled={submitting}>
          {submitting ? t('Creating...') : t('Create Wallet')}
        </button>
        <button className="mt-2" onClick={close} disabled={submitting}>{t('Cancel')}</button>
      </div>
    </form>
  )
}
