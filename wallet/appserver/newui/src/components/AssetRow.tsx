import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { TickerAsset } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import Spinner from './Spinner'

interface AssetRowProps {
  asset: TickerAsset
  fiatRate: number
  portfolioPct: number | null
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
    return <span className="ico-lock-closed asset-status-dot asset-status-locked has-tip fs11" aria-label={t('Locked')} />
  }

  if (!wallet.synced) {
    const pct = Math.round((wallet.syncProgress ?? 0) * 100)
    return (
      <span
        className="asset-status-syncing-spinner has-tip gold"
        aria-label={tvar('syncing_pct', { pct: String(pct) })}
      >
        <Spinner size={10} />
      </span>
    )
  }

  return null
}

export default function AssetRow ({ asset, fiatRate, portfolioPct }: AssetRowProps) {
  const usdValue = asset.total * fiatRate
  const availFmt = Doc.formatConventionalValue(asset.avail, asset.precision)
  const pctDisplay = portfolioPct !== null ? `${portfolioPct.toFixed(1)}%` : '—'

  return (
    <tr
      className="asset-row pointer hoverbg"
      onClick={() => app.loadPage(`asset/${asset.ticker}`)}
    >
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
        {fiatRate > 0 ? Doc.formatUSD(usdValue) : '—'}
      </td>
      <td className="asset-row-num">
        <span className="mono">{availFmt}</span>
      </td>
      <td className="asset-row-num">
        {fiatRate > 0 ? Doc.formatUSD(fiatRate) : '—'}
      </td>
      <td className="asset-row-num">
        {pctDisplay}
      </td>
      <td className="asset-row-actions">
        <button
          type="button"
          className="asset-action-icon has-tip ico-download fs16"
          aria-label={tvar('Receive {{ ticker }}', { ticker: asset.ticker })}
          onClick={(e) => { e.stopPropagation(); app.setForm({ form: 'receive', data: { ticker: asset.ticker } }) }}
        />
        <button
          type="button"
          className="asset-action-icon has-tip ico-upload fs16"
          aria-label={tvar('Send {{ ticker }}', { ticker: asset.ticker })}
          onClick={(e) => { e.stopPropagation(); app.setForm({ form: 'send', data: { ticker: asset.ticker } }) }}
        />
      </td>
    </tr>
  )
}
