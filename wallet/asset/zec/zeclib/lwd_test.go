// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

func dialFake(t *testing.T, srv *fakeLwd) *lwdClient {
	t.Helper()
	c, err := dialLightwalletd(&Config{Server: srv.addr, Plaintext: true})
	if err != nil {
		t.Fatalf("dialLightwalletd: %v", err)
	}
	t.Cleanup(func() { c.close() })
	return c
}

func TestClientCalls(t *testing.T) {
	srv := newFakeLwd(t, 50)
	c := dialFake(t, srv)
	ctx := context.Background()

	info, err := c.info(ctx)
	if err != nil || info.ChainName != "test" || info.BlockHeight != 50 {
		t.Errorf("info: %+v, %v", info, err)
	}
	tip, err := c.latestBlock(ctx)
	if err != nil || tip.Height != 50 || !sameHash(tip.Hash, srv.hashAt(50)) {
		t.Errorf("latestBlock: %+v, %v", tip, err)
	}
	block, err := c.block(ctx, 20)
	if err != nil || block.Height != 20 || !sameHash(block.PrevHash, srv.hashAt(19)) {
		t.Errorf("block: %+v, %v", block, err)
	}
	state, err := c.treeState(ctx, 20)
	if err != nil || state.Height != 20 || len(state.Hash) != 64 {
		t.Errorf("treeState: %+v, %v", state, err)
	}

	var heights []uint64
	err = c.blockRange(ctx, 10, 14, func(b *walletrpc.CompactBlock) error {
		heights = append(heights, b.Height)
		return nil
	})
	if err != nil || len(heights) != 5 || heights[0] != 10 || heights[4] != 14 {
		t.Errorf("blockRange: %v, %v", heights, err)
	}

	// A callback can end the stream early, or fail it with its own error.
	heights = nil
	err = c.blockRange(ctx, 10, 40, func(b *walletrpc.CompactBlock) error {
		heights = append(heights, b.Height)
		if len(heights) == 3 {
			return errStopStream
		}
		return nil
	})
	if err != nil || len(heights) != 3 {
		t.Errorf("blockRange stopped early: %v, %v", heights, err)
	}
	boom := errors.New("callback failed")
	if err := c.blockRange(ctx, 10, 40, func(*walletrpc.CompactBlock) error { return boom }); err != boom {
		t.Errorf("blockRange with a failing callback: %v", err)
	}
}

func TestClientSubtreeRoots(t *testing.T) {
	srv := newFakeLwd(t, 50)
	c := dialFake(t, srv)
	ctx := context.Background()
	srv.set(func(s *fakeLwd) {
		s.roots[walletrpc.ShieldedProtocol_orchard] = []*walletrpc.SubtreeRoot{
			{RootHash: []byte{1}, CompletingBlockHeight: 10},
			{RootHash: []byte{2}, CompletingBlockHeight: 20},
			{RootHash: []byte{3}, CompletingBlockHeight: 30},
		}
	})

	roots, err := c.subtreeRoots(ctx, walletrpc.ShieldedProtocol_orchard, 1)
	if err != nil || len(roots) != 2 || roots[0].completingHeight != 20 || roots[1].hash[0] != 3 {
		t.Errorf("subtreeRoots from 1: %+v, %v", roots, err)
	}
	if roots, err := c.subtreeRoots(ctx, walletrpc.ShieldedProtocol_sapling, 0); err != nil || len(roots) != 0 {
		t.Errorf("subtreeRoots of an empty tree: %+v, %v", roots, err)
	}
	if roots, err := c.subtreeRoots(ctx, walletrpc.ShieldedProtocol_orchard, 7); err != nil || len(roots) != 0 {
		t.Errorf("subtreeRoots past the end: %+v, %v", roots, err)
	}
	if _, err := c.subtreeRoots(ctx, walletrpc.ShieldedProtocol_orchard, 1<<40); !isServerError(err) {
		t.Errorf("subtreeRoots with an index that does not fit the protocol: %v", err)
	}

	srv.set(func(s *fakeLwd) {
		s.roots[walletrpc.ShieldedProtocol_ironwood] = []*walletrpc.SubtreeRoot{
			{RootHash: []byte{1}, CompletingBlockHeight: 1 << 40},
		}
	})
	if _, err := c.subtreeRoots(ctx, walletrpc.ShieldedProtocol_ironwood, 0); !isServerError(err) {
		t.Errorf("subtreeRoots with a height out of range: %v", err)
	}
}

