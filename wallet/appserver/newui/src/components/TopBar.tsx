import { useEffect, useState } from 'react'
import app from '../js/application'
import Doc from '../js/doc'
import { PortfolioTotals } from '../js/registry'

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

  return (
    <div className="topbar d-flex align-items-stretch">
      {/* Left: logo */}
      <div className="topbar-left d-flex align-items-center">
        <img className="logo-full medium" alt="Boss Wallet" />
      </div>

      {/* Right: total value + action buttons */}
      <div className="topbar-right d-flex align-items-center justify-content-end flex-grow-1">
        {totals.total > 0 && (
          <div className="topbar-total d-flex flex-column align-items-end me-4">
            <span className="topbar-total-value">${Doc.formatFourSigFigs(totals.total, 2)} USD</span>
            <span className="topbar-total-label">TOTAL VALUE</span>
          </div>
        )}

        <button className="topbar-action-btn flex-center flex-column noborder"
          onClick={() => app.setForm({ form: 'receive', data: {} })}
          title="Receive">
          <span className="ico-qrcode fs24" />
          <span className="topbar-action-label">RECEIVE</span>
        </button>

        <button className="topbar-action-btn flex-center flex-column noborder"
          onClick={() => app.setForm({ form: 'send', data: {} })}
          title="Send">
          <span className="ico-send fs24" />
          <span className="topbar-action-label">SEND</span>
        </button>

        <button className="topbar-action-btn flex-center flex-column noborder"
          onClick={() => app.setForm({ form: 'swap', data: {} })}
          title="Swap">
          <span className="ico-swap fs24" />
          <span className="topbar-action-label">SWAP</span>
        </button>

        <button className="topbar-action-btn flex-center flex-column noborder"
          onClick={() => app.loadPage('settings')}
          title="Settings">
          <span className="ico-gear fs24" />
          <span className="topbar-action-label">SETTINGS</span>
        </button>
      </div>
    </div>
  )
}
