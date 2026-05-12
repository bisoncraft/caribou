import Doc from '../js/doc'
import app from '../js/application'
import { TickerAsset } from '../js/assets'

interface InactiveAssetRowProps {
  asset: TickerAsset
}

export default function InactiveAssetRow ({ asset }: InactiveAssetRowProps) {
  const logoPath = Doc.logoPath(asset.logoSymbol)

  const handleAddWallet = () => {
    // TODO: open the wallet-creation form once it exists in Modals.
    // For now, find the best assetID and pass it along as a placeholder.
    app.setForm({ form: 'createwallet', data: { assetID: asset.bestID } })
  }

  return (
    <tr className="asset-row inactive-asset-row" onClick={handleAddWallet}>
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
        + Activate wallet
      </td>
      <td className="asset-row-num" />
    </tr>
  )
}