func TestClientTransparent(t *testing.T) {
	srv := newFakeLwd(t, 50)
	c := dialFake(t, srv)
	ctx := context.Background()
	srv.set(func(s *fakeLwd) {
		s.utxos["t1"] = []*walletrpc.GetAddressUtxosReply{{Address: "t1", ValueZat: 5, Height: 10}, {Address: "t1", ValueZat: 6, Height: 30}}
		s.utxos["t2"] = []*walletrpc.GetAddressUtxosReply{{Address: "t2", ValueZat: 7, Height: 40}}
		s.addrTxs["t1"] = []*walletrpc.RawTransaction{{Data: []byte{1}, Height: 10}, {Data: []byte{2}, Height: 30}}
		s.txs["known"] = &walletrpc.RawTransaction{Data: []byte{9}, Height: 12}
	})

	var total int64
	err := c.addressUTXOs(ctx, []string{"t1", "t2", "t3"}, 20, func(u *walletrpc.GetAddressUtxosReply) error {
		total += u.ValueZat
		return nil
	})
	if err != nil || total != 13 {
		t.Errorf("addressUTXOs from 20: total %d, %v", total, err)
	}

	var seen []byte
	err = c.addressTransactions(ctx, "t1", 20, 50, func(tx *walletrpc.RawTransaction) error {
		seen = append(seen, tx.Data...)
		return nil
	})
	if err != nil || len(seen) != 1 || seen[0] != 2 {
		t.Errorf("addressTransactions: %v, %v", seen, err)
	}

	tx, err := c.transaction(ctx, []byte("known"))
	if err != nil || tx == nil || tx.Height != 12 {
		t.Errorf("transaction: %+v, %v", tx, err)
	}
	if tx, err := c.transaction(ctx, []byte("unknown")); err != nil || tx != nil {
		t.Errorf("unknown transaction: %+v, %v", tx, err)
	}
}

func TestClientTransactionNotFound(t *testing.T) {
	srv := newFakeLwd(t, 50)
	c := dialFake(t, srv)
	ctx := context.Background()

	// The ways servers report a transaction they do not know.
	notFound := []error{
		status.Error(codes.NotFound, "anything"),
		status.Error(codes.Unknown, "Transaction not found"),
		status.Error(codes.InvalidArgument, "-5: No such mempool or main chain transaction"),
		status.Error(codes.Unknown, "No such mempool transaction. Use -txindex"),
	}
	for _, e := range notFound {
		srv.set(func(s *fakeLwd) { s.fail["GetTransaction"] = e })
		if tx, err := c.transaction(ctx, []byte("x")); err != nil || tx != nil {
			t.Errorf("%v: got %+v, %v", e, tx, err)
		}
	}
	srv.set(func(s *fakeLwd) { s.fail["GetTransaction"] = status.Error(codes.Unavailable, "down") })
	if _, err := c.transaction(ctx, []byte("x")); !isServerError(err) {
		t.Errorf("an unavailable server: %v", err)
	}
	if isTxNotFound(errors.New("transaction not found")) {
		t.Error("an error that is not a gRPC status was read as not-found")
	}
}

