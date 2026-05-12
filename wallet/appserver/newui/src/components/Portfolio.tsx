import { useState, useEffect, useCallback } from 'react'
import app from '../js/application'
import { CoreNote } from '../js/registry'
import { TickerAsset } from '../js/assets'
import PortfolioTotals from './PortfolioTotals'
import AssetPills from './AssetPills'
import AssetRow from './AssetRow'
import InactiveAssetRow from './InactiveAssetRow'

export default function Portfolio () {
  const [, setTick] = useState(0)
  const [selectedTicker, setSelectedTicker] = useState<string | null>(null)
  const [highlightedTicker, setHighlightedTicker] = useState<string | null>(null)

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
          break
      }
    })
    return () => {
      app.unregisterBalanceUpdater(balK)
      app.unregisterFiatRateUpdater(rateK)
      app.unregisterNoteFeed(noteK)
    }
  }, [redraw])

  const handlePillSelect = (ticker: string) => {
    setSelectedTicker(prev => prev === ticker ? null : ticker)
    setHighlightedTicker(ticker)
    // Clear the highlight flag after animation completes
    setTimeout(() => setHighlightedTicker(null), 900)
  }

  const totals = app.portfolioTotals()
  const tickerList: TickerAsset[] = app.tickerList ?? []

  const walletAssets = tickerList.filter(ta => ta.hasWallets)
  const inactiveAssets = tickerList.filter(ta => !ta.hasWallets)

  // Sort active assets by USD value descending
  const fiatRates = app.user?.fiatRates ?? {}
  const sortedWalletAssets = [...walletAssets].sort((a, b) => {
    const aRate = fiatRates[a.bestID] ?? 0
    const bRate = fiatRates[b.bestID] ?? 0
    const aVal = (a.total / a.cFactor) * aRate
    const bVal = (b.total / b.cFactor) * bRate
    return bVal - aVal
  })

  // Count distinct inactive assets
  const inactiveCount = inactiveAssets.length
  // TODO: count unique networks among inactive assets if network info becomes available per TickerAsset.
  const networkCount = 5 // placeholder — see TODO above

  return (
    <div className="portfolio-view fill-abs" style={{ overflowY: 'auto' }}>
      <div className="portfolio-inner">
        <PortfolioTotals totals={totals} walletAssets={sortedWalletAssets} />

        {walletAssets.length > 0 && (
          <AssetPills
            walletAssets={sortedWalletAssets}
            selectedTicker={selectedTicker}
            onSelect={handlePillSelect}
          />
        )}

        {walletAssets.length > 0 && (
          <table className="asset-table w-100">
            <thead>
              <tr className="asset-table-header">
                <th className="asset-th-asset">ASSETS</th>
                <th className="asset-th-num">BALANCE</th>
                <th className="asset-th-num">VALUE</th>
                <th className="asset-th-num">PRICE</th>
                <th className="asset-th-num">PORTFOLIO %</th>
              </tr>
            </thead>
            <tbody>
              {sortedWalletAssets.map(ta => {
                const rate = fiatRates[ta.bestID] ?? 0
                const usdVal = (ta.total / ta.cFactor) * rate
                const pct = totals.total > 0 ? (usdVal / totals.total) * 100 : null
                return (
                  <AssetRow
                    key={ta.ticker}
                    asset={ta}
                    fiatRate={rate}
                    portfolioPct={pct}
                    isHighlighted={highlightedTicker === ta.ticker}
                  />
                )
              })}
            </tbody>
          </table>
        )}

        {inactiveCount > 0 && (
          <>
            <div className="portfolio-section-header">ACTIVATE NEW ASSETS</div>
            <p className="portfolio-section-copy grey fs13">
              Bison Wallet supports {inactiveCount} other asset{inactiveCount !== 1 ? 's' : ''} across {networkCount} networks with more on the roadmap.
            </p>
            <table className="asset-table w-100">
              <tbody>
                {inactiveAssets.map(ta => (
                  <InactiveAssetRow key={ta.ticker} asset={ta} />
                ))}
              </tbody>
            </table>
          </>
        )}
      </div>
    </div>
  )
}
