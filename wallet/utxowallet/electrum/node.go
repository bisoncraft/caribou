// Copyright (c) 2024 The Caribou Developers
// Copyright (c) The go-electrum-client authors
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/wire"
)

// ErrNotConnected is returned when a Node API call is attempted on a Node
// whose underlying Server has not been started.
var ErrNotConnected = errors.New("electrum: node not connected")

const (
	// protoMin and protoMax bound the electrumx protocol versions we
	// negotiate. 1.4 is the standard for spesmilo/electrumx on regtest.
	protoMin = "1.4"
	protoMax = "1.4.2"

	// userAgent identifies the client to the server in server.version.
	userAgent = "Caribou"
)

// handshakeTimeout bounds the server.version and server.features round trips
// of a fresh connection. Elections run under the rotation mutex, so a server
// that accepts the socket but never answers must not hold rotations hostage
// on the network context. A variable so tests can tighten it.
var handshakeTimeout = 10 * time.Second

// Node is a single connected electrumx peer. It wraps a Server and exposes
// the JSON-RPC methods we use.
//
// A Node is one-shot: once its underlying Server's Done channel closes, the
// caller should discard the Node and dial a fresh one if needed.
type Node struct {
	addr   *ServerAddr
	server *Server

	softwareVersion string
	protocolVersion string
}

// NewNode dials addr and performs the electrumx handshake (server.version +
// server.features). On success the returned Node is ready for use; on failure
// any partially-opened connection is closed before returning.
//
// If torProxy is non-empty (e.g. "127.0.0.1:9050"), the connection is dialed
// through a local SOCKS5 proxy.
func NewNode(ctx context.Context, addr *ServerAddr, torProxy string, expectedGenesis string) (*Node, error) {
	if err := addr.Validate(); err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr.Addr)
	if err != nil {
		return nil, err
	}

	opts := &dialOptions{SOCKS5Proxy: torProxy}
	if addr.Proto == "ssl" {
		opts.TLSConfig = serverTLSConfig(addr, host)
	}

	srv, err := dial(ctx, addr.Addr, opts)
	if err != nil {
		return nil, err
	}

	n := &Node{
		addr:   addr,
		server: srv,
	}

	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	vers, err := n.serverVersion(hctx)
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("electrum: server.version: %w", err)
	}
	n.softwareVersion = vers[0]
	n.protocolVersion = vers[1]

	feats, err := n.serverFeatures(hctx)
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("electrum: server.features: %w", err)
	}
	if expectedGenesis != "" && feats.GenesisHash != expectedGenesis {
		srv.Close()
		return nil, fmt.Errorf("electrum: server genesis %s does not match expected %s",
			feats.GenesisHash, expectedGenesis)
	}

	log.Infof("electrum: connected to %s over %s (server %s, proto %s, genesis %s)",
		addr.Addr, addr.Proto, n.softwareVersion, n.protocolVersion, feats.GenesisHash)

	return n, nil
}

// serverTLSConfig builds the tls.Config for an "ssl" electrum server, selecting
// one of three verification modes:
//
//   - Pinned (addr.Cert set): the leaf certificate the server presents must
//     byte-match the pinned DER. Used for self-signed servers that cannot chain
//     to a public CA. Default chain/hostname verification is disabled in favor
//     of the exact-cert pin.
//   - Skip (addr.SkipVerify): no verification at all. Only for dev/testnet/simnet
//     servers with throwaway self-signed certificates.
//   - CA (neither set): standard verification against the host's system root
//     store. Used for public mainnet servers with CA-issued certificates, and
//     survives certificate rotation (e.g. Let's Encrypt renewals).
func serverTLSConfig(addr *ServerAddr, host string) *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: host,
	}
	switch {
	case len(addr.Cert) > 0:
		pinned := addr.Cert
		// Disable the default chain/hostname check; the pin below is the
		// verification. The pinned leaf is the server's identity, so the
		// hostname is not separately checked.
		cfg.InsecureSkipVerify = true //nolint:gosec // verified by the pin in VerifyPeerCertificate
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("electrum: server presented no certificate")
			}
			if !bytes.Equal(rawCerts[0], pinned) {
				return fmt.Errorf("electrum: %s certificate does not match the pinned certificate", addr.Addr)
			}
			return nil
		}
	case addr.SkipVerify:
		cfg.InsecureSkipVerify = true //nolint:gosec // dev/testnet/simnet self-signed certificates
	}
	// Default: InsecureSkipVerify false and RootCAs nil → verify against the
	// system root store.
	return cfg
}

