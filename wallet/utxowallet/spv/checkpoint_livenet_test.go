// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

//go:build livenet

// Live verification of the checkpoint records against public P2P peers. For
// each checkpoint, filter-serving peers found through the DNS seeds return the
// block header (getheaders with an empty locator and the checkpoint hash as the
// stop hash) and the filter header (a one-block getcfheaders). The header must
// hash to the checkpoint hash, and the filter header must agree across peers.
// Records that lack a header or filter header are printed as Go literals for
// the assets package, so adding a checkpoint only requires its height and
// hash:
//
//	go test -tags livenet -run TestCheckpointRecords -v ./wallet/utxowallet/spv/

package spv

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/bisoncraft/caribou/wallet/utxowallet/peer"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/wire"
)

// minFilterPeers is the number of peers that must return the same filter
// header for a checkpoint. The block header needs only one, since it is
// checked against the checkpoint hash.
const minFilterPeers = 3

func TestCheckpointRecords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		chain  bisonwire.Chain
		params *netparams.ChainParams
	}{
		{"btc-mainnet", bisonwire.ChainBTC, assets.BTCParams["mainnet"]},
		{"btc-testnet", bisonwire.ChainBTC, assets.BTCParams["testnet"]},
		{"ltc-mainnet", bisonwire.ChainLTC, assets.LTCParams["mainnet"]},
		{"ltc-testnet", bisonwire.ChainLTC, assets.LTCParams["testnet"]},
		{"dash-mainnet", bisonwire.ChainDASH, assets.DashParams["mainnet"]},
		{"dash-testnet", bisonwire.ChainDASH, assets.DashParams["testnet"]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()

			peers := connectFilterPeers(ctx, t, tc.chain, tc.params, minFilterPeers+2)
			defer func() {
				for _, fp := range peers {
					fp.Disconnect()
				}
			}()
			if len(peers) < minFilterPeers {
				t.Fatalf("connected to %d filter peers, need %d", len(peers), minFilterPeers)
			}

			var literals []string
			var incomplete bool
			for _, cp := range tc.params.Checkpoints {
				hdr, fh := checkpointData(ctx, t, tc.params, peers, cp)
				literals = append(literals, checkpointLiteral(cp, hdr, fh))
				if cp.Header == nil || cp.FilterHeader == nil {
					incomplete = true
					continue
				}
				if *cp.Header != *hdr {
					t.Errorf("checkpoint %d: recorded header differs from the network's", cp.Height)
				}
				if *cp.FilterHeader != fh {
					t.Errorf("checkpoint %d: recorded filter header %s, network %s",
						cp.Height, cp.FilterHeader, fh)
				}
			}
			if incomplete {
				t.Logf("%s checkpoints:\n%s", tc.name, strings.Join(literals, "\n"))
			}
		})
	}
}

// checkpointData asks every peer for the checkpoint's header and filter
// header. It fails the test unless at least one header hashes to the
// checkpoint hash and at least minFilterPeers peers return the same filter
// header with none returning a different one.
func checkpointData(ctx context.Context, t *testing.T, params *netparams.ChainParams,
	peers []*filterPeer, cp netparams.Checkpoint) (*wire.BlockHeader, chainhash.Hash) {

	t.Helper()
	var hdr *wire.BlockHeader
	votes := make(map[chainhash.Hash][]string)
	for _, fp := range peers {
		h, err := fp.header(ctx, params, cp.Hash)
		if err != nil {
			t.Logf("checkpoint %d: header from %s: %v", cp.Height, fp.Addr(), err)
		} else if hdr == nil {
			hdr = h
		}
		fh, err := fp.filterHeader(ctx, cp)
		if err != nil {
			t.Logf("checkpoint %d: filter header from %s: %v", cp.Height, fp.Addr(), err)
			continue
		}
		votes[fh] = append(votes[fh], fp.Addr())
	}
	if hdr == nil {
		t.Fatalf("checkpoint %d: no peer returned a header with hash %s", cp.Height, cp.Hash)
	}
	if len(votes) != 1 {
		t.Fatalf("checkpoint %d: peers disagree on the filter header: %v", cp.Height, votes)
	}
	for fh, addrs := range votes {
		if len(addrs) < minFilterPeers {
			t.Fatalf("checkpoint %d: filter header %s from %d peers, need %d",
				cp.Height, fh, len(addrs), minFilterPeers)
		}
		return hdr, fh
	}
	panic("unreachable")
}

