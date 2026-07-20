// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package wallet

// backends.go constructs the concrete chain backends that *Wallet owns. Backends
// are internal to this package; callers choose a backend via BackendKind.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/chain"
	"github.com/bisoncraft/caribou/wallet/utxowallet/electrum"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/bisoncraft/caribou/wallet/utxowallet/spv"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
)

// BackendKind selects the chain backend the wallet constructs and owns.
type BackendKind int

const (
	// BackendElectrum uses an electrumx server connection.
	BackendElectrum BackendKind = iota
	// BackendNeutrino uses the built-in neutrino (BIP-157/158) SPV service.
	BackendNeutrino
)

// walletBackend is the full interface that a wallet-owned backend must satisfy.
// It extends chain.Backend with the extra signals the wallet needs: sync progress,
// connectivity, full teardown, and rescan capability.
type walletBackend interface {
	chain.Backend
	// SyncHeight reports the locally-synced header tip, the target height the
	// backend is syncing toward, and whether the backend considers itself current.
	SyncHeight() (localTip, targetTip int32, synced bool)
	// Connected reports whether the backend has a live server/peer connection.
	Connected() bool
	// Shutdown fully tears the backend down. chain.Backend.Stop may stop only a
	// client while keeping an underlying service running (neutrino does this so
	// the wallet can restart the client across a rescan without re-downloading
	// headers); Shutdown always releases everything.
	Shutdown()
	// CanRescan reports whether the backend supports in-place rescanning: i.e.
	// its client can be stopped and restarted while the underlying service stays
	// running. Neutrino can; electrum cannot.
	CanRescan() bool
}

// peerBackend is the optional extension of walletBackend for backends that manage
// p2p peers (neutrino). Backends that don't expose peers (electrum) don't implement
// this interface.
type peerBackend interface {
	walletPeers() ([]Peer, error)
	walletAddPeer(addr string) error
	walletRemovePeer(addr string) error
}

// backendLogger is the minimal logging interface used by the backends.
type backendLogger interface {
	Errorf(format string, params ...any)
}

// ─── Electrum backend ────────────────────────────────────────────────────────

// electrumWalletBackend wraps *chain.ElectrumClient and satisfies walletBackend.
type electrumWalletBackend struct {
	*chain.ElectrumClient
}

func (b *electrumWalletBackend) Shutdown()       { b.ElectrumClient.Shutdown() }
func (b *electrumWalletBackend) CanRescan() bool { return false }

var _ walletBackend = (*electrumWalletBackend)(nil)

// newElectrumBackend constructs an electrum backend for the given chain params.
func newElectrumBackend(np *netparams.ChainParams, dataDir string) (walletBackend, error) {
	if len(np.ElectrumServers) == 0 {
		return nil, fmt.Errorf("electrum: no built-in server for network %q", np.BTCDParams().Name)
	}
	for _, srvAddr := range np.ElectrumServers {
		if err := srvAddr.Validate(); err != nil {
			return nil, fmt.Errorf("electrum: invalid built-in server address: %w", err)
		}
	}
	bwChain, err := bisonwire.ChainFromString(np.Chain)
	if err != nil {
		return nil, fmt.Errorf("electrum: chain id %q: %w", np.Chain, err)
	}
	chainCfg, err := electrum.NewChainConfig(bwChain, np)
	if err != nil {
		return nil, fmt.Errorf("electrum: chain config: %w", err)
	}
	// The full built-in list, in preference order: Start elects one
	// connection, rotating past recently-failed candidates (electrum
	// serverpool).
	network, err := electrum.NewNetwork(&electrum.Config{
		Chain:   chainCfg,
		Servers: np.ElectrumServers,
		DataDir: dataDir,
	})
	if err != nil {
		return nil, fmt.Errorf("electrum: new network: %w", err)
	}
	client, err := chain.NewElectrumClient(np, network)
	if err != nil {
		return nil, fmt.Errorf("electrum: new client: %w", err)
	}
	return &electrumWalletBackend{ElectrumClient: client}, nil
}

// ─── Neutrino (SPV) backend ──────────────────────────────────────────────────

const neutrinoDBName = "neutrino.db"

// neutrinoWalletBackend wraps *chain.NeutrinoClient plus the chain service and
// satisfies walletBackend + peerBackend.
type neutrinoWalletBackend struct {
	*chain.NeutrinoClient
	chainService     *spv.ChainService
	neutrinoDB       walletdb.DB
	peerMgr          *walletSPVPeerManager
	connectPeersOnce sync.Once
}

// Start starts the neutrino client and, on the first call, connects to initial peers.
func (b *neutrinoWalletBackend) Start() error {
	if err := b.NeutrinoClient.Start(); err != nil {
		return err
	}
	b.connectPeersOnce.Do(b.peerMgr.connectToInitialPeers)
	return nil
}

// Stop stops only the neutrino client, leaving the chain service running. The wallet
// calls this during a rescan so it can restart without re-downloading headers.
func (b *neutrinoWalletBackend) Stop() {
	b.NeutrinoClient.Stop()
}

