import { useEffect, useState } from 'react'
import t from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import Doc from '@bossw/shared/doc'
import { PortfolioTotals } from '@bossw/shared/registry'

export default function TopBar () {
  const [totals, setTotals] = useState<PortfolioTotals>(app.portfolioTotals())

  useEffect(() => {
    const balK = app.registerBalanceUpdater(() => setTotals(app.portfolioTotals()))
    const rateK = app.registerFiatRateUpdater(() => setTotals(app.portfolioTotals()))
    return () => {
      app.unregisterBalanceUpdater(balK)
      app.unregisterFiatRateUpdater(rateK)
    }
  }, [])

  const totalDisplay = totals.total > 0 ? `$${Doc.formatFourSigFigs(totals.total, 2)} USD` : '—'

  return (
    <div className="topbar d-flex align-items-stretch">
      <div className="topbar-left d-flex align-items-center">
        <img className="logo-full medium" alt="Boss Wallet" />
      </div>

      <div className="topbar-right d-flex align-items-center justify-content-end flex-grow-1">
        {totals.numWallets > 0 && (
          <div className="topbar-total d-flex flex-column align-items-end me-4">
            <span className="topbar-total-value">{totalDisplay}</span>
            <span className="topbar-total-label">{t('TOTAL VALUE')}</span>
          </div>
        )}

        <button type="button" className="topbar-action-btn flex-center flex-column noborder"
          onClick={() => app.setForm({ form: 'receive', data: {} })}>
          <span className="ico-qrcode fs24" />
          <span className="topbar-action-label">{t('RECEIVE')}</span>
        </button>

        <button type="button" className="topbar-action-btn flex-center flex-column noborder"
          onClick={() => app.setForm({ form: 'send', data: {} })}>
          <span className="ico-send fs24" />
          <span className="topbar-action-label">{t('SEND')}</span>
        </button>

        <button type="button" className="topbar-action-btn flex-center flex-column noborder"
          onClick={() => app.setForm({ form: 'swap', data: {} })}>
          <span className="ico-swap fs24" />
          <span className="topbar-action-label">{t('SWAP')}</span>
        </button>

        <button type="button" className="topbar-action-btn flex-center flex-column noborder"
          onClick={() => app.loadPage('settings')}>
          <span className="ico-gear fs24" />
          <span className="topbar-action-label">{t('SETTINGS')}</span>
        </button>
      </div>
    </div>
  )
}
