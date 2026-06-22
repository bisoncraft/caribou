// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/chain"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/wire"
)

// neutrinoRecoveryWindow is the address gap limit btcwallet recovers across when
// opening a neutrino-backed wallet. Neutrino is filter-scan, so this recovery is
// what drives the compact-filter scan that discovers the wallet's funds. Matches
// the window the previous SPV wallet used.
const neutrinoRecoveryWindow = 250

// neutrinoBackend is a chainBackend driven by a built-in neutrino (BIP-157/158)
// chain service — the SPV counterpart to the electrum backend. The lean wallet
// drives it through the same chainBackend surface; only the construction here
// differs from electrum. It embeds *chain.NeutrinoClient (which provides the
// chain.Backend + SyncHeight/Connected surface) and overrides Start/Stop to
// also manage the chain service, peer connections, and the neutrino db.
type neutrinoBackend struct {
	*chain.NeutrinoClient
	chainService *spv.ChainService
	neutrinoDB   walletdb.DB
	peerManager  *SPVPeerManager
}

// Start starts the neutrino client (which lazily starts the chain service) and
// connects to the initial wallet peers.
func (b *neutrinoBackend) Start() error {
	if err := b.NeutrinoClient.Start(); err != nil {
		return err
	}
	b.peerManager.ConnectToInitialWalletPeers()
	return nil
}

// Stop stops the neutrino client and chain service and closes the neutrino db.
// It is best-effort; errors closing down are not actionable.
func (b *neutrinoBackend) Stop() {
	b.NeutrinoClient.Stop()
	_ = b.chainService.Stop()
	_ = b.neutrinoDB.Close()
}

// Peers, AddPeer, and RemovePeer expose the neutrino peer manager so the wallet
// can satisfy asset.PeerManager when driven by this backend.
func (b *neutrinoBackend) Peers() ([]*asset.WalletPeer, error) { return b.peerManager.Peers() }
func (b *neutrinoBackend) AddPeer(addr string) error           { return b.peerManager.AddPeer(addr) }
func (b *neutrinoBackend) RemovePeer(addr string) error        { return b.peerManager.RemovePeer(addr) }

var _ chainBackend = (*neutrinoBackend)(nil)
var _ peerManagedBackend = (*neutrinoBackend)(nil)

// neutrinoBackendFactory returns a ChainConfig.NewBackend that stands up a
// neutrino chain service + NeutrinoClient for cc's chain. Assigning it to
// ChainConfig.NewBackend turns the shared lean wallet into an SPV wallet —
// identical to the electrum wallet except for the backend it builds.
func neutrinoBackendFactory(cc ChainConfig) func(dir string, log util.Logger) (chainBackend, error) {
	return func(dir string, log util.Logger) (chainBackend, error) {
		neutrinoDBPath := filepath.Join(dir, neutrinoDBName)
		db, err := walletdb.Create("bdb", neutrinoDBPath, true, dbTimeout)
		if err != nil {
			return nil, fmt.Errorf("neutrino: create db at %q: %w", neutrinoDBPath, err)
		}

		cs, err := spv.NewChainService(spv.Config{
			DataDir:       dir,
			Database:      db,
			Chain:         cc.Chain,
			ChainParams:   cc.NetParams,
			PersistToDisk: true, // keep cfilter headers on disk for efficient rescanning
			// PublishTransaction round trips can take a few seconds; keep
			// neutrino from cancelling queries too readily.
			BroadcastTimeout: 6 * time.Second,
		})
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("neutrino: create chain service: %w", err)
		}

		var defaultPeers []string
		switch cc.ChainParams.Net {
		case wire.TestNet3:
			defaultPeers = []string{"dex-test.ssgen.io:18333"}
		case wire.TestNet, wire.SimNet: // plain wire.TestNet is regnet
			defaultPeers = []string{"127.0.0.1:20575"}
		}
		pm := NewSPVPeerManager(&btcChainService{cs}, defaultPeers, dir, log, cc.ChainParams.DefaultPort)

		return &neutrinoBackend{
			NeutrinoClient: chain.NewNeutrinoClient(cc.NetParams, cs),
			chainService:   cs,
			neutrinoDB:     db,
			peerManager:    pm,
		}, nil
	}
}
