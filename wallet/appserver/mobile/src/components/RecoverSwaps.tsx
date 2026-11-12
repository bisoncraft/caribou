import { useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import { postJSON } from '@caribou/shared/http'
import { Sheet, Spinner } from './ui'

// RecoverSwapsSheet confirms and runs POST /api/intents/reconstruct, which
// rebuilds swap records lost with local state (e.g. after a seed restore) by
// probing the swap service for hits against recent outgoing-transaction
// recipients. The privacy tradeoff is disclosed before the request fires.
// Recovered records arrive as intentsswap notes, so open views update live.
export default function RecoverSwapsSheet ({ onClose }: { onClose: () => void }) {
  const [recovering, setRecovering] = useState(false)
  const [err, setErr] = useState('')
  const [found, setFound] = useState<number | null>(null)

  const recover = async () => {
    setRecovering(true)
    setErr('')
    const r = await postJSON('/api/intents/reconstruct')
    setRecovering(false)
    if (!r.ok) {
      setErr(r.errMsg || r.msg || t('Failed to recover swaps.'))
      return
    }
    setFound(r.found ?? 0)
  }

  if (found !== null) {
    return (
      <Sheet title={t('Recover swaps')} onClose={onClose} foot={<button type='button' className='btn' onClick={onClose}>{t('Close')}</button>}>
        <p className='center fs15 pt8'>
          {found > 0
            ? tvar('Recovered {{ n }} swap record(s).', { n: String(found) })
            : t('No additional swap records were found.')}
        </p>
      </Sheet>
    )
  }

  return (
    <Sheet
      title={t('Recover swaps')}
      onClose={() => { if (!recovering) onClose() }}
      foot={
        <>
          {err && <div className='err-box' aria-live='polite'>{err}</div>}
          <button type='button' className='btn' onClick={recover} disabled={recovering}>
            {recovering ? <><Spinner />{t('Recovering...')}</> : t('Recover swaps')}
          </button>
        </>
      }
    >
      <div className='prose'>
        <p>{t('If swap history was lost, for example after restoring the wallet from a seed, Caribou can try to rebuild it.')}</p>
        <ul>
          <li>{t('Recovery sends the recipient addresses of recent outgoing transactions (up to 25 per wallet, across all connected wallets) to the swap service, 1click.chaindefuser.com, asking whether each was a swap deposit address.')}</li>
          <li>{t('Most of these addresses are ordinary payment recipients, not swap deposits, and the service can associate them with one another and with the request origin.')}</li>
          <li>{t("Requests honor the wallet's Tor proxy, if one is configured, which limits network-level association.")}</li>
          <li>{t('The service is rate-limited, so this can take a while.')}</li>
        </ul>
      </div>
    </Sheet>
  )
}
