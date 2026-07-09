import { useState, useEffect, useRef, useCallback } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { CoreNote } from '@caribou/shared/registry'
import { TickerAsset, networkID } from '@caribou/shared/assets'
import PortfolioTotals from './PortfolioTotals'
import AssetPills from './AssetPills'
import AssetRow from './AssetRow'
import InactiveAssetRow from './InactiveAssetRow'

export default function Portfolio () {
  const [, setTick] = useState(0)
  const [selectedTicker, setSelectedTicker] = useState<string | null>(null)
  const [highlightedTicker, setHighlightedTicker] = useState<string | null>(null)
  const highlightTimerRef = useRef<number | null>(null)

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
      if (highlightTimerRef.current !== null) {
        window.clearTimeout(highlightTimerRef.current)
      }
    }
  }, [redraw])

  const handlePillSelect = (ticker: string) => {
    const isCurrentlySelected = selectedTicker === ticker
    setSelectedTicker(prev => prev === ticker ? null : ticker)
    // Only flash the highlight when actually selecting, not when toggling off.
    if (isCurrentlySelected) return
    if (highlightTimerRef.current !== null) {
      window.clearTimeout(highlightTimerRef.current)
    }
    setHighlightedTicker(ticker)
    highlightTimerRef.current = window.setTimeout(() => {
      setHighlightedTicker(null)
      highlightTimerRef.current = null
    }, 900)
  }

  const totals = app.portfolioTotals()
  const tickerList: TickerAsset[] = app.tickerList ?? []

  const sortedWalletAssets: TickerAsset[] = []
  const inactiveAssets: TickerAsset[] = []
  for (const ta of tickerList) {
    if (ta.hasWallets) sortedWalletAssets.push(ta)
    else inactiveAssets.push(ta)
  }
  sortedWalletAssets.sort((a, b) => {
    const aVal = a.total * (a.xcRate ?? 0)
    const bVal = b.total * (b.xcRate ?? 0)
    return bVal - aVal
  })

  // Distinct settlement networks across inactive variants.
  const inactiveNetworks = new Set<number>()
  for (const ta of inactiveAssets) {
    for (const v of ta.variants) inactiveNetworks.add(networkID(v))
  }
  const inactiveCount = inactiveAssets.length
  const inactiveNetworkCount = inactiveNetworks.size

  return (
    <div className="portfolio-view fill-abs overflow-y-auto">
      <div className="portfolio-inner">
        <PortfolioTotals totals={totals} walletAssets={sortedWalletAssets} />

        {sortedWalletAssets.length > 0 && (
          <AssetPills
            walletAssets={sortedWalletAssets}
            selectedTicker={selectedTicker}
            onSelect={handlePillSelect}
          />
        )}

        {sortedWalletAssets.length > 0 && (
          <table className="asset-table w-100">
            <thead>
              <tr className="asset-table-header">
                <th className="asset-th-asset">{t('ASSETS')}</th>
                <th className="asset-th-num">{t('VALUE')}</th>
                <th className="asset-th-num">{t('BALANCE')}</th>
                <th className="asset-th-num">{t('PRICE')}</th>
                <th className="asset-th-num">{t('PORTFOLIO %')}</th>
                <th className="asset-row-actions" aria-hidden />
              </tr>
            </thead>
            <tbody>
              {sortedWalletAssets.map(ta => {
                const rate = ta.xcRate ?? 0
                const usdVal = ta.total * rate
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
            <div className="portfolio-section-header">{t('ACTIVATE NEW ASSETS')}</div>
            <p className="portfolio-section-copy grey fs13">
              {tvar('Caribou supports {{ assets }} other {{ assetWord }} across {{ networks }} {{ networkWord }} with more on the roadmap.', {
                assets: String(inactiveCount),
                assetWord: inactiveCount === 1 ? t('asset') : t('assets'),
                networks: String(inactiveNetworkCount),
                networkWord: inactiveNetworkCount === 1 ? t('network') : t('networks')
              })}
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
