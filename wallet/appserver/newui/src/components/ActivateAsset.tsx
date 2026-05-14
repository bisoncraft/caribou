import { useState } from 'react'
import t, { tvar } from '../js/intl'
import app from '../js/application'
import Doc from '../js/doc'
import { postJSON } from '../js/http'

interface ActivateAssetParams {
  assetID: number
}

export default function ActivateAsset ({ assetID }: ActivateAssetParams) {
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

  const walletDef = asset.info?.availablewallets?.[0]
  if (!walletDef) {
    return (
      <form className="modal form-width-300">
        <header>
          <img src={Doc.logoPath(asset.unitInfo.conventional.unit)} className="mini-icon me-2" />
          <span>{asset.name}</span>
        </header>
        <div className="px-3 pb-3 grey text-center">
          {t('Activation for this asset is not yet supported.')}
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
      walletType: walletDef.type,
      config: {}
    })
    setSubmitting(false)
    if (!r.ok) {
      setErr(r.msg || t('Failed to activate asset'))
      return
    }
    close()
  }

  return (
    <form className="modal form-width-300">
      <header>
        <img src={Doc.logoPath(asset.unitInfo.conventional.unit)} className="mini-icon me-2" />
        <span>{tvar('Activate {{ name }}', { name: asset.name })}</span>
      </header>
      <div className="px-3 pb-2 fs14 grey">
        {walletDef.description}
      </div>
      <div className={`flex-center errcolor px-3 pb-2 ${err ? '' : 'd-none'}`}>{err}</div>
      <div className="flex-stretch-column p-3">
        <button className="feature" onClick={submit} disabled={submitting}>
          {submitting ? t('Activating...') : t('Activate Now')}
        </button>
        <button className="mt-2" onClick={close} disabled={submitting}>{t('Cancel')}</button>
      </div>
    </form>
  )
}
