import CoinLogo from './CoinLogo'

interface NetworkedTokenLogoProps {
  // token is the ticker of the main (foreground) logo.
  token: string
  // network is the ticker of the superscript badge logo.
  network: string
  // className sizes the token logo (an icon-N utility) and carries any layout
  // classes (e.g. margins). The badge is sized and offset as proportions of the
  // token logo, so the whole composite scales with the icon-N size.
  className?: string
}

// NetworkedTokenLogo renders a token logo with the network's logo as a small
// superscript badge in the top-right corner. Badge size, offset, and ring are
// proportions of the token logo (see .networked-token-badge), so changing the
// token logo's size scales the badge and its placement with it. The ring lives
// on the badge wrapper, never on a logo img, so the coin artwork is not clipped.
export default function NetworkedTokenLogo ({ token, network, className }: NetworkedTokenLogoProps) {
  return (
    <span className={`position-relative d-inline-flex ${className ?? ''}`}>
      <CoinLogo ticker={token} className="w-100 h-100" />
      <span className="networked-token-badge d-inline-flex">
        <CoinLogo ticker={network} className="w-100 h-100" />
      </span>
    </span>
  )
}
