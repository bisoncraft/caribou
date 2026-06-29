import { TickerAsset } from '@bossw/shared/assets'

interface AssetPillsProps {
  walletAssets: TickerAsset[]
  selectedTicker: string | null
  onSelect: (ticker: string) => void
}

export default function AssetPills ({ walletAssets, selectedTicker, onSelect }: AssetPillsProps) {
  if (walletAssets.length === 0) return null

  return (
    <div className="asset-pills d-flex align-items-center">
      {walletAssets.map((ta) => (
        <button
          key={ta.ticker}
          className={`asset-pill ${selectedTicker === ta.ticker ? 'asset-pill-selected' : ''}`}
          onClick={() => onSelect(ta.ticker)}
        >
          {ta.ticker}
        </button>
      ))}
    </div>
  )
}
