import { FormEvent, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { postJSON } from '@caribou/shared/http'
import Spinner from './Spinner'
import { useModalBusy } from './modalBusy'

const noSubmit = (e: FormEvent) => e.preventDefault()

// RecoverSwaps confirms and runs POST /api/intents/reconstruct, which
// rebuilds swap records lost with local state (e.g. after a seed restore) by
// probing the swap service for hits against recent outgoing-transaction
// recipients. The privacy tradeoff is disclosed before the request fires.
// Recovered records arrive as intentsswap notes, so open views update live.
export default function RecoverSwaps () {
  const close = () => app.setForm({ form: '', data: {} })

  const [recovering, setRecovering] = useState(false)
  const [errMsg, setErrMsg] = useState('')
  const [found, setFound] = useState<number | null>(null)
  useModalBusy(recovering)

  const recover = async () => {
    setRecovering(true)
    setErrMsg('')
    const r = await postJSON('/api/intents/reconstruct')
    setRecovering(false)
    if (!r.ok) {
      setErrMsg(r.errMsg || r.msg || t('Failed to recover swaps.'))
      return
    }
    setFound(r.found ?? 0)
  }

  return (
    <form className="modal form-width-300" onSubmit={noSubmit}>
      <header>
        <span className="ico-swap me-2 fs18" />
        <span>{t('Recover swaps')}</span>
      </header>
      {found === null
        ? (
          <>
            <div className="p-3 fs13">
              <p className="grey mb-2">
                {t('If swap history was lost — for example after restoring the wallet from your seed — Caribou can attempt to rebuild it.')}
              </p>
              <ul className="grey ps-3 mb-0">
                <li>{t('Recovery sends the recipient addresses of recent outgoing transactions — up to 25 per wallet, across all connected wallets — to the swap service, 1click.chaindefuser.com, asking whether each was a swap deposit address.')}</li>
                <li>{t('Most of these addresses are ordinary payment recipients, not swap deposits, and the service can associate them with one another and with the request origin.')}</li>
                <li>{t("Requests honor the wallet's Tor proxy, if one is configured, which limits network-level association.")}</li>
                <li>{t('This can take a while — the service is rate-limited.')}</li>
              </ul>
              {errMsg && (
                <div key={errMsg} className="form-err errcolor fs12 pt-2 text-center">{errMsg}</div>
              )}
            </div>
            <div className="flex-stretch-column p-3 pt-0">
              <button type="button" className="feature" onClick={recover} disabled={recovering}>
                {recovering
                  ? (
                    <span className="d-inline-flex align-items-center justify-content-center">
                      <Spinner className="me-2" />{t('Recovering...')}
                    </span>
                    )
                  : t('Recover swaps')}
              </button>
              <button type="button" className="mt-2" onClick={close} disabled={recovering}>{t('Cancel')}</button>
            </div>
          </>
          )
        : (
          <>
            <div className="p-3 fs13 text-center">
              {found > 0
                ? tvar('Recovered {{ n }} swap record(s).', { n: String(found) })
                : t('No additional swap records were found.')}
            </div>
            <div className="p-3 pt-0">
              <button type="button" className="feature w-100" onClick={close}>{t('Close')}</button>
            </div>
          </>
          )}
    </form>
  )
}
