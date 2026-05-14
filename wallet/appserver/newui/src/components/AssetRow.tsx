import { useEffect, useRef } from 'react'
import Doc from '../js/doc'
import app from '../js/application'
import { TickerAsset } from '../js/assets'

const usdFmt = new Intl.NumberFormat(navigator.languages as string[], {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

interface AssetRowProps {
  asset: TickerAsset
  fiatRate: number
  portfolioPct: number | null
  isHighlighted: boolean
}

function StatusDot ({ asset }: { asset: TickerAsset }) {
  // Use the primary (non-token) variant for wallet status
  const primary = asset.variants.find(v => !v.token) ?? asset.variants[0]
  if (!primary) return null
  const wallet = app.user?.assets[primary.assetID]?.wallet
  if (!wallet) return null

  // Happy path: synced and open — no dot
  if (wallet.synced && wallet.open) return null

  // Locked wallet (encrypted and not open)
  if (wallet.encrypted && !wallet.open) {
    return <span className="ico-lock-closed asset-status-dot asset-status-locked fs11" title="Locked" />
  }

  // Syncing
  if (!wallet.synced) {
    const pct = Math.round((wallet.syncProgress ?? 0) * 100)
    return (
      <span
        className="asset-status-dot asset-status-syncing"
        title={`Syncing: ${pct}%`}
      />
    )
  }

  return null
}

export default function AssetRow ({ asset, fiatRate, portfolioPct, isHighlighted }: AssetRowProps) {
  const rowRef = useRef<HTMLTableRowElement>(null)

  useEffect(() => {
    if (!isHighlighted || !rowRef.current) return
    const el = rowRef.current
    el.classList.add('asset-row-highlight')
    const timer = setTimeout(() => el.classList.remove('asset-row-highlight'), 800)
    return () => clearTimeout(timer)
  }, [isHighlighted])

  const totalAtoms = asset.total
  const totalConv = totalAtoms / asset.cFactor
  const usdValue = totalConv * fiatRate

  const availConv = asset.avail / asset.cFactor
  const availFmt = new Intl.NumberFormat(navigator.languages as string[], {
    minimumFractionDigits: 0,
    maximumFractionDigits: 8
  }).format(availConv)

  const pctDisplay = portfolioPct !== null
    ? `${portfolioPct.toFixed(1)}%`
    : '—'

  const logoPath = Doc.logoPath(asset.logoSymbol)

  return (
    <tr ref={rowRef} className="asset-row">
      <td className="asset-row-asset">
        <div className="d-flex align-items-center">
          <img src={logoPath} alt={asset.ticker} className="asset-logo" />
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
        <span className="mono">{availFmt}</span>
      </td>
      <td className="asset-row-num">
        {fiatRate > 0 ? usdFmt.format(usdValue) : '—'}
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
