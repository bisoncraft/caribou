import { PortfolioTotals as PortfolioTotalsData } from '../js/registry'
import { TickerAsset } from '../js/assets'
import Doc from '../js/doc'

const usdFmt = new Intl.NumberFormat(navigator.languages as string[], {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

interface PortfolioTotalsProps {
  totals: PortfolioTotalsData
  walletAssets: TickerAsset[]
}

export default function PortfolioTotals ({ totals, walletAssets }: PortfolioTotalsProps) {
  const { total, available, locked, numWallets } = totals

  if (numWallets === 0) {
    return (
      <div className="portfolio-totals portfolio-totals-empty">
        <div className="portfolio-totals-heading grey">No assets yet</div>
        <div className="portfolio-totals-subtitle grey fs13">Activate an asset to get started.</div>
      </div>
    )
  }

  const lockedPct = (available + locked) > 0
    ? Math.round((locked / (available + locked)) * 100)
    : 0

  return (
    <div className="portfolio-totals">
      <div className="portfolio-totals-logos d-flex justify-content-center align-items-center">
        {walletAssets.map(ta => (
          <img
            key={ta.ticker}
            src={Doc.logoPath(ta.ticker)}
            alt={ta.ticker}
            title={ta.name}
            className="portfolio-totals-logo"
          />
        ))}
      </div>
      <div className="portfolio-totals-heading">{usdFmt.format(total)}</div>
      <div className="portfolio-totals-stats d-flex justify-content-center align-items-center">
        <span className="portfolio-stat">{numWallets} Assets</span>
        <span className="portfolio-stat-sep" />
        <span className="portfolio-stat">{usdFmt.format(available)} Available</span>
        <span className="portfolio-stat-sep" />
        <span className="portfolio-stat">{lockedPct}% Locked</span>
      </div>
    </div>
  )
}
