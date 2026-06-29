import t, { tvar } from '@bossw/shared/intl'
import { PortfolioTotals as PortfolioTotalsData } from '@bossw/shared/registry'
import { TickerAsset } from '@bossw/shared/assets'
import CoinLogo from '@bossw/shared/components/CoinLogo'

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
        <div className="portfolio-totals-heading grey">{t('No assets yet')}</div>
        <div className="portfolio-totals-subtitle grey fs13">{t('Activate an asset to get started.')}</div>
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
          // Wrap so the .has-tip ::after tooltip renders (pseudo-elements
          // don't render on the replaced <img> itself).
          <span key={ta.ticker} className="has-tip" aria-label={ta.name}>
            <CoinLogo
              ticker={ta.ticker}
              alt={ta.name}
              className="portfolio-totals-logo"
            />
          </span>
        ))}
      </div>
      <div className="portfolio-totals-heading">{usdFmt.format(total)}</div>
      <div className="portfolio-totals-stats d-flex justify-content-center align-items-center">
        <span className="portfolio-stat">{tvar('{{ n }} Assets', { n: String(numWallets) })}</span>
        <span className="portfolio-stat-sep" />
        <span className="portfolio-stat">{tvar('{{ amt }} Available', { amt: usdFmt.format(available) })}</span>
        <span className="portfolio-stat-sep" />
        <span className="portfolio-stat">{tvar('{{ pct }}% Locked', { pct: String(lockedPct) })}</span>
      </div>
    </div>
  )
}
