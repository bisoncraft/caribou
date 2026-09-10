import { tvar } from '@caribou/shared/intl'
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

  if (!wallet.synced) {
    const pct = Math.round((wallet.syncProgress ?? 0) * 100)
    return (
      <span
        className="asset-status-syncing-spinner d-inline-flex align-items-center has-tip gold"
        aria-label={tvar('syncing_pct', { pct: String(pct) })}
      >
        <Spinner size={10} />
      </span>
    )
  }

  return null
}

export default function AssetRow ({ asset, fiatRate, portfolioPct }: AssetRowProps) {
  // BALANCE shows the total the VALUE column prices, so the two agree; the
  // part that is not spendable (a network reserve, an immature coinbase) is
  // broken out beneath it rather than silently subtracted.
  const usdValue = asset.total * fiatRate
  const totalFmt = Doc.formatConventionalValue(asset.total, asset.precision)
  const lockedFmt = asset.locked > 0 ? Doc.formatConventionalValue(asset.locked, asset.precision) : ''
  const immatureFmt = asset.immature > 0 ? Doc.formatConventionalValue(asset.immature, asset.precision) : ''
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
        <span className="mono">{totalFmt}</span>
        {lockedFmt && (
          <div className="grey fs11 mono">{tvar('{{ amount }} locked', { amount: lockedFmt })}</div>
        )}
        {immatureFmt && (
          <div className="grey fs11 mono">{tvar('{{ amount }} immature', { amount: immatureFmt })}</div>
        )}
      </td>
      <td className="asset-row-num">
        {fiatRate > 0 ? Doc.formatUSD(fiatRate) : '—'}
      </td>
      <td className="asset-row-num">
        {pctDisplay}
      </td>
      <td className="asset-row-actions text-right">
        <button
          type="button"
          className="icon-btn d-inline-flex align-items-center justify-content-center p-0 border-0 has-tip ico-download fs16"
          aria-label={tvar('Receive {{ ticker }}', { ticker: asset.ticker })}
          onClick={(e) => { e.stopPropagation(); app.setForm({ form: 'receive', data: { ticker: asset.ticker } }) }}
        />
        <button
          type="button"
          className="icon-btn d-inline-flex align-items-center justify-content-center p-0 border-0 has-tip ico-upload fs16 ms-1"
          aria-label={tvar('Send {{ ticker }}', { ticker: asset.ticker })}
          onClick={(e) => { e.stopPropagation(); app.setForm({ form: 'send', data: { ticker: asset.ticker } }) }}
        />
      </td>
    </tr>
  )
}
