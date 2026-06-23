// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/chain"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/spv"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/chaincfg"
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
	// connectPeersOnce ensures the initial wallet peers are connected only on the
	// first Start. A rescan restarts the client (Stop is client-only, so the
	// chain service and its peers stay up), and re-adding an already-connected
	// peer logs a spurious error.
	connectPeersOnce sync.Once
}

// Start starts the neutrino client (which lazily starts the chain service) and,
// on the first start, connects to the initial wallet peers.
func (b *neutrinoBackend) Start() error {
	if err := b.NeutrinoClient.Start(); err != nil {
		return err
	}
	b.connectPeersOnce.Do(b.peerManager.ConnectToInitialWalletPeers)
	return nil
}

// Stop stops only the neutrino client, leaving the chain service (and its synced
// headers) running. This mirrors the standalone NeutrinoClient.Stop and lets the
// wallet restart the client — e.g. across a rescan, where btcwallet.Stop stops
// the client — without re-downloading the chain. Full teardown is Shutdown.
func (b *neutrinoBackend) Stop() {
	b.NeutrinoClient.Stop()
}

// Shutdown stops the client, stops the chain service, and closes the neutrino
// db. It is best-effort; errors closing down are not actionable.
func (b *neutrinoBackend) Shutdown() {
	b.NeutrinoClient.Stop()
	_ = b.chainService.Stop()
	_ = b.neutrinoDB.Close()
}

// CanRescan reports true: a neutrino rescan stops only the client (Stop) and
// restarts it, leaving the synced chain service running.
func (b *neutrinoBackend) CanRescan() bool { return true }

// Peers, AddPeer, and RemovePeer expose the neutrino peer manager so the wallet
// can satisfy asset.PeerManager when driven by this backend.
func (b *neutrinoBackend) Peers() ([]*asset.WalletPeer, error) { return b.peerManager.Peers() }
func (b *neutrinoBackend) AddPeer(addr string) error           { return b.peerManager.AddPeer(addr) }
func (b *neutrinoBackend) RemovePeer(addr string) error        { return b.peerManager.RemovePeer(addr) }

var _ chainBackend = (*neutrinoBackend)(nil)
var _ peerManagedBackend = (*neutrinoBackend)(nil)

// neutrinoBackendFactory returns a ChainConfig.NewBackend that stands up a
// neutrino chain service + NeutrinoClient for cc's chain. seeds are the
// network's default peers and newSPVService wraps the chain service into the
// peer manager's SPVService — both chain-specific (btc and ltc serve some
// neutrino queries differently), supplied by the chain's SPVChainConfig call.
func neutrinoBackendFactory(cc ChainConfig, seeds []string, newSPVService func(*spv.ChainService) PeerManagerChainService) func(dir string, log util.Logger) (chainBackend, error) {
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

		pm := NewSPVPeerManager(newSPVService(cs), seeds, dir, log, cc.ChainParams.DefaultPort)

		return &neutrinoBackend{
			NeutrinoClient: chain.NewNeutrinoClient(cc.NetParams, cs),
			chainService:   cs,
			neutrinoDB:     db,
			peerManager:    pm,
		}, nil
	}
}

// SPVChainConfig returns base configured to drive the shared lean wallet with
// the neutrino (BIP-157/158) backend — i.e. as an SPV wallet rather than an
// electrum one. seeds are the network's default peers and newSPVService wraps
// the neutrino chain service for the peer manager; both are chain-specific. btc
// and ltc build their SPV wallet by passing their electrum ChainConfig here.
func SPVChainConfig(base ChainConfig, seeds []string, newSPVService func(*spv.ChainService) PeerManagerChainService) ChainConfig {
	base.NewBackend = neutrinoBackendFactory(base, seeds, newSPVService)
	base.RecoveryWindow = neutrinoRecoveryWindow
	return base
}

// btcSPVChainConfig returns the ChainConfig for a BTC SPV wallet.
func btcSPVChainConfig(chainParams *chaincfg.Params) ChainConfig {
	return SPVChainConfig(btcChainConfig(chainParams), btcNeutrinoSeeds(chainParams.Net),
		func(cs *spv.ChainService) PeerManagerChainService { return &btcChainService{cs} })
}

// btcNeutrinoSeeds returns the default neutrino seed peers for a BTC network.
func btcNeutrinoSeeds(net wire.BitcoinNet) []string {
	switch net {
	case wire.TestNet3:
		return []string{"dex-test.ssgen.io:18333"}
	case wire.TestNet, wire.SimNet: // plain wire.TestNet is regnet
		return []string{"127.0.0.1:20575"}
	}
	return nil
}