func TestServerErrorKinds(t *testing.T) {
	server := func(err error) error { return &serverError{"Call", err} }
	tests := []struct {
		err                     error
		unsupported, connection bool
	}{
		{server(status.Error(codes.Unimplemented, "unknown method")), true, false},
		{server(status.Error(codes.InvalidArgument, "unknown pool")), true, false},
		{server(status.Error(codes.Unavailable, "down")), false, true},
		{server(status.Error(codes.DeadlineExceeded, "slow")), false, true},
		{server(status.Error(codes.Canceled, "cancelled")), false, true},
		{server(status.Error(codes.ResourceExhausted, "busy")), false, true},
		{server(context.Canceled), false, true},
		{server(context.DeadlineExceeded), false, true},
		{server(status.Error(codes.Internal, "boom")), false, false},
		{server(status.Error(codes.NotFound, "no such thing")), false, false},
		{server(errors.New("malformed reply")), false, false},
		// The same statuses outside a server error say nothing about the
		// server.
		{status.Error(codes.Unimplemented, "unknown method"), false, false},
		{status.Error(codes.Unavailable, "down"), false, false},
		{wrapf(ErrDatabase, "disk full"), false, false},
		{nil, false, false},
	}
	for _, tt := range tests {
		if got := isUnsupported(tt.err); got != tt.unsupported {
			t.Errorf("isUnsupported(%v) = %t", tt.err, got)
		}
		if got := isConnectionError(tt.err); got != tt.connection {
			t.Errorf("isConnectionError(%v) = %t", tt.err, got)
		}
	}
}

func TestClientErrorsAreServerErrors(t *testing.T) {
	srv := newFakeLwd(t, 50)
	c := dialFake(t, srv)
	ctx := context.Background()
	down := status.Error(codes.Unavailable, "down")
	nop := func(*walletrpc.CompactBlock) error { return nil }

	calls := map[string]func() error{
		"GetLightdInfo":  func() error { _, err := c.info(ctx); return err },
		"GetLatestBlock": func() error { _, err := c.latestBlock(ctx); return err },
		"GetBlock":       func() error { _, err := c.block(ctx, 1); return err },
		"GetTreeState":   func() error { _, err := c.treeState(ctx, 1); return err },
		"GetBlockRange":  func() error { return c.blockRange(ctx, 1, 2, nop) },
		"GetSubtreeRoots": func() error {
			_, err := c.subtreeRoots(ctx, walletrpc.ShieldedProtocol_sapling, 0)
			return err
		},
		"GetAddressUtxosStream": func() error {
			return c.addressUTXOs(ctx, []string{"t"}, 0, func(*walletrpc.GetAddressUtxosReply) error { return nil })
		},
		"GetTransaction": func() error { _, err := c.transaction(ctx, []byte("x")); return err },
		"GetTaddressTxids": func() error {
			return c.addressTransactions(ctx, "t", 0, 1, func(*walletrpc.RawTransaction) error { return nil })
		},
	}
	for method, call := range calls {
		srv.set(func(s *fakeLwd) { s.fail[method] = down })
		err := call()
		var se *serverError
		if !errors.As(err, &se) || status.Code(se.Unwrap()) != codes.Unavailable {
			t.Errorf("%s: %v", method, err)
		} else if se.Error() == "" || se.op == "" {
			t.Errorf("%s: error without text: %#v", method, se)
		}
		srv.set(func(s *fakeLwd) { delete(s.fail, method) })
	}

	// With the server gone and a cancelled context, every call still
	// returns a server error, promptly.
	srv.stop()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for method, call := range calls {
		ctx = cancelled
		if err := call(); !isServerError(err) {
			t.Errorf("%s with a cancelled context: %v", method, err)
		}
	}
	if isServerError(errors.New("other")) || isServerError(nil) {
		t.Error("isServerError accepts other errors")
	}
}

