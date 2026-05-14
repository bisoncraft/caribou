import Doc from '../js/doc'
import app from '../js/application'
import { TickerAsset } from '../js/assets'

interface InactiveAssetRowProps {
  asset: TickerAsset
}

export default function InactiveAssetRow ({ asset }: InactiveAssetRowProps) {
  const logoPath = Doc.logoPath(asset.ticker)

  const handleActivate = () => {
    app.setForm({ form: 'activateasset', data: { assetID: asset.bestID } })
  }

  return (
    <tr className="asset-row inactive-asset-row" onClick={handleActivate}>
      <td className="asset-row-asset">
        <div className="d-flex align-items-center">
          <img src={logoPath} alt={asset.ticker} className="asset-logo inactive-asset-logo" />
          <div className="asset-name-wrap ms-2">
            <div className="asset-name">
              <span>{asset.name}</span>
            </div>
            <div className="asset-ticker grey fs12">{asset.ticker}</div>
          </div>
        </div>
      </td>
      <td className="asset-row-add-wallet" colSpan={3}>
        + Activate
      </td>
      <td className="asset-row-num" />
    </tr>
  )
}