func checkpointLiteral(cp netparams.Checkpoint, hdr *wire.BlockHeader, fh chainhash.Hash) string {
	var b bytes.Buffer
	if err := hdr.Serialize(&b); err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{
	Height:       %d, // %s
	Hash:         newHashFromStr("%s"),
	Header:       newHeaderFromStr("%s"),
	FilterHeader: newHashFromStr("%s"),
},`, cp.Height, hdr.Timestamp.UTC().Format(time.DateOnly), cp.Hash,
		hex.EncodeToString(b.Bytes()), fh)
}

// filterPeer is an outbound peer that serves compact filters, with the
// responses this test requests routed to channels.
type filterPeer struct {
	*peer.Peer
	headers   chan *wire.MsgHeaders
	cfheaders chan *wire.MsgCFHeaders
}

// requestTimeout bounds each request to a single peer.
const requestTimeout = 20 * time.Second

// header fetches the header for hash. An empty block locator makes the peer
// return the header of the stop hash alone.
func (fp *filterPeer) header(ctx context.Context, params *netparams.ChainParams,
	hash *chainhash.Hash) (*wire.BlockHeader, error) {

	msg := wire.NewMsgGetHeaders()
	msg.HashStop = *hash
	fp.QueueMessage(msg, nil)
	select {
	case resp := <-fp.headers:
		if len(resp.Headers) != 1 {
			return nil, fmt.Errorf("got %d headers, want 1", len(resp.Headers))
		}
		if h := params.HeaderHash(resp.Headers[0]); h != *hash {
			return nil, fmt.Errorf("header hash %s", h)
		}
		return resp.Headers[0], nil
	case <-time.After(requestTimeout):
		return nil, errors.New("timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// filterHeader fetches the basic filter header of the checkpoint block from a
// one-block getcfheaders: the response carries the previous filter header and
// the block's filter hash, and the filter header is their double SHA-256.
func (fp *filterPeer) filterHeader(ctx context.Context, cp netparams.Checkpoint) (chainhash.Hash, error) {
	fp.QueueMessage(wire.NewMsgGetCFHeaders(wire.GCSFilterRegular, uint32(cp.Height), cp.Hash), nil)
	select {
	case resp := <-fp.cfheaders:
		if resp.StopHash != *cp.Hash {
			return chainhash.Hash{}, fmt.Errorf("stop hash %s", resp.StopHash)
		}
		if len(resp.FilterHashes) != 1 {
			return chainhash.Hash{}, fmt.Errorf("got %d filter hashes, want 1", len(resp.FilterHashes))
		}
		var b [2 * chainhash.HashSize]byte
		copy(b[:], resp.FilterHashes[0][:])
		copy(b[chainhash.HashSize:], resp.PrevFilterHeader[:])
		return chainhash.DoubleHashH(b[:]), nil
	case <-time.After(requestTimeout):
		return chainhash.Hash{}, errors.New("timed out")
	case <-ctx.Done():
		return chainhash.Hash{}, ctx.Err()
	}
}

// connectFilterPeers dials addresses from the DNS seeds and the configured
// neutrino seeds until want peers complete the handshake, or the candidates run
// out. A peer must advertise compact filters and a height at or above the
// newest checkpoint. Stalled nodes still accept connections but ignore
// requests for blocks they do not have.
func connectFilterPeers(ctx context.Context, t *testing.T, chain bisonwire.Chain,
	params *netparams.ChainParams, want int) []*filterPeer {

	t.Helper()
	services := wire.SFNodeNetwork | wire.SFNodeCF
	if chain != bisonwire.ChainDASH {
		services |= wire.SFNodeWitness
	}
	var minHeight int32
	if n := len(params.Checkpoints); n > 0 {
		minHeight = params.Checkpoints[n-1].Height
	}

	var mtx sync.Mutex
	seen := make(map[string]bool)
	var candidates []string
	add := func(addr string) {
		mtx.Lock()
		defer mtx.Unlock()
		if !seen[addr] {
			seen[addr] = true
			candidates = append(candidates, addr)
		}
	}
	for _, addr := range params.NeutrinoSeeds {
		add(addr)
	}
	connmgr.SeedFromDNS(params.BTCDParams(), services, net.LookupIP, func(addrs []*wire.NetAddressV2) {
		for _, na := range addrs {
			add(net.JoinHostPort(na.Addr.String(), strconv.Itoa(int(na.Port))))
		}
	})
	// SeedFromDNS resolves in the background.
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return nil
	}

	mtx.Lock()
	addrs := append([]string(nil), candidates...)
	mtx.Unlock()
	t.Logf("%d candidate addresses", len(addrs))

	// Dialing stops once enough peers have connected. Peers that finish
	// the handshake after that are dropped.
	dialCtx, stopDialing := context.WithCancel(ctx)
	defer stopDialing()
	var peers []*filterPeer
	results := make(chan *filterPeer)
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-dialCtx.Done():
				return
			}
			defer func() { <-sem }()
			fp, err := dialFilterPeer(dialCtx, chain, params, addr, minHeight)
			if err != nil {
				return
			}
			results <- fp
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	for fp := range results {
		if len(peers) >= want {
			fp.Disconnect()
			continue
		}
		t.Logf("filter peer %s (%s, height %d)", fp.Addr(), fp.UserAgent(), fp.StartingHeight())
		peers = append(peers, fp)
		if len(peers) == want {
			stopDialing()
		}
	}
	return peers
}

func dialFilterPeer(ctx context.Context, chain bisonwire.Chain, params *netparams.ChainParams,
	addr string, minHeight int32) (*filterPeer, error) {

	verack := make(chan struct{})
	fp := &filterPeer{
		headers:   make(chan *wire.MsgHeaders, 4),
		cfheaders: make(chan *wire.MsgCFHeaders, 4),
	}
	cfg := &peer.Config{
		Chain:            chain,
		Net:              params.Net,
		UserAgentName:    "caribou-checkpoints",
		UserAgentVersion: "0.1.0",
		ProtocolVersion:  protocolVersionForChain(chain),
		DisableRelayTx:   true,
		Listeners: peer.MessageListeners{
			OnVerAck: func(*peer.Peer, *wire.MsgVerAck) { close(verack) },
			OnHeaders: func(_ *peer.Peer, msg *wire.MsgHeaders) {
				select {
				case fp.headers <- msg:
				default:
				}
			},
			OnCFHeaders: func(_ *peer.Peer, msg *wire.MsgCFHeaders) {
				select {
				case fp.cfheaders <- msg:
				default:
				}
			},
		},
	}
	p, err := peer.NewOutboundPeer(cfg, addr)
	if err != nil {
		return nil, err
	}
	fp.Peer = p
	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	p.AssociateConnection(conn)
	select {
	case <-verack:
	case <-time.After(10 * time.Second):
		p.Disconnect()
		return nil, errors.New("handshake timed out")
	case <-ctx.Done():
		p.Disconnect()
		return nil, ctx.Err()
	}
	if p.Services()&wire.SFNodeCF == 0 {
		p.Disconnect()
		return nil, errors.New("no compact filters")
	}
	if h := p.StartingHeight(); h < minHeight {
		p.Disconnect()
		return nil, fmt.Errorf("height %d is below %d", h, minHeight)
	}
	return fp, nil
}
