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
      app.loadPage(`networks/${n.symbol}`)
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
    <button type="button" className="network-card d-flex flex-column border pointer text-left" onClick={onClick}>
      <div className="network-card-header d-flex align-items-center">
        <CoinLogo
          ticker={n.ticker}
          className={`icon-32 ${n.activated ? '' : 'logo-off'}`}
        />
        <span className="network-card-name demi">{n.name}</span>
      </div>
      <div className="fs13 grey">
        {tvar('Supports {{ count }} {{ assetWord }}', { count: String(n.assetCount), assetWord })}
      </div>
      <div className="network-card-assets d-flex align-items-center">
        {shownAssets.map(a => (
          <CoinLogo key={a.assetID} ticker={a.ticker} title={a.ticker} className="icon-16" />
        ))}
        {overflow > 0 && <span className="fs11 demi grey">+{overflow}</span>}
      </div>
      <div className="d-flex align-items-center mt-auto pt-1">
        {n.activated
          ? (
            <span className="network-status network-status--on d-inline-flex align-items-center fs12 demi">
              <span className="network-status-dot" />
              {t('Activated')}
            </span>
            )
          : (
            <span className="network-status network-status--off d-inline-flex align-items-center fs12 demi">
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
      <div className="networks-inner mx-auto w-100">
        <div className="demi fs22">{t('Networks')}</div>
        <p className="grey fs13 mt-1 mb-4">
          {t('Networks are the blockchains that settle your assets. Activate a network to create its wallet.')}
        </p>
        <div className="networks-grid d-grid">
          {networks.map(n => <NetworkCard key={n.nativeAssetID} n={n} />)}
        </div>
      </div>
    </div>
  )
}
