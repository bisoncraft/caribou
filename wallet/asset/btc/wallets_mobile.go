//go:build mobile

package btc

import "github.com/bisoncraft/caribou/wallet/asset"

// availableWallets is the mobile wallet menu: electrum only, so it is both the
// default and the sole choice. Neutrino is not viable on a phone. It syncs
// block and filter headers from genesis (the checkpoint authenticates the
// birthday anchor, it does not let the header walk skip ahead), and it holds
// a 31.2 MB filter cache plus a 41 MB block cache per service by default.
// Electrum instead anchors its header store at the most recent checkpoint, so
// a fresh wallet stores almost nothing, keeps no filter or block cache, and
// opens no p2p connections.
//
// The tradeoff is real and deliberate: an electrum server learns the wallet's
// address history, where neutrino's filter matching does not. It is taken here
// because the resource cost is prohibitive on a phone and because mainnet
// defines no NeutrinoSeeds, leaving mainnet SPV to bootstrap from generic DNS
// seeds that need not serve BIP-157 at all.
//
// Both types drive the same NativeWallet over the same on-disk btcwallet
// database, so this is a menu change, not a storage change, and the
// walletTypeSPV branches in Exists, Create and NewWallet stay compiled.
//
// Withholding the definition does reach past the menu in one place, and it is
// not a soft failure: asset.WalletDef resolves a wallet type by scanning
// AvailableWallets, and core's loadWallet calls it unconditionally and returns
// its error, so a walletTypeSPV wallet on mobile could not be loaded at all
// — not to use, not to sweep, not to migrate. That is safe only because no
// mobile build has ever been released, so no phone holds such a wallet. It is
// a claim about shipping history, not about the code, so anyone reintroducing
// SPV to the mobile menu, or shipping a build that predates this file, owes a
// one-time migration rewriting dbWallet.Type from SPV to electrum. The
// migration is cheap precisely because of the paragraph above: same wallet,
// same database, only the type string changes.
var availableWallets = []*asset.WalletDefinition{
	electrumWalletDefinition,
}
