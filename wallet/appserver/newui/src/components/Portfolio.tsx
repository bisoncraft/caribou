import { useEffect } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { useForceUpdate } from '@caribou/shared/hooks'
import { CoreNote } from '@caribou/shared/registry'
import { TickerAsset, compareByFiatValue } from '@caribou/shared/assets'
import PortfolioTotals from './PortfolioTotals'
import AssetRow from './AssetRow'
import InactiveAssetRow from './InactiveAssetRow'

export default function Portfolio () {
  const redraw = useForceUpdate()

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

  const totals = app.portfolioTotals()
  const tickerList: TickerAsset[] = app.tickerList ?? []

  const sortedWalletAssets: TickerAsset[] = []
  const inactiveAssets: TickerAsset[] = []
  for (const ta of tickerList) {
    if (ta.hasWallets) sortedWalletAssets.push(ta)
    else inactiveAssets.push(ta)
  }
  sortedWalletAssets.sort(compareByFiatValue)

  return (
    <div className="portfolio-view fill-abs overflow-y-auto">
      <div className="portfolio-inner">
        <PortfolioTotals totals={totals} />
      </div>

      <div className="portfolio-tables">
        {tickerList.length > 0 && (
          <table className="asset-table w-100 border-top">
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
            {sortedWalletAssets.length > 0 && (
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
                    />
                  )
                })}
              </tbody>
            )}
            {/* Inactive assets trail the funded ones in their own tbody,
                so they stay grouped at the bottom of the one table. */}
            {inactiveAssets.length > 0 && (
              <tbody>
                {inactiveAssets.map(ta => (
                  <InactiveAssetRow key={ta.ticker} asset={ta} />
                ))}
              </tbody>
            )}
          </table>
        )}
      </div>
    </div>
  )
}