// Addr returns the server address this node is connected to.
func (n *Node) Addr() *ServerAddr { return n.addr }

// Done returns a channel that closes after the node has fully shut down.
func (n *Node) Done() <-chan struct{} { return n.server.Done() }

// Close initiates shutdown.
func (n *Node) Close() { n.server.Close() }

// ----------------------------------------------------------------------------
// JSON-RPC API
// ----------------------------------------------------------------------------

// serverVersion issues `server.version`.
func (n *Node) serverVersion(ctx context.Context) ([2]string, error) {
	var out [2]string
	args := positional{userAgent, []string{protoMin, protoMax}}
	if err := n.server.request(ctx, "server.version", args, &out); err != nil {
		return out, err
	}
	return out, nil
}

// serverFeatures issues `server.features`.
func (n *Node) serverFeatures(ctx context.Context) (*ServerFeatures, error) {
	var out ServerFeatures
	if err := n.server.request(ctx, "server.features", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BlockHeader returns the serialized block header (hex-encoded) at height.
func (n *Node) BlockHeader(ctx context.Context, height int32) (string, error) {
	var hex string
	if err := n.server.request(ctx, "blockchain.block.header", positional{height}, &hex); err != nil {
		return "", err
	}
	return hex, nil
}

// BlockHeadersResult is the result shape of `blockchain.block.headers`.
type BlockHeadersResult struct {
	// Count is the number of headers actually returned, which may be less
	// than the request count if we are near the chain tip or if the server
	// caps responses below the requested count (see Max).
	Count int `json:"count"`
	// HexConcat is the concatenated raw bytes of the returned headers,
	// hex-encoded. Length == Count * HeaderSize * 2.
	HexConcat string `json:"hex"`
	// Max is the server's maximum allowed batch size.
	Max int `json:"max"`
}

// BlockHeaders issues `blockchain.block.headers` for a batch of headers
// starting at startHeight.
func (n *Node) BlockHeaders(ctx context.Context, startHeight int32, count int) (*BlockHeadersResult, error) {
	var out BlockHeadersResult
	if err := n.server.request(ctx, "blockchain.block.headers",
		positional{startHeight, count}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SubscribeHeaders issues `blockchain.headers.subscribe` and returns the
// current tip. Subsequent tip changes arrive on the channel returned by
// HeadersNotifyChan.
func (n *Node) SubscribeHeaders(ctx context.Context) (*HeadersTip, error) {
	var out HeadersTip
	if err := n.server.request(ctx, "blockchain.headers.subscribe", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HeadersNotifyChan returns the underlying server's headers notification
// channel.
func (n *Node) HeadersNotifyChan() <-chan *HeadersTip { return n.server.HeadersNotifyChan() }

// SubscribeScripthash issues `blockchain.scripthash.subscribe` for a single
// scripthash and returns the current status.
func (n *Node) SubscribeScripthash(ctx context.Context, scripthash string) (*ScripthashStatus, error) {
	var status string
	if err := n.server.request(ctx, "blockchain.scripthash.subscribe",
		positional{scripthash}, &status); err != nil {
		return nil, err
	}
	return &ScripthashStatus{Scripthash: scripthash, Status: status}, nil
}

// UnsubscribeScripthash issues `blockchain.scripthash.unsubscribe`. Errors
// are logged but not returned; an unsubscribe failure is not interesting to
// most callers.
func (n *Node) UnsubscribeScripthash(ctx context.Context, scripthash string) {
	var ok bool
	if err := n.server.request(ctx, "blockchain.scripthash.unsubscribe",
		positional{scripthash}, &ok); err != nil {
		log.Debugf("electrum: scripthash unsubscribe %s: %v", scripthash, err)
	}
}

// ScripthashNotifyChan returns the underlying server's scripthash notification
// channel.
func (n *Node) ScripthashNotifyChan() <-chan *ScripthashStatus {
	return n.server.ScripthashNotifyChan()
}

// GetHistory issues `blockchain.scripthash.get_history`.
func (n *Node) GetHistory(ctx context.Context, scripthash string) ([]HistoryEntry, error) {
	var out []HistoryEntry
	if err := n.server.request(ctx, "blockchain.scripthash.get_history",
		positional{scripthash}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListUnspent issues `blockchain.scripthash.listunspent`.
func (n *Node) ListUnspent(ctx context.Context, scripthash string) ([]UnspentEntry, error) {
	var out []UnspentEntry
	if err := n.server.request(ctx, "blockchain.scripthash.listunspent",
		positional{scripthash}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTransactionVerbose issues `blockchain.transaction.get` with verbose=true.
// Note: some servers do not support verbose mode; callers should fall back to
// GetRawTransaction.
func (n *Node) GetTransactionVerbose(ctx context.Context, txid string) (*VerboseTx, error) {
	var out VerboseTx
	if err := n.server.request(ctx, "blockchain.transaction.get",
		positional{txid, true}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRawTransaction issues `blockchain.transaction.get` with verbose=false,
// returning the hex-encoded raw transaction.
//
// The returned bytes are verified to actually hash (double-SHA256, reversed)
// to the requested txid. Without this check, a hostile server can substitute
// arbitrary bytes for a real txid and we would record a transaction that was
// never on chain. This is a cheap, one-shot bound — no Merkle proof, but
// real bytes for the right id.
func (n *Node) GetRawTransaction(ctx context.Context, txid string) (string, error) {
	var out string
	if err := n.server.request(ctx, "blockchain.transaction.get",
		positional{txid, false}, &out); err != nil {
		return "", err
	}
	if err := verifyTxidMatchesHex(txid, out); err != nil {
		return "", err
	}
	return out, nil
}

// verifyTxidMatchesHex returns nil if outHex deserializes to a transaction
// whose txid equals the requested txid. The check is a cheap defence against a
// server returning the wrong bytes for a requested txid.
//
// We must hash the no-witness serialization, not the raw bytes. electrum's
// blockchain.transaction.get returns the full network serialization, which for
// a segwit tx carries the marker/flag/witness; SHA256d of those bytes is the
// wtxid, not the txid. wire.MsgTx.TxHash hashes the witness-stripped form,
// which is exactly what the txid commits to — so this works for both legacy
// and segwit transactions.
func verifyTxidMatchesHex(txid, outHex string) error {
	raw, err := hex.DecodeString(outHex)
	if err != nil {
		return fmt.Errorf("electrum: decode tx hex: %w", err)
	}
	// wire.MsgTx parses BTC and standard (including segwit) LTC transactions.
	// It does NOT understand LTC MWEB transactions (extension flag 0x08),
	// which fail here just as the previous SHA256d-of-raw check failed to
	// match their txid — verifying those requires chain-aware hashing
	// (bisonwire's litcoinTxHash) and remains a known limitation rather than
	// a regression.
	r := bytes.NewReader(raw)
	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(r); err != nil {
		return fmt.Errorf("electrum: deserialize tx: %w", err)
	}
	// Reject trailing bytes. Deserialize stops at the transaction boundary, so
	// a server appending arbitrary bytes after a valid tx would otherwise pass
	// — the prior whole-buffer hash would have rejected it.
	if r.Len() != 0 {
		return fmt.Errorf("electrum: tx hex has %d trailing bytes after the transaction", r.Len())
	}
	got := msgTx.TxHash().String()
	if got != txid {
		return fmt.Errorf("electrum: tx hex hashes to %s, requested %s", got, txid)
	}
	return nil
}

// Broadcast issues `blockchain.transaction.broadcast` and returns the txid
// reported by the server.
func (n *Node) Broadcast(ctx context.Context, rawTxHex string) (string, error) {
	var txid string
	if err := n.server.request(ctx, "blockchain.transaction.broadcast",
		positional{rawTxHex}, &txid); err != nil {
		return "", err
	}
	return txid, nil
}

// EstimateFee issues `blockchain.estimatefee` for a given confirmation target
// and returns the fee rate in satoshis per kilobyte.
//
// The protocol returns a floating-point value in coin units per kilobyte, or
// -1 when the daemon lacks enough data. We surface -1 as an error so callers
// can fall back to a default rate.
func (n *Node) EstimateFee(ctx context.Context, confTarget int32) (int64, error) {
	var coinsPerKB float64
	if err := n.server.request(ctx, "blockchain.estimatefee",
		positional{confTarget}, &coinsPerKB); err != nil {
		return 0, err
	}
	if coinsPerKB <= 0 {
		return 0, errors.New("electrum: server cannot estimate fee for given target")
	}
	return int64(coinsPerKB * 1e8), nil
}

// maxAdvertisedPeers bounds how many peers ServerPeers parses from a single
// server.peers.subscribe reply. Honest deployments advertise a few dozen
// peers; a hostile server can pack tens of thousands of fabricated entries
// into one reply, each of which would become a stored pool candidate and a
// potential dial. Entries past the cap are ignored. (The wire-level frame
// size is separately bounded by maxFrameSize in server.go.)
const maxAdvertisedPeers = 64

// ServerPeers issues `server.peers.subscribe` (a one-shot query despite the
// name) and parses the reply into dialable ssl candidates, at most
// maxAdvertisedPeers of them. Each raw entry is [ip, hostname, [features...]]
// where features include a protocol version ("v1.4") and port tags ("s50002"
// = ssl, "t50001" = tcp). Only ssl-capable peers speaking protocol 1.4+ are
// returned; the recommendation conveys no trust — candidates prove themselves
// at handshake like any other server.
func (n *Node) ServerPeers(ctx context.Context) ([]*ServerAddr, error) {
	var raw [][]interface{}
	if err := n.server.request(ctx, "server.peers.subscribe", nil, &raw); err != nil {
		return nil, err
	}
	var out []*ServerAddr
	for _, entry := range raw {
		if len(out) == maxAdvertisedPeers {
			break
		}
		if len(entry) < 3 {
			continue
		}
		host, _ := entry[1].(string)
		feats, _ := entry[2].([]interface{})
		if host == "" || feats == nil {
			continue
		}
		var sslPort, version string
		for _, f := range feats {
			fs, _ := f.(string)
			switch {
			case strings.HasPrefix(fs, "s") && len(fs) > 1:
				sslPort = fs[1:]
			case strings.HasPrefix(fs, "v"):
				version = fs[1:]
			}
		}
		if sslPort == "" || version == "" {
			continue
		}
		if !protoAtLeast(version, 1, 4) {
			continue
		}
		out = append(out, &ServerAddr{
			Proto:      "ssl",
			Addr:       host + ":" + sslPort,
			SkipVerify: true,
		})
	}
	return out, nil
}

// protoAtLeast reports whether a server-advertised protocol version string
// ("1.4", "1.4.2", "1.10") is at least major.minor. Numeric comparison —
// lexicographic ordering would reject a future "1.10".
func protoAtLeast(version string, major, minor int) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min_, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return maj > major || (maj == major && min_ >= minor)
}

// GetMerkle issues `blockchain.transaction.get_merkle`: the merkle branch
// proving a confirmed transaction's inclusion in the block at height.
func (n *Node) GetMerkle(ctx context.Context, txid string, height int32) (*MerkleProof, error) {
	var out MerkleProof
	if err := n.server.request(ctx, "blockchain.transaction.get_merkle",
		positional{txid, height}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
