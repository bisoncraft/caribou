import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { TickerAsset } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'

interface InactiveAssetRowProps {
  asset: TickerAsset
}

export default function InactiveAssetRow ({ asset }: InactiveAssetRowProps) {
  const handleActivate = () => {
    app.setForm({ form: 'activateasset', data: { ticker: asset.ticker } })
  }

  return (
    <tr className="asset-row inactive-asset-row" onClick={handleActivate}>
      <td className="asset-row-asset">
        <div className="d-flex align-items-center">
          <CoinLogo ticker={asset.ticker} className="asset-logo inactive-asset-logo" />
          <div className="asset-name-wrap ms-2">
            <div className="asset-name">
              <span>{asset.name}</span>
            </div>
            <div className="asset-ticker grey fs12">{asset.ticker}</div>
          </div>
        </div>
      </td>
      <td className="asset-row-add-wallet" colSpan={5}>
        + {t('Activate')}
      </td>
    </tr>
  )
}