// Shutdown stops the client, chain service, and closes the neutrino database.
func (b *neutrinoWalletBackend) Shutdown() {
	b.NeutrinoClient.Stop()
	_ = b.chainService.Stop()
	_ = b.neutrinoDB.Close()
}

// CanRescan reports true: neutrino's Stop is client-only.
func (b *neutrinoWalletBackend) CanRescan() bool { return true }

func (b *neutrinoWalletBackend) walletPeers() ([]Peer, error) { return b.peerMgr.peers() }
func (b *neutrinoWalletBackend) walletAddPeer(addr string) error {
	return b.peerMgr.addPeer(addr, peerUserAdded, false)
}
func (b *neutrinoWalletBackend) walletRemovePeer(addr string) error {
	return b.peerMgr.removePeer(addr)
}

var _ walletBackend = (*neutrinoWalletBackend)(nil)
var _ peerBackend = (*neutrinoWalletBackend)(nil)

// newNeutrinoBackend constructs a neutrino chain-service + client backend.
func newNeutrinoBackend(np *netparams.ChainParams, dataDir string, logger backendLogger) (walletBackend, error) {
	bwChain, err := bisonwire.ChainFromString(np.Chain)
	if err != nil {
		return nil, fmt.Errorf("neutrino: chain id %q: %w", np.Chain, err)
	}
	neutrinoDBPath := filepath.Join(dataDir, neutrinoDBName)
	db, err := walletdb.Create("bdb", neutrinoDBPath, true, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("neutrino: create db at %q: %w", neutrinoDBPath, err)
	}

	cs, err := spv.NewChainService(spv.Config{
		DataDir:          dataDir,
		Database:         db,
		Chain:            bwChain,
		ChainParams:      np,
		PersistToDisk:    true,
		BroadcastTimeout: 6 * time.Second,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("neutrino: create chain service: %w", err)
	}

	bcs := &walletChainService{cs}
	pm := newWalletSPVPeerManager(bcs, np.NeutrinoSeeds, dataDir, logger, np.DefaultPort)

	return &neutrinoWalletBackend{
		NeutrinoClient: chain.NewNeutrinoClient(np, cs),
		chainService:   cs,
		neutrinoDB:     db,
		peerMgr:        pm,
	}, nil
}

// ─── walletChainService wraps *spv.ChainService ──────────────────────────────

// spvServicePeer is the interface expected from a neutrino peer for the peer
// manager's connected-peer snapshot.
type spvServicePeer interface {
	Addr() string
}

// walletChainService adapts *spv.ChainService to peerChainService.
type walletChainService struct {
	*spv.ChainService
}

func (s *walletChainService) AddPeer(addr string) error {
	return s.ChainService.ConnectNode(addr, true)
}

func (s *walletChainService) RemovePeer(addr string) error {
	return s.ChainService.RemoveNodeByAddr(addr)
}

func (s *walletChainService) Peers() []spvServicePeer {
	rawPeers := s.ChainService.Peers()
	peers := make([]spvServicePeer, 0, len(rawPeers))
	for _, p := range rawPeers {
		peers = append(peers, p)
	}
	return peers
}

// ─── walletSPVPeerManager ─────────────────────────────────────────────────────

type peerChainService interface {
	AddPeer(addr string) error
	RemovePeer(addr string) error
	Peers() []spvServicePeer
}

type walletPeerSource uint16

const (
	peerUserAdded walletPeerSource = iota
	peerDefault
)

func (s walletPeerSource) toPeerSource() PeerSource {
	switch s {
	case peerUserAdded:
		return PeerUserAdded
	case peerDefault:
		return PeerDefault
	}
	return PeerDiscovered
}

type walletTrackedPeer struct {
	source       walletPeerSource
	resolvedName string
}

// walletSPVPeerManager manages p2p peers for the neutrino chain service. It uses
// the neutral wallet.Peer type so the wallet package stays free of wallet/asset.
type walletSPVPeerManager struct {
	cs peerChainService

	peersMtx     sync.RWMutex
	trackedPeers map[string]*walletTrackedPeer

	savedPeersFilePath string
	defaultPeers       []string
	defaultPort        string
	log                backendLogger
}

func newWalletSPVPeerManager(cs peerChainService, defaultPeers []string, dir string, log backendLogger, defaultPort string) *walletSPVPeerManager {
	return &walletSPVPeerManager{
		cs:                 cs,
		defaultPeers:       defaultPeers,
		trackedPeers:       make(map[string]*walletTrackedPeer),
		savedPeersFilePath: filepath.Join(dir, "caribou-peers.json"),
		log:                log,
		defaultPort:        defaultPort,
	}
}

func (s *walletSPVPeerManager) connectedPeers() map[string]struct{} {
	rawPeers := s.cs.Peers()
	connected := make(map[string]struct{}, len(rawPeers))
	for _, p := range rawPeers {
		connected[p.Addr()] = struct{}{}
	}
	return connected
}

func (s *walletSPVPeerManager) peers() ([]Peer, error) {
	s.peersMtx.RLock()
	defer s.peersMtx.RUnlock()

	connected := s.connectedPeers()
	result := make([]Peer, 0, len(connected))

	for originalAddr, peer := range s.trackedPeers {
		_, isConnected := connected[peer.resolvedName]
		delete(connected, peer.resolvedName)
		result = append(result, Peer{
			Addr:      originalAddr,
			Connected: isConnected,
			Source:    peer.source.toPeerSource(),
		})
	}
	for addr := range connected {
		result = append(result, Peer{
			Addr:      addr,
			Connected: true,
			Source:    PeerDiscovered,
		})
	}
	return result, nil
}

func (s *walletSPVPeerManager) resolveAddress(addr string) (string, error) {
	host, strPort, err := net.SplitHostPort(addr)
	if err != nil {
		switch err.(type) {
		case *net.AddrError:
			host = addr
			strPort = s.defaultPort
		default:
			return "", err
		}
	}
	if strings.HasSuffix(host, ".onion") {
		return addr, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("no addresses found for %s", host)
	}
	var ip string
	if host == "localhost" && len(ips) > 1 {
		ip = "127.0.0.1"
	} else {
		ip = ips[0].String()
	}
	return net.JoinHostPort(ip, strPort), nil
}

func (s *walletSPVPeerManager) peerWithResolvedAddr(resolvedAddr string) (string, bool) {
	for originalAddr, peer := range s.trackedPeers {
		if peer.resolvedName == resolvedAddr {
			return originalAddr, true
		}
	}
	return "", false
}

func (s *walletSPVPeerManager) loadSavedPeers() (map[string]walletPeerSource, error) {
	content, err := os.ReadFile(s.savedPeersFilePath)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]walletPeerSource), nil
	}
	if err != nil {
		return nil, err
	}
	var peers map[string]walletPeerSource
	if err = json.Unmarshal(content, &peers); err != nil {
		return nil, err
	}
	return peers, nil
}

