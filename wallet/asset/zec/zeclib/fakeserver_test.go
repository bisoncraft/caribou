// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"slices"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

// fakeLwd is a lightwalletd server over a synthetic chain of blocks without
// transactions. The note commitment trees of such a chain are empty at every
// height, so its blocks and tree states are consistent input for the real
// wallet backend as well as for the fake one.
//
// A test controls it through the chain methods (mine, reorg, truncate), by
// planting transparent outputs and transactions, and by making calls fail or
// block.
type fakeLwd struct {
	walletrpc.UnimplementedCompactTxStreamerServer

	addr string
	lis  net.Listener
	srv  *grpc.Server

	mtx       sync.Mutex
	chainName string
	// branch is the consensus branch that the server reports for its tip,
	// as eight hexadecimal digits. An empty one is not reported.
	branch string
	// lag is the number of blocks by which the server's node estimates the
	// chain to be ahead of its tip.
	lag uint64
	// blocks is indexed by height. Block 0 is the genesis block.
	blocks []*walletrpc.CompactBlock
	// fork distinguishes the hashes of blocks mined after a reorg from the
	// ones they replace.
	fork byte
	// tip, if set, replaces the reply of GetLatestBlock.
	tip *walletrpc.BlockID
	// treeStateHeight, if not zero, is the height GetTreeState answers for,
	// whatever height is asked.
	treeStateHeight uint64

	utxos   map[string][]*walletrpc.GetAddressUtxosReply
	txs     map[string]*walletrpc.RawTransaction
	addrTxs map[string][]*walletrpc.RawTransaction
	roots   map[walletrpc.ShieldedProtocol][]*walletrpc.SubtreeRoot

	// fail maps a method name to the error it returns.
	fail map[string]error
	// failOnce maps a method name to an error it returns one time.
	failOnce map[string]error
	// failNth maps a method name to the number of the call to it, counted
	// from 1, that fails the way a lost connection does.
	failNth map[string]int
	// failEvery maps a method name to n, where every nth call to it fails
	// the way a lost connection does.
	failEvery map[string]int
	// rootsFail maps a shielded pool to the error GetSubtreeRoots returns
	// for it.
	rootsFail map[walletrpc.ShieldedProtocol]error
	// txFail maps a transaction ID to the error GetTransaction returns for
	// it.
	txFail map[string]error
	// rangeOverrun is the number of blocks GetBlockRange sends past the
	// end of the requested range.
	rangeOverrun uint64
	// gate maps a method name to a channel that its calls wait on.
	gate map[string]chan struct{}
	// entered receives the name of each method call as it starts, if set.
	entered chan string
	calls   map[string]int
	// addrQueries records the address arguments of GetTaddressTxids calls.
	addrQueries []*walletrpc.TransparentAddressBlockFilter
	// utxoQueries records the arguments of GetAddressUtxosStream calls.
	utxoQueries []*walletrpc.GetAddressUtxosArg
	// rootQueries records the arguments of GetSubtreeRoots calls.
	rootQueries []*walletrpc.GetSubtreeRootsArg
}

// newFakeLwd starts a server with a chain of the given height.
func newFakeLwd(t *testing.T, height uint32) *fakeLwd {
	t.Helper()
	s := &fakeLwd{
		chainName: "test",
		utxos:     make(map[string][]*walletrpc.GetAddressUtxosReply),
		txs:       make(map[string]*walletrpc.RawTransaction),
		addrTxs:   make(map[string][]*walletrpc.RawTransaction),
		roots:     make(map[walletrpc.ShieldedProtocol][]*walletrpc.SubtreeRoot),
		fail:      make(map[string]error),
		failOnce:  make(map[string]error),
		failNth:   make(map[string]int),
		failEvery: make(map[string]int),
		rootsFail: make(map[walletrpc.ShieldedProtocol]error),
		txFail:    make(map[string]error),
		gate:      make(map[string]chan struct{}),
		calls:     make(map[string]int),
	}
	s.blocks = []*walletrpc.CompactBlock{s.makeBlock(0, make([]byte, 32))}
	s.mine(height)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.addr = lis.Addr().String()
	s.serve(lis)
	t.Cleanup(s.stop)
	return s
}

func (s *fakeLwd) serve(lis net.Listener) {
	s.lis = lis
	s.srv = grpc.NewServer()
	walletrpc.RegisterCompactTxStreamerServer(s.srv, s)
	go s.srv.Serve(lis)
}

