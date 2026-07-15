import { useEffect, useState, useCallback } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import Doc from '@bossw/shared/doc'
import { CoreNote } from '@bossw/shared/registry'
import { TickerAsset } from '@bossw/shared/assets'
import CoinLogo from '@bossw/shared/components/CoinLogo'

interface PortfolioProps {
  onSend: () => void
  onReceive: () => void
  onActivate: () => void
}

// Portfolio is the mobile balances view: a totals header and a single-column
// list of wallet assets sorted by fiat value. It reuses the shared core's
// portfolio aggregation and live updaters (balance / fiat-rate / wallet notes),
// the same data the desktop Portfolio renders.
export default function Portfolio ({ onSend, onReceive, onActivate }: PortfolioProps) {
  const [, setTick] = useState(0)
  const redraw = useCallback(() => setTick(n => n + 1), [])

  useEffect(() => {
    const balK = app.registerBalanceUpdater(redraw)
    const rateK = app.registerFiatRateUpdater(redraw)
    const noteK = app.registerNoteFeed((n: CoreNote) => {
      switch (n.type) {
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
          redraw()
      }
    })
    return () => {
      app.unregisterBalanceUpdater(balK)
      app.unregisterFiatRateUpdater(rateK)
      app.unregisterNoteFeed(noteK)
    }
  }, [redraw])

  const totals = app.portfolioTotals()
  const tickerList: TickerAsset[] = app.tickerList ?? []

  const walletAssets = tickerList.filter(ta => ta.hasWallets)
  walletAssets.sort((a, b) => {
    const aVal = a.total * (a.xcRate ?? 0)
    const bVal = b.total * (b.xcRate ?? 0)
    return bVal - aVal
  })
  const hasInactive = tickerList.some(ta => !ta.hasWallets)

  return (
    <section className='mb-portfolio'>
      <div className='mb-actions'>
        <button className='mb-btn' type='button' onClick={onSend}>{t('Send')}</button>
        <button className='mb-btn' type='button' onClick={onReceive}>{t('Receive')}</button>
      </div>
      <div className='mb-totals'>
        {totals.numWallets === 0
          ? (
            <div className='mb-muted mb-totals-empty'>{t('No assets yet')}</div>
            )
          : (
            <>
              <div className='mb-totals-value'>{Doc.formatUSD(totals.total)}</div>
              <div className='mb-totals-sub mb-muted'>
                {tvar('{{ amt }} Available', { amt: Doc.formatUSD(totals.available) })}
              </div>
            </>
            )}
      </div>

      {walletAssets.length > 0 && (
        <ul className='mb-asset-list'>
          {walletAssets.map(ta => {
            const rate = ta.xcRate ?? 0
            const usdVal = ta.total * rate
            return (
              <li key={ta.ticker} className='mb-asset-row'>
                <CoinLogo ticker={ta.ticker} alt={ta.name} className='mb-coin-logo' />
                <div className='mb-asset-id'>
                  <span className='mb-asset-ticker'>{ta.ticker}</span>
                  <span className='mb-asset-name mb-muted'>{ta.name}</span>
                </div>
                <div className='mb-asset-bal'>
                  <span className='mb-asset-fiat'>{Doc.formatUSD(usdVal)}</span>
                  <span className='mb-asset-amt mb-muted'>{Doc.formatConventionalValue(ta.total, ta.precision)} {ta.ticker}</span>
                </div>
              </li>
            )
          })}
        </ul>
      )}

      {hasInactive && (
        <button className='mb-btn mb-btn-secondary' type='button' onClick={onActivate}>
          {t('Activate New Assets')}
        </button>
      )}
    </section>
  )
}
