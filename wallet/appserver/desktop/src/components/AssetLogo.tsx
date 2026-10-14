import app from '@caribou/shared/application'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import NetworkedTokenLogo from '@caribou/shared/components/NetworkedTokenLogo'

interface AssetLogoProps {
  // assetID resolves the asset's token/network relationship; an ID unknown
  // to this build renders as a plain logo for ticker.
  assetID: number
  // ticker is the display ticker of the asset itself.
  ticker: string
  // className sizes the logo (an icon-N utility) and carries any layout
  // classes (e.g. margins), on the composite's wrapper or the plain img.
  className?: string
}

// AssetLogo renders an asset's logo by wallet asset ID: a token gets the
// networked-token composite with its settlement network's logo as a
// superscript badge, while a chain native (or an unknown asset) gets the
// plain coin logo.
export default function AssetLogo ({ assetID, ticker, className }: AssetLogoProps) {
  const sa = app.user?.assets[assetID]
  const network = sa?.token ? app.user?.assets[sa.token.networkID]?.unitInfo.conventional.unit : undefined
  if (!network) return <CoinLogo ticker={ticker} className={className} />
  return <NetworkedTokenLogo token={ticker} network={network} className={className} />
}
