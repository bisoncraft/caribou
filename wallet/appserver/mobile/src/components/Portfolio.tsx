import { useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { useForceUpdate, useCoreSubscriptions } from '@caribou/shared/hooks'
import { TickerAsset, compareByFiatValue, isVariantActive } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import { useNav } from '../nav'
import { Icon, Spinner } from './ui'
import AddAssetsSheet from './AddAssetsSheet'

// syncProgress returns the sync fraction (0-1) of a ticker's primary wallet
// while it is syncing, or null once it is synced or has no wallet. The
// primary is the native (non-token) variant when there is one.
export function syncProgress (ta: TickerAsset): number | null {
  const primary = ta.variants.find(v => !v.token) ?? ta.variants[0]
  const w = primary ? app.user?.assets[primary.assetID]?.wallet : undefined
  if (!w || w.synced) return null
  return w.syncProgress ?? 0
}

// AssetActions is the row of round Receive / Send / Swap buttons.
export function AssetActions ({ ticker, swapDisabled }: { ticker?: string, swapDisabled?: boolean }) {
  const nav = useNav()
  return (
    <nav className='actions' aria-label={t('Actions')}>
      <button type='button' className='act' onClick={() => nav.push({ kind: 'receive', ticker })}>
        <span className='disc'><Icon name='qrcode' /></span>{t('Receive')}
      </button>
      <button type='button' className='act' onClick={() => nav.push({ kind: 'send', ticker })}>
        <span className='disc'><Icon name='send' /></span>{t('Send')}
      </button>
      <button type='button' className='act' disabled={swapDisabled} onClick={() => nav.push({ kind: 'swap', ticker })}>
        <span className='disc'><Icon name='swap' /></span>{t('Swap')}
      </button>
    </nav>
  )
}

function AssetListRow ({ ta }: { ta: TickerAsset }) {
  const nav = useNav()
  const rate = ta.xcRate ?? 0
  const sync = syncProgress(ta)
  const nets = ta.variants.filter(isVariantActive).length
  const sub = [rate > 0 ? Doc.formatUSD(rate) : '']
  if (nets > 1) sub.push(tvar('{{ n }} networks', { n: String(nets) }))
  return (
    <button type='button' className='row tappable' onClick={() => nav.push({ kind: 'asset', ticker: ta.ticker })}>
      <CoinLogo ticker={ta.ticker} alt='' className='coin' />
      <div className='grow'>
        <div className='t1 name-line'>
          <span className='ellipsis'>{ta.name}</span>
          {sync !== null && <Spinner gold small />}
        </div>
        {sync !== null
          ? <div className='t2 gold demi fs12'>{tvar('syncing_pct', { pct: String(Math.round(sync * 100)) })}</div>
          : <div className='t2'>{sub.filter(Boolean).join(' · ') || ta.ticker}</div>}
      </div>
      <div className='num'>
        <div className='t1'>{rate > 0 ? Doc.formatUSD(ta.total * rate) : '—'}</div>
        <div className='t2 mono'>{Doc.formatConventionalValue(ta.total, ta.precision)} {ta.ticker}</div>
      </div>
    </button>
  )
}

// Portfolio is the home tab: total value, the Receive / Send / Swap actions,
// and the list of held assets sorted by fiat value.
export default function Portfolio () {
  const redraw = useForceUpdate()
  const [adding, setAdding] = useState(false)

  useCoreSubscriptions({
    onBalance: redraw,
    onRate: redraw,
    onNote: (n) => {
      switch (n.type) {
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
        case 'balance':
          redraw()
      }
    }
  }, [redraw])

  const totals = app.portfolioTotals()
  const walletAssets = (app.tickerList ?? []).filter(ta => ta.hasWallets).sort(compareByFiatValue)
  const lockedPct = totals.available + totals.locked > 0
    ? Math.round(totals.locked / (totals.available + totals.locked) * 100)
    : 0

  return (
    <div className='scroll'>
      <header className='home-head'>
        <span className='mark' role='img' aria-label='Caribou' />
      </header>

      {totals.numWallets === 0
        ? (
          <section className='hero'>
            <div className='hero-empty'>{t('No assets yet')}</div>
            <div className='hero-sub'>{t('Activate an asset to get started.')}</div>
            <button type='button' className='btn mt24' onClick={() => setAdding(true)}>
              <Icon name='add' />{t('Add assets')}
            </button>
          </section>
          )
        : (
          <>
            <section className='hero' aria-label={t('Total value')}>
              <span className='lbl'>{t('Total value')}</span>
              <div className='hero-val'>{Doc.formatUSD(totals.total)}</div>
              <div className='hero-sub'>
                {tvar('{{ amt }} available', { amt: Doc.formatUSD(totals.available) })}
                {lockedPct > 0 && <> · {tvar('{{ pct }}% locked', { pct: String(lockedPct) })}</>}
              </div>
            </section>
            <AssetActions />
            <div className='sec-row home-sec'>
              <h2 className='lbl'>{t('Assets')}</h2>
              <button type='button' className='link add-link' onClick={() => setAdding(true)}>
                <Icon name='add' />{t('Add assets')}
              </button>
            </div>
            <div className='list gutter'>
              {walletAssets.map(ta => <AssetListRow key={ta.ticker} ta={ta} />)}
            </div>
            <div className='pt24' />
          </>
          )}

      {adding && <AddAssetsSheet onClose={() => setAdding(false)} />}
    </div>
  )
}