// stop shuts the server down, failing the calls in flight.
func (s *fakeLwd) stop() {
	s.mtx.Lock()
	srv, lis := s.srv, s.lis
	s.srv = nil
	s.mtx.Unlock()
	if srv != nil {
		srv.Stop()
		// Stop closes the listener only once Serve has taken it over, which
		// a stop right after a start can be ahead of.
		lis.Close()
	}
}

// restart brings a stopped server back on the same address.
func (s *fakeLwd) restart(t *testing.T) {
	t.Helper()
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		t.Fatalf("listen on %s again: %v", s.addr, err)
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.serve(lis)
}

func fakeBlockHash(height uint32, fork byte) []byte {
	hash := slices.Repeat([]byte{fork}, 32)
	binary.LittleEndian.PutUint32(hash, height)
	return hash
}

func (s *fakeLwd) makeBlock(height uint32, prevHash []byte) *walletrpc.CompactBlock {
	return &walletrpc.CompactBlock{
		Height:        uint64(height),
		Hash:          fakeBlockHash(height, s.fork),
		PrevHash:      prevHash,
		Time:          1_700_000_000 + height,
		ChainMetadata: &walletrpc.ChainMetadata{},
	}
}

// mine extends the chain by n blocks.
func (s *fakeLwd) mine(n uint32) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.mineLocked(n)
}

func (s *fakeLwd) mineLocked(n uint32) {
	for range n {
		prev := s.blocks[len(s.blocks)-1]
		s.blocks = append(s.blocks, s.makeBlock(uint32(len(s.blocks)), prev.Hash))
	}
}

// reorg replaces the last depth blocks with depth+grow new ones.
func (s *fakeLwd) reorg(depth, grow uint32) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.blocks = s.blocks[:len(s.blocks)-int(depth)]
	s.fork++
	s.mineLocked(depth + grow)
}

// height returns the height of the chain tip.
func (s *fakeLwd) height() uint32 {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return uint32(len(s.blocks) - 1)
}

// hashAt returns the hash of the block at height.
func (s *fakeLwd) hashAt(height uint32) []byte {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.blocks[height].Hash
}

// set runs f with the server state locked.
func (s *fakeLwd) set(f func(s *fakeLwd)) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	f(s)
}

// callCount returns the number of calls made to a method.
func (s *fakeLwd) callCount(method string) int {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.calls[method]
}