func (s *walletSPVPeerManager) writeSavedPeers(peers map[string]walletPeerSource) error {
	content, err := json.Marshal(peers)
	if err != nil {
		return err
	}
	return os.WriteFile(s.savedPeersFilePath, content, 0644)
}

func (s *walletSPVPeerManager) addPeer(addr string, source walletPeerSource, initialLoad bool) error {
	s.peersMtx.Lock()
	defer s.peersMtx.Unlock()

	resolvedAddr, err := s.resolveAddress(addr)
	if err != nil {
		if initialLoad {
			s.trackedPeers[addr] = &walletTrackedPeer{source: source}
		}
		return fmt.Errorf("failed to resolve address: %w", err)
	}

	if dupPeer, found := s.peerWithResolvedAddr(resolvedAddr); found {
		return fmt.Errorf("%s and %s resolve to the same node", dupPeer, addr)
	}

	s.trackedPeers[addr] = &walletTrackedPeer{source: source, resolvedName: resolvedAddr}

	if !initialLoad {
		saved, err := s.loadSavedPeers()
		if err != nil {
			s.log.Errorf("failed to load saved peers: %v", err)
		} else {
			saved[addr] = source
			if err = s.writeSavedPeers(saved); err != nil {
				s.log.Errorf("failed to write saved peers: %v", err)
			}
		}
	}

	connected := s.connectedPeers()
	if _, ok := connected[resolvedAddr]; !ok {
		return s.cs.AddPeer(resolvedAddr)
	}
	return nil
}

func (s *walletSPVPeerManager) removePeer(addr string) error {
	s.peersMtx.Lock()
	defer s.peersMtx.Unlock()

	peer, found := s.trackedPeers[addr]
	if !found {
		return fmt.Errorf("peer not found: %v", addr)
	}

	saved, err := s.loadSavedPeers()
	if err != nil {
		return err
	}
	delete(saved, addr)
	if err = s.writeSavedPeers(saved); err != nil {
		s.log.Errorf("failed to write saved peers after removal: %v", err)
	} else {
		delete(s.trackedPeers, addr)
	}

	connected := s.connectedPeers()
	if _, ok := connected[peer.resolvedName]; ok {
		return s.cs.RemovePeer(peer.resolvedName)
	}
	return nil
}

func (s *walletSPVPeerManager) connectToInitialPeers() {
	for _, peer := range s.defaultPeers {
		if err := s.addPeer(peer, peerDefault, true); err != nil {
			s.log.Errorf("failed to add default peer %s: %v", peer, err)
		}
	}
	saved, err := s.loadSavedPeers()
	if err != nil {
		s.log.Errorf("failed to load saved peers: %v", err)
		return
	}
	for addr := range saved {
		if err := s.addPeer(addr, peerUserAdded, true); err != nil {
			s.log.Errorf("failed to add saved peer %s: %v", addr, err)
		}
	}
}

// ─── newBackend dispatches on BackendKind ─────────────────────────────────────

// newBackend constructs and returns an unstarted walletBackend of the given kind.
func newBackend(kind BackendKind, np *netparams.ChainParams, dataDir string, logger backendLogger) (walletBackend, error) {
	switch kind {
	case BackendElectrum:
		return newElectrumBackend(np, dataDir)
	case BackendNeutrino:
		return newNeutrinoBackend(np, dataDir, logger)
	default:
		return nil, fmt.Errorf("unknown backend kind %d", kind)
	}
}
