import { useState, useEffect, useCallback } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { postJSON } from '@caribou/shared/http'
import { CoreNote } from '@caribou/shared/registry'
import { TickerAsset, AssetVariant, networkID } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'

interface ActivateProps {
  onClose: () => void
}

// nativeVariant prefers the non-token variant (the settlement chain).
function nativeVariant (ta: TickerAsset): AssetVariant {
  return ta.variants.find(v => !v.token) ?? ta.variants[0]
}

// Activate lists assets without a wallet and creates one on tap, using the
// asset's default wallet definition (the same minimal path the desktop inline
// activation uses). Token multi-network selection is deferred; this activates
// the asset's primary settlement network.
export default function Activate ({ onClose }: ActivateProps) {
  const [, setTick] = useState(0)
  const [busy, setBusy] = useState<string | null>(null)
  const [errs, setErrs] = useState<Record<string, string>>({})

  // The walletstate/walletconfig note that flips a ticker's hasWallets arrives
  // asynchronously over the WS, racing the /api/newwallet HTTP response. Without
  // subscribing, a just-activated asset can linger in the inactive list because
  // app.reRender is never driven by the note feed. Redraw on wallet notes so the
  // activated row drops out as soon as core confirms the wallet.
  const redraw = useCallback(() => setTick(n => n + 1), [])
  useEffect(() => {
    const noteK = app.registerNoteFeed((n: CoreNote) => {
      switch (n.type) {
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
          redraw()
          break
      }
    })
    return () => app.unregisterNoteFeed(noteK)
  }, [redraw])

  const inactive = (app.tickerList ?? []).filter(ta => !ta.hasWallets)

  const activate = async (ta: TickerAsset) => {
    if (busy) return
    const v = nativeVariant(ta)
    const netID = networkID(v)
    const walletDef = app.user?.assets[netID]?.info?.availablewallets?.[0]
    if (!walletDef) {
      setErrs(prev => ({ ...prev, [ta.ticker]: t('Activation for this asset is not yet supported.') }))
      return
    }
    setBusy(ta.ticker)
    setErrs(prev => { const next = { ...prev }; delete next[ta.ticker]; return next })
    const r = await postJSON('/api/newwallet', { assetID: netID, walletType: walletDef.type, config: {} })
    setBusy(null)
    if (!r.ok) {
      setErrs(prev => ({ ...prev, [ta.ticker]: r.errMsg || r.msg || t('Failed to activate asset') }))
    }
    // On success the wallet note feed (subscribed above) redraws this view and
    // the row drops out of the inactive list once core confirms the wallet.
  }

  return (
    <div className='mb-overlay'>
      <header className='mb-overlay-head'>
        <button className='mb-back' type='button' onClick={onClose} aria-label={t('Back')}>‹</button>
        <span>{t('Activate New Assets')}</span>
      </header>
      <div className='mb-overlay-body'>
        {inactive.length === 0
          ? <p className='mb-muted mb-pad'>{t('No assets yet')}</p>
          : (
            <ul className='mb-asset-list'>
              {inactive.map(ta => (
                <li key={ta.ticker} className='mb-asset-row'>
                  <CoinLogo ticker={ta.ticker} alt={ta.name} className='mb-coin-logo' />
                  <div className='mb-asset-id'>
                    <span className='mb-asset-ticker'>{ta.ticker}</span>
                    <span className='mb-asset-name mb-muted'>{ta.name}</span>
                  </div>
                  <button
                    className='mb-btn mb-btn-small'
                    type='button'
                    onClick={() => activate(ta)}
                    disabled={busy !== null}
                  >
                    {busy === ta.ticker ? t('Activating...') : t('Activate')}
                  </button>
                  {errs[ta.ticker] && <div className='mb-err mb-row-err'>{errs[ta.ticker]}</div>}
                </li>
              ))}
            </ul>
            )}
      </div>
    </div>
  )
}
