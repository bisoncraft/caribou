//go:build mobile

package electrum

// maxObservers bounds the observer connections per network. One on mobile, where
// electrum is the only wallet type and so this runs for every UTXO asset: at the
// desktop count that is four sockets per asset, each with its own keep-alive
// ping, which on a metered radio is the dominant cost of the electrum backend.
//
// Deliberately one and not zero. The observers are not redundancy for its own
// sake, they are the check that catches a wallet server which is lagging or
// lying (see the divergence and lag probes in observers.go), and mobile is
// exactly where that check matters most, because electrum-only means every
// mobile wallet trusts a server with its address history. One observer still
// detects a wrong chain or a stalled server, just with a single opinion rather
// than a choice of three.
const maxObservers = 1
