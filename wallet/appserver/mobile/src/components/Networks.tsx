import { useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { useForceUpdate, useCoreSubscriptions } from '@caribou/shared/hooks'
import { networkList, NetworkSummary } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import { useNav } from '../nav'
import { Icon, Spinner } from './ui'
import { activateNetwork } from './AddAssetsSheet'

function assetCount (n: NetworkSummary): string {
  return n.assetCount === 1 ? t('1 asset') : tvar('{{ n }} assets', { n: String(n.assetCount) })
}

// SyncLine is an active network's status: a green "Synced" dot, or a gold
// progress bar while syncing.
export function SyncLine ({ assetID, suffix }: { assetID: number, suffix?: string }) {
  const w = app.wallet(assetID)
  if (!w) return null
  if (!w.synced) {
    const pct = Math.round((w.syncProgress ?? 0) * 100)
    return (
      <div className='sync-line'>
        <span className='bar grow'><span style={{ width: `${pct}%` }} /></span>
        <span className='gold demi fs12'>{tvar('syncing_pct', { pct: String(pct) })}</span>
      </div>
    )
  }
  return (
    <div className='t2'>
      <span className='pos'><span className='dot' />{t('Synced')}</span>
      {suffix && <> · {suffix}</>}
    </div>
  )
}

function InactiveNetworkRow ({ n }: { n: NetworkSummary }) {
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const activate = async () => {
    setBusy(true)
    setErr('')
    const e = await activateNetwork(n.nativeAssetID)
    setBusy(false)
    if (e) setErr(e)
    // On success the walletstate note redraws the list and the network moves
    // to the active section.
  }
  return (
    <div>
      <div className='row compact'>
        <CoinLogo ticker={n.ticker} alt='' className='net-logo off' />
        <div className='grow'><div className='t1'>{n.name}</div><div className='t2'>{assetCount(n)}</div></div>
        {busy
          ? <span className='activating'><Spinner />{t('Activating')}</span>
          : <button type='button' className='chip outline' onClick={activate}>{t('Activate')}</button>}
      </div>
      {err && <div className='row-err'>{err}</div>}
    </div>
  )
}

// Networks lists the settlement networks: active ones with their sync state
// (each opens its detail view), then the rest with an Activate control.
export default function Networks () {
  const nav = useNav()
  const redraw = useForceUpdate()
  useCoreSubscriptions({
    onNote: (note) => {
      switch (note.type) {
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
          redraw()
      }
    }
  }, [redraw])

  const networks = networkList()
  const active = networks.filter(n => n.activated)
  const inactive = networks.filter(n => !n.activated)

  return (
    <div className='scroll'>
      <div className='page'>
        <h1 className='page-title'>{t('Networks')}</h1>
        <p className='page-sub'>{t('Networks are the blockchains that settle your assets. Activate a network to create its wallet.')}</p>

        {active.length > 0 && (
          <>
            <h2 className='lbl sec'>{t('Active')}</h2>
            <div className='list'>
              {active.map(n => (
                <button key={n.nativeAssetID} type='button' className='row compact tappable' onClick={() => nav.push({ kind: 'network', assetID: n.nativeAssetID })}>
                  <CoinLogo ticker={n.ticker} alt='' className='net-logo' />
                  <div className='grow'>
                    <div className='t1'>{n.name}</div>
                    <SyncLine assetID={n.nativeAssetID} suffix={assetCount(n)} />
                  </div>
                  <Icon name='chevron-left' className='chev' />
                </button>
              ))}
            </div>
          </>
        )}

        {inactive.length > 0 && (
          <>
            <h2 className='lbl sec'>{t('Not activated')}</h2>
            <div className='list'>
              {inactive.map(n => <InactiveNetworkRow key={n.nativeAssetID} n={n} />)}
            </div>
          </>
        )}
      </div>
    </div>
  )
}
