import { useEffect, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { useForceUpdate, useCoreSubscriptions } from '@caribou/shared/hooks'
import { FeeState, UnitInfo } from '@caribou/shared/registry'
import { networkSummary, normalizedTicker } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import NetworkedTokenLogo from '@caribou/shared/components/NetworkedTokenLogo'
import { useNav } from '../nav'
import { Icon, NavHeader, Spinner } from './ui'

// FeeRows shows the network fee rate and the cost of a standard send. Fee
// state is computed on demand (it is not part of WalletState), so it is
// fetched from /api/feestate on mount.
function FeeRows ({ assetID, ui, ticker }: { assetID: number, ui: UnitInfo, ticker: string }) {
  const [fee, setFee] = useState<FeeState | null>(null)
  const [loaded, setLoaded] = useState(false)
  useEffect(() => {
    let stale = false
    postJSON('/api/feestate', { assetID }).then(r => {
      if (stale) return
      setFee(r.ok && r.feeState?.rate ? r.feeState : null)
      setLoaded(true)
    })
    return () => { stale = true }
  }, [assetID])

  const rate = app.idRate(assetID) ?? 0
  const unavailable = loaded ? t('Unavailable') : '—'
  const sendUsd = fee && rate > 0 ? Doc.usdString(fee.send, ui.conventional.conversionFactor, rate) : ''
  return (
    <div className='list'>
      <div className='kv'>
        <span>{t('Network fee rate')}</span>
        <span className='mono'>{fee ? `${Doc.formatInt(fee.rate)} ${ui.atomicUnit}/${ui.feeRateDenom}` : unavailable}</span>
      </div>
      <div className='kv'>
        <span>{t('Cost of a standard send')}</span>
        <span className='mono'>
          {fee ? `${Doc.formatCoinValue(fee.send, ui)} ${ticker}` : unavailable}
          {sendUsd && <><br /><span className='grey'>{sendUsd}</span></>}
        </span>
      </div>
    </div>
  )
}

// NetworkScreen is the detail view for one settlement network: the native
// asset's balance, the tokens that settle on it, sync status, and fees.
export default function NetworkScreen ({ assetID }: { assetID: number }) {
  const nav = useNav()
  const redraw = useForceUpdate()
  useCoreSubscriptions({
    onBalance: redraw,
    onRate: redraw,
    onNote: (note) => {
      switch (note.type) {
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
          redraw()
      }
    }
  }, [redraw])

  const n = networkSummary(assetID)
  if (!n) {
    return (
      <div className='frame'>
        <NavHeader title={t('Unknown network')} onBack={nav.back} />
        <div className='empty'>{t('Unknown network')}</div>
      </div>
    )
  }

  const wallet = app.wallet(assetID)
  const ui = app.unitInfo(assetID)
  const tokens = n.supportedAssets.filter(a => a.assetID !== assetID)
  const avail = wallet?.balance?.available ?? 0
  const rate = app.idRate(assetID) ?? 0
  const usd = rate > 0 ? Doc.usdString(avail, ui.conventional.conversionFactor, rate) : ''
  const ss = wallet?.syncStatus
  const openAsset = (id: number) => {
    const a = app.user?.assets[id]
    if (a) nav.push({ kind: 'asset', ticker: normalizedTicker(a) })
  }

  return (
    <div className='frame'>
      <NavHeader title={tvar('{{name}} Network', { name: n.name })} onBack={nav.back} />
      <div className='scroll'>
        <div className='body'>
          <button type='button' className='net-hero' onClick={() => openAsset(assetID)}>
            <CoinLogo ticker={n.ticker} alt='' className='coin-xl' />
            <div className='asset-val mono'>{Doc.formatCoinValue(avail, ui)} {n.ticker}</div>
            {usd && <div className='asset-bal'>{usd}</div>}
          </button>

          {tokens.length > 0 && (
            <>
              <h2 className='lbl sec'>{t('Tokens')}</h2>
              <div className='list'>
                {tokens.map(tk => {
                  const a = app.asset(tk.assetID)
                  const tui = a.unitInfo
                  const bal = a.wallet?.balance?.available ?? 0
                  const tRate = app.idRate(tk.assetID) ?? 0
                  return (
                    <button key={tk.assetID} type='button' className='row tappable' onClick={() => openAsset(tk.assetID)}>
                      <NetworkedTokenLogo token={tk.ticker} network={n.ticker} className='coin' />
                      <div className='grow'><div className='t1'>{a.name}</div><div className='t2'>{tk.ticker}</div></div>
                      <div className='num'>
                        <div className='mono fs15'>{Doc.formatCoinValue(bal, tui)} {tk.ticker}</div>
                        {tRate > 0 && <div className='t2'>{Doc.usdString(bal, tui.conventional.conversionFactor, tRate)}</div>}
                      </div>
                    </button>
                  )
                })}
              </div>
            </>
          )}

          {wallet
            ? (
              <>
                <h2 className='lbl sec'>{t('Network status')}</h2>
                <div className='list'>
                  <div className='kv'>
                    <span>{t('Status')}</span>
                    {wallet.synced
                      ? <span className='pos'><span className='dot' />{t('Synced')}</span>
                      : <span className='gold kv-logo'><Spinner gold small />{tvar('syncing_pct', { pct: String(Math.round((wallet.syncProgress ?? 0) * 100)) })}</span>}
                  </div>
                  <div className='kv'><span>{t('Network type')}</span><span>{wallet.class || '—'}</span></div>
                  <div className='kv'><span>{t('Peers')}</span><span className='mono'>{Doc.formatInt(wallet.peerCount ?? 0)}</span></div>
                  <div className='kv'>
                    <span>{t('Block height')}</span>
                    <span className='mono'>
                      {wallet.synced
                        ? Doc.formatInt(ss?.blocks ?? 0)
                        : `${Doc.formatInt(ss?.blocks ?? 0)} / ${Doc.formatInt(ss?.targetHeight ?? 0)}`}
                    </span>
                  </div>
                </div>
                <h2 className='lbl sec'>{t('Fees')}</h2>
                <FeeRows assetID={assetID} ui={ui} ticker={n.ticker} />
              </>
              )
            : <div className='empty'><Icon name='network' className='empty-ico' /><div className='mt12'>{t('This network is not activated.')}</div></div>}
        </div>
      </div>
    </div>
  )
}
