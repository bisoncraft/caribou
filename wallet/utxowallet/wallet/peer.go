// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package wallet

// PeerSource specifies how a wallet knows about a peer — built-in default,
// manually added by the user, or discovered from other peers.
type PeerSource uint16

const (
	// PeerDefault is a peer hard-coded into the wallet as a network bootstrap point.
	PeerDefault PeerSource = iota
	// PeerUserAdded is a peer explicitly added by the user via AddPeer.
	PeerUserAdded
	// PeerDiscovered is a peer discovered automatically by the p2p layer.
	PeerDiscovered
)

// Peer carries information about a connected or tracked network peer. It is the
// neutral analogue of asset.WalletPeer; callers that need asset.WalletPeer map
// the fields at their own boundary.
type Peer struct {
	Addr      string
	Source    PeerSource
	Connected bool
}
