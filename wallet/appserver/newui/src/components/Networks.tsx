import { useState, useEffect, useCallback } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import { CoreNote } from '@bossw/shared/registry'
import { networkList, NetworkSummary } from '@bossw/shared/assets'
import CoinLogo from '@bossw/shared/components/CoinLogo'

// Cap on the number of asset logos shown per card. When exceeded, the last slot
// becomes a "+N" overflow chip so the row stays compact.
const MAX_ASSET_ICONS = 10

// NetworkCard renders one network as a clickable tile. Activated networks open
// the network detail view; unactivated networks open the activation dialog.
function NetworkCard ({ n }: { n: NetworkSummary }) {
  const onClick = () => {
    if (n.activated) {
      app.loadPage(`networks/${n.nativeAssetID}`)
    } else {
      app.setForm({ form: 'activatenetwork', data: { assetID: n.nativeAssetID } })
    }
  }

  const assetWord = n.assetCount === 1 ? t('asset') : t('assets')

  const shownAssets = n.supportedAssets.length > MAX_ASSET_ICONS
    ? n.supportedAssets.slice(0, MAX_ASSET_ICONS - 1)
    : n.supportedAssets
  const overflow = n.supportedAssets.length - shownAssets.length

  return (
    <button type="button" className="network-card" onClick={onClick}>
      <div className="network-card-header">
        <CoinLogo
          ticker={n.ticker}
          className={`network-card-logo ${n.activated ? '' : 'network-card-logo--off'}`}
        />
        <span className="network-card-name">{n.name}</span>
      </div>
      <div className="network-card-meta">
        {tvar('Supports {{ count }} {{ assetWord }}', { count: String(n.assetCount), assetWord })}
      </div>
      <div className="network-card-assets">
        {shownAssets.map(a => (
          <CoinLogo key={a.assetID} ticker={a.ticker} title={a.ticker} className="network-card-asset-icon" />
        ))}
        {overflow > 0 && <span className="network-card-asset-more">+{overflow}</span>}
      </div>
      <div className="network-card-footer">
        {n.activated
          ? (
            <span className="network-status network-status--on">
              <span className="network-status-dot" />
              {t('Activated')}
            </span>
            )
          : (
            <span className="network-status network-status--off">
              <span className="network-status-dot" />
              {t('Not activated')}
            </span>
            )}
      </div>
    </button>
  )
}

export default function Networks () {
  const [, setTick] = useState(0)
  const redraw = useCallback(() => setTick(n => n + 1), [])

  // Networks activate/deactivate via wallet state changes — redraw so the
  // status badges and click behavior stay current (e.g. after an activation
  // completes elsewhere, or in the activation dialog opened from here).
  useEffect(() => {
    const noteK = app.registerNoteFeed((note: CoreNote) => {
      switch (note.type) {
        case 'walletstate':
        case 'walletconfig':
          redraw()
          break
      }
    })
    return () => app.unregisterNoteFeed(noteK)
  }, [redraw])

  const networks = networkList()

  return (
    <div className="networks-view fill-abs">
      <div className="networks-inner">
        <div className="networks-title fs22">{t('Networks')}</div>
        <p className="grey fs13 networks-subtitle">
          {t('Networks are the blockchains that settle your assets. Activate a network to create its wallet.')}
        </p>
        <div className="networks-grid">
          {networks.map(n => <NetworkCard key={n.nativeAssetID} n={n} />)}
        </div>
      </div>
    </div>
  )
}