func TestDialOptions(t *testing.T) {
	srv := newFakeLwd(t, 50)
	ctx := context.Background()

	for _, server := range []string{"", "no-port", "a:b:c"} {
		if _, err := dialLightwalletd(&Config{Server: server}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("dial %q: %v", server, err)
		}
	}

	// TLS is the default, and a plaintext server fails its handshake.
	for _, cfg := range []*Config{
		{Server: srv.addr},
		{Server: srv.addr, TLSConfig: &tls.Config{InsecureSkipVerify: true}},
	} {
		c, err := dialLightwalletd(cfg)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if _, err := c.info(ctx); !isServerError(err) {
			t.Errorf("TLS call to a plaintext server: %v", err)
		}
		c.close()
	}

	// A custom dialer gets the address as configured, host name and all.
	var dials atomic.Int32
	var dialed atomic.Value
	c, err := dialLightwalletd(&Config{
		Server:    "lightwalletd.example:9067",
		Plaintext: true,
		Dialer: func(ctx context.Context, address string) (net.Conn, error) {
			dials.Add(1)
			dialed.Store(address)
			var d net.Dialer
			return d.DialContext(ctx, "tcp", srv.addr)
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	if info, err := c.info(ctx); err != nil || info.BlockHeight != 50 {
		t.Fatalf("info through a custom dialer: %+v, %v", info, err)
	}
	if dials.Load() == 0 || dialed.Load() != "lightwalletd.example:9067" {
		t.Errorf("the dialer was called %d times with %v", dials.Load(), dialed.Load())
	}
}

func TestChainStateBefore(t *testing.T) {
	srv := newFakeLwd(t, 50)
	c := dialFake(t, srv)
	ctx := context.Background()
	network := Regtest(AllUpgradesAt(10))

	if _, err := chainStateBefore(ctx, c, network, 0, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("height 0: %v", err)
	}

	// Above activation the state comes from the server.
	before := srv.callCount("GetTreeState")
	if state, err := chainStateBefore(ctx, c, network, 11, nil); err != nil || len(state) == 0 {
		t.Errorf("above activation: %v", err)
	}
	if got := srv.callCount("GetTreeState"); got != before+1 {
		t.Errorf("GetTreeState calls: %d", got-before)
	}

	// At activation it is built from the parent hash, fetched if needed.
	blocks := srv.callCount("GetBlock")
	fetched, err := chainStateBefore(ctx, c, network, 10, nil)
	if err != nil {
		t.Fatalf("at activation: %v", err)
	}
	if got := srv.callCount("GetBlock"); got != blocks+1 {
		t.Errorf("GetBlock calls: %d", got-blocks)
	}
	given, err := chainStateBefore(ctx, c, network, 10, srv.hashAt(9))
	if err != nil || string(given) != string(fetched) {
		t.Errorf("with the parent hash given: %v", err)
	}
	if got := srv.callCount("GetBlock"); got != blocks+1 {
		t.Errorf("GetBlock was called although the parent hash was given")
	}
	if got := srv.callCount("GetTreeState"); got != before+1 {
		t.Errorf("GetTreeState was called at activation")
	}

	if _, err := chainStateBefore(ctx, c, network, 10, []byte{1, 2, 3}); !isServerError(err) {
		t.Errorf("a short parent hash: %v", err)
	}
	// A server that answers for another height.
	srv.set(func(s *fakeLwd) { s.treeStateHeight = 3 })
	if _, err := chainStateBefore(ctx, c, network, 20, nil); !isServerError(err) {
		t.Errorf("a tree state for another height: %v", err)
	}
}

func TestLatestHeight(t *testing.T) {
	srv := newFakeLwd(t, 50)
	c := dialFake(t, srv)
	ctx := context.Background()
	if h, err := latestHeight(ctx, c); err != nil || h != 50 {
		t.Errorf("latestHeight: %d, %v", h, err)
	}
	srv.set(func(s *fakeLwd) { s.tip = &walletrpc.BlockID{Height: 1 << 33} })
	if _, err := latestHeight(ctx, c); !isServerError(err) {
		t.Errorf("a height out of range: %v", err)
	}
}
