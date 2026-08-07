import t, { tvar } from '@caribou/shared/intl'
import Doc from '@caribou/shared/doc'
import { PortfolioTotals as PortfolioTotalsData } from '@caribou/shared/registry'

interface PortfolioTotalsProps {
  totals: PortfolioTotalsData
}

export default function PortfolioTotals ({ totals }: PortfolioTotalsProps) {
  const { total, available, locked, numWallets } = totals

  if (numWallets === 0) {
    return (
      <div className="portfolio-totals text-center pb-4">
        <div className="fs24 grey">{t('No assets yet')}</div>
        <div className="grey fs13 mt-2">{t('Activate an asset to get started.')}</div>
      </div>
    )
  }

  const lockedPct = (available + locked) > 0
    ? Math.round((locked / (available + locked)) * 100)
    : 0

  return (
    <div className="portfolio-totals text-center pb-4">
      <div className="portfolio-totals-heading demi">{Doc.formatUSD(total)}</div>
      <div className="portfolio-totals-stats d-flex justify-content-center align-items-center">
        <span className="portfolio-stat fs14 grey">{tvar('{{ n }} Assets', { n: String(numWallets) })}</span>
        <span className="portfolio-stat-sep mx-1" />
        <span className="portfolio-stat fs14 grey">{tvar('{{ amt }} Available', { amt: Doc.formatUSD(available) })}</span>
        <span className="portfolio-stat-sep mx-1" />
        <span className="portfolio-stat fs14 grey">{tvar('{{ pct }}% Locked', { pct: String(lockedPct) })}</span>
      </div>
    </div>
  )
}
