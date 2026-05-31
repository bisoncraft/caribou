import t from '../js/intl'
import app from '../js/application'
import { TickerAsset } from '../js/assets'
import CoinLogo from './CoinLogo'

const languages = navigator.languages as string[]

const usdFmt = new Intl.NumberFormat(languages, {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

const balFmt = new Intl.NumberFormat(languages, {
  minimumFractionDigits: 0,
  maximumFractionDigits: 8
})

interface AssetRowProps {
  asset: TickerAsset
  fiatRate: number
  portfolioPct: number | null
  isHighlighted: boolean
}

function StatusDot ({ asset }: { asset: TickerAsset }) {
  // Use the primary (non-token) variant for wallet status.
  const primary = asset.variants.find(v => !v.token) ?? asset.variants[0]
  if (!primary) return null
  const wallet = app.user?.assets[primary.assetID]?.wallet
  if (!wallet) return null

  // Happy path: synced and open — no dot.
  if (wallet.synced && wallet.open) return null

  if (wallet.encrypted && !wallet.open) {
    return <span className="ico-lock-closed asset-status-dot asset-status-locked fs11" title={t('Locked')} />
  }

  if (!wallet.synced) {
    const pct = Math.round((wallet.syncProgress ?? 0) * 100)
    return (
      <span
        className="asset-status-dot asset-status-syncing"
        title={`${t('Syncing')}: ${pct}%`}
      />
    )
  }

  return null
}

export default function AssetRow ({ asset, fiatRate, portfolioPct, isHighlighted }: AssetRowProps) {
  const totalConv = asset.total / asset.cFactor
  const usdValue = totalConv * fiatRate
  const availFmt = balFmt.format(asset.avail / asset.cFactor)
  const pctDisplay = portfolioPct !== null ? `${portfolioPct.toFixed(1)}%` : '—'

  return (
    <tr className={`asset-row ${isHighlighted ? 'asset-row-highlight' : ''}`}>
      <td className="asset-row-asset">
        <div className="d-flex align-items-center">
          <CoinLogo ticker={asset.ticker} className="asset-logo" />
          <div className="asset-name-wrap ms-2">
            <div className="asset-name d-flex align-items-center">
              <span>{asset.name}</span>
              <StatusDot asset={asset} />
            </div>
            <div className="asset-ticker grey fs12">{asset.ticker}</div>
          </div>
        </div>
      </td>
      <td className="asset-row-num">
        {fiatRate > 0 ? usdFmt.format(usdValue) : '—'}
      </td>
      <td className="asset-row-num">
        <span className="mono">{availFmt}</span>
      </td>
      <td className="asset-row-num">
        {fiatRate > 0 ? usdFmt.format(fiatRate) : '—'}
      </td>
      <td className="asset-row-num">
        {pctDisplay}
      </td>
    </tr>
  )
}