// enter records a call and applies the failure and gate set for the method.
func (s *fakeLwd) enter(ctx context.Context, method string) error {
	s.mtx.Lock()
	s.calls[method]++
	err := s.fail[method]
	if once, ok := s.failOnce[method]; ok {
		delete(s.failOnce, method)
		err = once
	}
	if n, ok := s.failNth[method]; ok && n == s.calls[method] {
		err = status.Error(codes.Unavailable, "the connection was lost")
	}
	if n := s.failEvery[method]; n > 0 && s.calls[method]%n == 0 {
		err = status.Error(codes.Unavailable, "the connection was lost")
	}
	gate := s.gate[method]
	entered := s.entered
	s.mtx.Unlock()

	if entered != nil {
		select {
		case entered <- method:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	return err
}

func (s *fakeLwd) GetLightdInfo(ctx context.Context, _ *walletrpc.Empty) (*walletrpc.LightdInfo, error) {
	if err := s.enter(ctx, "GetLightdInfo"); err != nil {
		return nil, err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return &walletrpc.LightdInfo{
		Version:           "fake",
		Vendor:            "zeclib test",
		ChainName:         s.chainName,
		ConsensusBranchId: s.branch,
		BlockHeight:       uint64(len(s.blocks) - 1),
		EstimatedHeight:   uint64(len(s.blocks)-1) + s.lag,
	}, nil
}

func (s *fakeLwd) GetLatestBlock(ctx context.Context, _ *walletrpc.ChainSpec) (*walletrpc.BlockID, error) {
	if err := s.enter(ctx, "GetLatestBlock"); err != nil {
		return nil, err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.tip != nil {
		return s.tip, nil
	}
	tip := s.blocks[len(s.blocks)-1]
	return &walletrpc.BlockID{Height: tip.Height, Hash: tip.Hash}, nil
}

func (s *fakeLwd) GetBlock(ctx context.Context, id *walletrpc.BlockID) (*walletrpc.CompactBlock, error) {
	if err := s.enter(ctx, "GetBlock"); err != nil {
		return nil, err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if id.Height >= uint64(len(s.blocks)) {
		return nil, status.Error(codes.OutOfRange, "block not found")
	}
	return s.blocks[id.Height], nil
}

func (s *fakeLwd) GetBlockRange(r *walletrpc.BlockRange, stream grpc.ServerStreamingServer[walletrpc.CompactBlock]) error {
	if err := s.enter(stream.Context(), "GetBlockRange"); err != nil {
		return err
	}
	s.mtx.Lock()
	var blocks []*walletrpc.CompactBlock
	for h := r.Start.Height; h <= r.End.Height+s.rangeOverrun && h < uint64(len(s.blocks)); h++ {
		blocks = append(blocks, s.blocks[h])
	}
	s.mtx.Unlock()
	for _, b := range blocks {
		if err := stream.Send(b); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeLwd) GetTreeState(ctx context.Context, id *walletrpc.BlockID) (*walletrpc.TreeState, error) {
	if err := s.enter(ctx, "GetTreeState"); err != nil {
		return nil, err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if id.Height == 0 || id.Height >= uint64(len(s.blocks)) {
		// zebrad rejects height 0 with this message.
		return nil, status.Error(codes.InvalidArgument, "block id must specify a hash or a height")
	}
	height := id.Height
	if s.treeStateHeight != 0 {
		height = s.treeStateHeight
	}
	display := slices.Clone(s.blocks[height].Hash)
	slices.Reverse(display)
	return &walletrpc.TreeState{
		Network: s.chainName,
		Height:  height,
		Hash:    hex.EncodeToString(display),
		Time:    s.blocks[height].Time,
	}, nil
}

func (s *fakeLwd) GetSubtreeRoots(arg *walletrpc.GetSubtreeRootsArg, stream grpc.ServerStreamingServer[walletrpc.SubtreeRoot]) error {
	if err := s.enter(stream.Context(), "GetSubtreeRoots"); err != nil {
		return err
	}
	s.mtx.Lock()
	s.rootQueries = append(s.rootQueries, arg)
	if err := s.rootsFail[arg.ShieldedProtocol]; err != nil {
		s.mtx.Unlock()
		return err
	}
	roots := s.roots[arg.ShieldedProtocol]
	if int(arg.StartIndex) < len(roots) {
		roots = roots[arg.StartIndex:]
	} else {
		roots = nil
	}
	s.mtx.Unlock()
	for _, r := range roots {
		if err := stream.Send(r); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeLwd) GetAddressUtxosStream(arg *walletrpc.GetAddressUtxosArg, stream grpc.ServerStreamingServer[walletrpc.GetAddressUtxosReply]) error {
	if err := s.enter(stream.Context(), "GetAddressUtxosStream"); err != nil {
		return err
	}
	s.mtx.Lock()
	s.utxoQueries = append(s.utxoQueries, arg)
	var replies []*walletrpc.GetAddressUtxosReply
	for _, addr := range arg.Addresses {
		for _, u := range s.utxos[addr] {
			if u.Height >= arg.StartHeight {
				replies = append(replies, u)
			}
		}
	}
	s.mtx.Unlock()
	for _, u := range replies {
		if err := stream.Send(u); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeLwd) GetTransaction(ctx context.Context, f *walletrpc.TxFilter) (*walletrpc.RawTransaction, error) {
	if err := s.enter(ctx, "GetTransaction"); err != nil {
		return nil, err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if err := s.txFail[string(f.Hash)]; err != nil {
		return nil, err
	}
	tx, ok := s.txs[string(f.Hash)]
	if !ok {
		return nil, status.Error(codes.NotFound, "Transaction not found")
	}
	return tx, nil
}

func (s *fakeLwd) GetTaddressTxids(f *walletrpc.TransparentAddressBlockFilter, stream grpc.ServerStreamingServer[walletrpc.RawTransaction]) error {
	if err := s.enter(stream.Context(), "GetTaddressTxids"); err != nil {
		return err
	}
	s.mtx.Lock()
	s.addrQueries = append(s.addrQueries, f)
	var txs []*walletrpc.RawTransaction
	for _, tx := range s.addrTxs[f.Address] {
		if tx.Height >= f.Range.Start.Height && tx.Height <= f.Range.End.Height {
			txs = append(txs, tx)
		}
	}
	s.mtx.Unlock()
	for _, tx := range txs {
		if err := stream.Send(tx); err != nil {
			return err
		}
	}
	return nil
}
