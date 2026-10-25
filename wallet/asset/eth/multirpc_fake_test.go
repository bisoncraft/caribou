//go:build !harness && !rpclive

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/erc20"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// fakeEthNode is a minimal, hand-rolled Ethereum JSON-RPC backend: just
// enough of the eth_* namespace for multirpc.go's provider-level methods
// (which all wrap a real *ethclient.Client) to be exercised without a live
// chain. This is a deliberately smaller alternative to go-ethereum's
// ethclient/simulated package, which pulls in the full node/p2p/eth/catalyst
// stack (~35 extra indirect dependencies) just to serve tests.
//
// Its Go method names map to RPC method names by go-ethereum's rpc package
// convention: the exported method's first letter is lowercased and prefixed
// with the registered namespace (GetBalance -> "eth_getBalance").
type fakeEthNode struct {
	mu sync.Mutex

	chainID *big.Int
	number  uint64 // current head block number; advanced by mine()

	balances map[common.Address]*big.Int
	// nonces is the pending nonce, bumped immediately by SendRawTransaction.
	// confirmedNonces only catches up to it on mine(), so a caller asking for
	// the "pending" block tag and one asking for "latest" can actually get
	// different answers (see GetTransactionCount).
	nonces          map[common.Address]uint64
	confirmedNonces map[common.Address]uint64
	code            map[common.Address][]byte
	receipts        map[common.Hash]*types.Receipt
	txs             map[common.Hash]*types.Transaction

	gasPrice *big.Int
	tipCap   *big.Int

	// tokenBalances answers eth_call's balanceOf(address), keyed by
	// [token contract address][account address], for erc20TokenContractor
	// tests. Any other contract call still returns empty data (see Call).
	tokenBalances map[common.Address]map[common.Address]*big.Int
}

func newFakeEthNode(chainID *big.Int) *fakeEthNode {
	return &fakeEthNode{
		chainID:         chainID,
		number:          1,
		balances:        make(map[common.Address]*big.Int),
		nonces:          make(map[common.Address]uint64),
		confirmedNonces: make(map[common.Address]uint64),
		code:            make(map[common.Address][]byte),
		receipts:        make(map[common.Hash]*types.Receipt),
		txs:             make(map[common.Hash]*types.Transaction),
		gasPrice:        big.NewInt(4_000_000_000), // 4 gwei
		// Above MinGasTipCap (params.go), so suggestTipCap's floor doesn't
		// mask this value in assertions.
		tipCap:        big.NewInt(3_000_000_000), // 3 gwei
		tokenBalances: make(map[common.Address]map[common.Address]*big.Int),
	}
}

// setTokenBalance sets the ERC20 balanceOf response a token contract gives
// for an account.
func (n *fakeEthNode) setTokenBalance(tokenAddr, acctAddr common.Address, bal *big.Int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.tokenBalances[tokenAddr] == nil {
		n.tokenBalances[tokenAddr] = make(map[common.Address]*big.Int)
	}
	n.tokenBalances[tokenAddr][acctAddr] = bal
}

// seedTxAndReceipt registers a transaction and a successful receipt for it
// directly, without going through SendRawTransaction, for tests that need a
// specific, pre-known transaction hash (e.g. compatibility-check fixtures).
func (n *fakeEthNode) seedTxAndReceipt(tx *types.Transaction) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.txs[tx.Hash()] = tx
	n.receipts[tx.Hash()] = &types.Receipt{
		Status:      types.ReceiptStatusSuccessful,
		TxHash:      tx.Hash(),
		BlockNumber: new(big.Int).SetUint64(n.number),
		Logs:        []*types.Log{},
	}
}

// fund credits an account's balance, as a genesis allocation would.
func (n *fakeEthNode) fund(addr common.Address, wei *big.Int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.balances[addr] = new(big.Int).Set(wei)
}

// mine advances the head block number, as if a block were produced. Any
// transaction accepted by SendRawTransaction already has a receipt recorded
// at the block number current when it was sent, so mining afterward is what
// gives it confirmations.
func (n *fakeEthNode) mine() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.number++
	maps.Copy(n.confirmedNonces, n.nonces)
}

// headNumber, nonceOf and hasTx give tests a locked read of state SendRawTransaction
// and mine mutate, instead of reaching into the struct's fields directly.
func (n *fakeEthNode) headNumber() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.number
}

func (n *fakeEthNode) nonceOf(addr common.Address) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nonces[addr]
}

func (n *fakeEthNode) hasTx(hash common.Hash) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.txs[hash] != nil
}

func (n *fakeEthNode) header() *types.Header {
	return n.headerForNumber(n.number)
}

// headerForNumber deterministically derives a header for any block number up
// to the current head, so headers for past blocks can be recomputed and
// looked up by hash. Caller holds n.mu.
func (n *fakeEthNode) headerForNumber(number uint64) *types.Header {
	// A zero LondonBlock-less config (see newTestMultiRPC) means
	// eip1559.CalcBaseFee never dereferences BaseFee, so it can stay nil.
	return &types.Header{
		ParentHash: common.Hash{},
		Number:     new(big.Int).SetUint64(number),
		Difficulty: new(big.Int),
		GasLimit:   30_000_000,
		GasUsed:    21_000,
		Time:       number, // monotonic; exact value is unchecked in Simnet mode
		Extra:      []byte{},
	}
}

func (n *fakeEthNode) GetBalance(_ context.Context, addr common.Address, _ string) (*hexutil.Big, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	bal := n.balances[addr]
	if bal == nil {
		bal = new(big.Int)
	}
	return (*hexutil.Big)(bal), nil
}

func (n *fakeEthNode) GetBlockByNumber(_ context.Context, _ string, _ bool) (*types.Header, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.header(), nil
}

// GetBlockByHash walks the deterministic headers for every block up to the
// current head looking for one whose hash matches, returning not-found (nil,
// nil) for a hash the fake has not produced, e.g. the zero hash.
func (n *fakeEthNode) GetBlockByHash(_ context.Context, hash common.Hash, _ bool) (*types.Header, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for num := uint64(1); num <= n.number; num++ {
		hdr := n.headerForNumber(num)
		if hdr.Hash() == hash {
			return hdr, nil
		}
	}
	return nil, nil
}

func (n *fakeEthNode) GetCode(_ context.Context, addr common.Address, _ string) (hexutil.Bytes, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.code[addr], nil
}

// GetTransactionCount answers eth_getTransactionCount. The block tag matters:
// "pending" reflects every SendRawTransaction so far, while any other tag
// (e.g. "latest") reflects only what mine() has confirmed.
func (n *fakeEthNode) GetTransactionCount(_ context.Context, addr common.Address, blockTag string) (*hexutil.Uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var count uint64
	if blockTag == "pending" {
		count = n.nonces[addr]
	} else {
		count = n.confirmedNonces[addr]
	}
	v := hexutil.Uint64(count)
	return &v, nil
}

func (n *fakeEthNode) GasPrice(context.Context) (*hexutil.Big, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return (*hexutil.Big)(n.gasPrice), nil
}

func (n *fakeEthNode) MaxPriorityFeePerGas(context.Context) (*hexutil.Big, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return (*hexutil.Big)(n.tipCap), nil
}

// EstimateGas derives its answer from the call's data length instead of
// returning a flat constant, so a contractor call that packs the wrong
// method or arguments produces a different, checkable gas figure rather than
// always passing.
func (n *fakeEthNode) EstimateGas(_ context.Context, callMsg map[string]any) (hexutil.Uint64, error) {
	inputHex, _ := callMsg["input"].(string)
	data, _ := hexutil.Decode(inputHex)
	return hexutil.Uint64(21_000 + 16*len(data)), nil
}

// Call answers eth_call. It only understands the ERC20 balanceOf(address)
// selector, against tokenBalances seeded by setTokenBalance; every other
// call (including balanceOf for an unseeded token/account pair) returns
// empty data, matching a contract that reverts or a method this fake doesn't
// model.
func (n *fakeEthNode) Call(_ context.Context, callMsg map[string]any, _ string) (hexutil.Bytes, error) {
	inputHex, _ := callMsg["input"].(string)
	toHex, _ := callMsg["to"].(string)
	if inputHex == "" || toHex == "" {
		return hexutil.Bytes{}, nil
	}
	data, err := hexutil.Decode(inputHex)
	if err != nil || len(data) < 4 {
		return hexutil.Bytes{}, nil
	}
	method, err := erc20.ERC20ABI.MethodById(data[:4])
	if err != nil || method.Name != "balanceOf" {
		return hexutil.Bytes{}, nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	bal := n.tokenBalances[common.HexToAddress(toHex)][n.callBalanceOfArg(data)]
	if bal == nil {
		return hexutil.Bytes{}, nil
	}
	return method.Outputs.Pack(bal)
}

// callBalanceOfArg decodes the account argument out of a packed
// balanceOf(address) call.
func (n *fakeEthNode) callBalanceOfArg(data []byte) common.Address {
	args, err := erc20.ERC20ABI.Methods["balanceOf"].Inputs.Unpack(data[4:])
	if err != nil || len(args) != 1 {
		return common.Address{}
	}
	addr, _ := args[0].(common.Address)
	return addr
}

func (n *fakeEthNode) GetLogs(context.Context, any) ([]types.Log, error) {
	return []types.Log{}, nil
}

func (n *fakeEthNode) ChainId(context.Context) (*hexutil.Big, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return (*hexutil.Big)(n.chainID), nil
}

func (n *fakeEthNode) GetTransactionByHash(_ context.Context, hash common.Hash) (*types.Transaction, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.txs[hash], nil // nil, nil -> ethclient/rpcTransaction decode as "not found"
}

func (n *fakeEthNode) SendRawTransaction(_ context.Context, raw hexutil.Bytes) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, fmt.Errorf("bad raw tx: %w", err)
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	signer := types.LatestSignerForChainID(n.chainID)
	from, err := types.Sender(signer, tx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("recover sender: %w", err)
	}
	n.nonces[from]++

	n.txs[tx.Hash()] = tx
	n.receipts[tx.Hash()] = &types.Receipt{
		Status:      types.ReceiptStatusSuccessful,
		TxHash:      tx.Hash(),
		BlockNumber: new(big.Int).SetUint64(n.number),
		Logs:        []*types.Log{},
	}
	return tx.Hash(), nil
}

func (n *fakeEthNode) GetTransactionReceipt(_ context.Context, hash common.Hash) (*types.Receipt, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.receipts[hash], nil // nil, nil -> ethclient translates this to ethereum.NotFound
}

// testMultiRPC bundles a multiRPCClient wired to a fakeEthNode via an
// in-process JSON-RPC dial (no sockets, no goroutine leaks to clean up
// beyond the server itself) with the credentials of its one funded account.
type testMultiRPC struct {
	node  *fakeEthNode
	creds *accountCredentials
	m     *multiRPCClient
}

func newTestMultiRPC(t *testing.T) *testMultiRPC {
	t.Helper()

	ks := newTestKeyStore(t)
	acct, err := ks.NewAccount(string(testKeyStorePW))
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Unlock(acct, string(testKeyStorePW)); err != nil {
		t.Fatal(err)
	}
	creds, err := credentialsFromKeyStore(ks)
	if err != nil {
		t.Fatal(err)
	}

	chainID := big.NewInt(1337)
	node := newFakeEthNode(chainID)
	oneEther := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	node.fund(creds.addr, new(big.Int).Mul(oneEther, big.NewInt(100)))

	srv := rpc.NewServer()
	if err := srv.RegisterName("eth", node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	rpcClient := rpc.DialInProc(srv)
	t.Cleanup(rpcClient.Close)

	p := &provider{
		host: "fake",
		ec:   &combinedRPCClient{Client: ethclient.NewClient(rpcClient), rpc: rpcClient},
		// Simnet skips bestHeader's wall-clock staleness check, which would
		// otherwise reject this fake node's arbitrary header timestamps.
		net: util.Simnet,
	}

	m := &multiRPCClient{
		log: tLogger,
		// No fork blocks set, so config.IsLondon is always false and
		// eip1559.CalcBaseFee never touches header.BaseFee (see
		// fakeEthNode.header).
		cfg:       &params.ChainConfig{ChainID: chainID},
		chainID:   chainID,
		net:       util.Simnet,
		creds:     creds,
		providers: []*provider{p},
	}
	m.receipts.cache = make(map[common.Hash]*receiptRecord)
	m.receipts.lastClean = time.Now()

	return &testMultiRPC{node: node, creds: creds, m: m}
}

func ethereumCallMsg(from, to common.Address) ethereum.CallMsg {
	return ethereum.CallMsg{From: from, To: &to, Value: big.NewInt(1)}
}

func ethereumFilterQuery(addr common.Address) ethereum.FilterQuery {
	return ethereum.FilterQuery{Addresses: []common.Address{addr}}
}

func TestMultiRPCClientAgainstFakeNode(t *testing.T) {
	t.Parallel()

	t.Run("address, chainConfig, contractBackend, peerCount", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		if s.m.address() != s.creds.addr {
			t.Errorf("address() = %s, want %s", s.m.address(), s.creds.addr)
		}
		if s.m.chainConfig() != s.m.cfg {
			t.Error("chainConfig() should return the configured *params.ChainConfig")
		}
		if s.m.contractBackend() != s.m {
			t.Error("contractBackend() should return the multiRPCClient itself")
		}
		if got := s.m.peerCount(); got != 1 {
			t.Errorf("peerCount() = %d, want 1 (one unfailed provider)", got)
		}
	})

	t.Run("addressBalance", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		bal, err := s.m.addressBalance(context.Background(), s.creds.addr)
		if err != nil {
			t.Fatal(err)
		}
		if bal.Sign() <= 0 {
			t.Errorf("addressBalance = %s, want the funded genesis balance", bal)
		}
	})

	t.Run("bestHeader, headerByHash, HeaderByNumber", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		hdr, err := s.m.bestHeader(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if hdr == nil || hdr.Number.Uint64() != s.node.headNumber() {
			t.Fatalf("bestHeader = %+v, want block %d", hdr, s.node.headNumber())
		}

		byHash, err := s.m.headerByHash(context.Background(), hdr.Hash())
		if err != nil {
			t.Fatal(err)
		}
		if byHash.Number.Uint64() != hdr.Number.Uint64() {
			t.Errorf("headerByHash number = %d, want %d", byHash.Number.Uint64(), hdr.Number.Uint64())
		}

		byNumber, err := s.m.HeaderByNumber(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if byNumber.Number.Uint64() != hdr.Number.Uint64() {
			t.Errorf("HeaderByNumber number = %d, want %d", byNumber.Number.Uint64(), hdr.Number.Uint64())
		}
	})

	t.Run("CodeAt, PendingCodeAt, PendingNonceAt", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		// The funded account is an EOA, so it has no code.
		code, err := s.m.CodeAt(context.Background(), s.creds.addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 0 {
			t.Errorf("CodeAt = %x, want no code for an EOA", code)
		}
		if _, err := s.m.PendingCodeAt(context.Background(), s.creds.addr); err != nil {
			t.Fatal(err)
		}
		nonce, err := s.m.PendingNonceAt(context.Background(), s.creds.addr)
		if err != nil {
			t.Fatal(err)
		}
		if nonce != 0 {
			t.Errorf("PendingNonceAt = %d, want 0 before any transaction", nonce)
		}
	})

	t.Run("SuggestGasPrice and SuggestGasTipCap", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		price, err := s.m.SuggestGasPrice(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if price.Cmp(s.node.gasPrice) != 0 {
			t.Errorf("SuggestGasPrice = %s, want %s", price, s.node.gasPrice)
		}
		tipCap, err := s.m.SuggestGasTipCap(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tipCap.Cmp(s.node.tipCap) != 0 {
			t.Errorf("SuggestGasTipCap = %s, want %s", tipCap, s.node.tipCap)
		}
	})

	t.Run("EstimateGas and CallContract", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		to := common.HexToAddress("0x1111111111111111111111111111111111111111")
		gas, err := s.m.EstimateGas(context.Background(), ethereumCallMsg(s.creds.addr, to))
		if err != nil {
			t.Fatal(err)
		}
		if gas == 0 {
			t.Error("EstimateGas = 0, want a positive gas estimate")
		}
		if _, err := s.m.CallContract(context.Background(), ethereumCallMsg(s.creds.addr, to), nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("FilterLogs with no matches", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		logs, err := s.m.FilterLogs(context.Background(), ethereumFilterQuery(s.creds.addr))
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) != 0 {
			t.Errorf("FilterLogs = %d logs, want 0", len(logs))
		}
	})

	t.Run("nonce", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		confirmed, pending, err := s.m.nonce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if confirmed.Sign() != 0 || pending.Sign() != 0 {
			t.Errorf("nonce = (%s, %s), want (0, 0) before any transaction", confirmed, pending)
		}

		// A sent-but-unmined transaction must bump the pending nonce without
		// bumping the confirmed one, or this test can't tell "latest" and
		// "pending" apart.
		to := common.HexToAddress("0x3333333333333333333333333333333333333333")
		txOpts, err := s.m.txOpts(context.Background(), 0, 21000, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.m.sendTransaction(context.Background(), txOpts, &to, nil); err != nil {
			t.Fatal(err)
		}
		confirmed, pending, err = s.m.nonce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if confirmed.Sign() != 0 {
			t.Errorf("nonce confirmed = %s, want 0 before the tx is mined", confirmed)
		}
		if pending.Uint64() != 1 {
			t.Errorf("nonce pending = %s, want 1 after one sent transaction", pending)
		}

		s.node.mine()
		confirmed, _, err = s.m.nonce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if confirmed.Uint64() != 1 {
			t.Errorf("nonce confirmed = %s, want 1 after mine()", confirmed)
		}
	})

	t.Run("currentFees and txOpts", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		base, tip, err := s.m.currentFees(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if base.Sign() <= 0 {
			t.Errorf("currentFees base = %s, want positive", base)
		}
		if tip.Cmp(s.node.tipCap) != 0 {
			t.Errorf("currentFees tip = %s, want %s", tip, s.node.tipCap)
		}

		txOpts, err := s.m.txOpts(context.Background(), 0, 21000, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if txOpts.Nonce == nil || txOpts.Nonce.Sign() != 0 {
			t.Fatalf("txOpts.Nonce = %v, want 0 (no prior transactions)", txOpts.Nonce)
		}
		if txOpts.Signer == nil {
			t.Fatal("txOpts.Signer is nil")
		}

		// A caller-supplied nonce should be used as-is, with no lookup.
		txOpts2, err := s.m.txOpts(context.Background(), 0, 21000, base, tip, big.NewInt(7))
		if err != nil {
			t.Fatal(err)
		}
		if txOpts2.Nonce.Uint64() != 7 {
			t.Errorf("txOpts.Nonce = %d, want the supplied 7", txOpts2.Nonce.Uint64())
		}
	})

	t.Run("sendTransaction, transactionConfirmations, syncProgress", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		to := common.HexToAddress("0x2222222222222222222222222222222222222222")
		txOpts, err := s.m.txOpts(context.Background(), 0, 21000, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.m.sendTransaction(context.Background(), txOpts, &to, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.node.mine() // advance the head so the tx has a confirmation

		confs, err := s.m.transactionConfirmations(context.Background(), tx.Hash())
		if err != nil {
			t.Fatal(err)
		}
		if confs == 0 {
			t.Error("transactionConfirmations = 0 after the tx was mined")
		}

		if got := s.node.nonceOf(s.creds.addr); got != 1 {
			t.Errorf("sender nonce after send = %d, want 1", got)
		}

		prog, tipTime, err := s.m.syncProgress(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if prog == nil {
			t.Fatal("syncProgress returned a nil progress")
		}
		if tipTime == 0 {
			t.Error("syncProgress tipTime = 0")
		}
	})

	t.Run("unknown transaction confirmations errors", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		var unknownHash common.Hash
		unknownHash[0] = 0xff
		if _, err := s.m.transactionConfirmations(context.Background(), unknownHash); err == nil {
			t.Error("expected an error for an unmined transaction hash")
		}
	})

	t.Run("lock, locked, unlock", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		if s.m.locked() {
			t.Fatal("account should start unlocked (newTestMultiRPC unlocks it)")
		}
		if err := s.m.lock(); err != nil {
			t.Fatal(err)
		}
		if !s.m.locked() {
			t.Error("locked() = false after lock()")
		}
		if err := s.m.unlock(string(testKeyStorePW)); err != nil {
			t.Fatal(err)
		}
		if s.m.locked() {
			t.Error("locked() = true after unlock()")
		}
	})

	t.Run("signHash and signData via multiRPCClient", func(t *testing.T) {
		t.Parallel()
		s := newTestMultiRPC(t)
		hash := make([]byte, 32)
		sig, pub, err := s.m.signHash(hash)
		if err != nil {
			t.Fatal(err)
		}
		if len(sig) != 65 || len(pub) == 0 {
			t.Fatalf("signHash returned sig=%d bytes pub=%d bytes", len(sig), len(pub))
		}
		sig2, pub2, err := s.m.signData([]byte("some data"))
		if err != nil {
			t.Fatal(err)
		}
		if len(sig2) != 65 || len(pub2) == 0 {
			t.Fatalf("signData returned sig=%d bytes pub=%d bytes", len(sig2), len(pub2))
		}
	})
}

func TestProbeFailedProviders(t *testing.T) {
	tm := newTestMultiRPC(t)
	p := tm.m.providers[0]

	// A provider still in quarantine is left alone.
	p.setFailed()
	tm.m.probeFailedProviders(t.Context())
	if !p.failed() {
		t.Fatal("probe returned a quarantined provider to service")
	}

	// A provider past brickedFailCount is skipped by every request, so only
	// the probe can return it to service.
	p.tip.Lock()
	p.tip.failCount = brickedFailCount + 1
	p.tip.failStamp = time.Now().Add(-failQuarantine)
	p.tip.Unlock()
	if !p.failed() {
		t.Fatal("bricked provider not failed")
	}
	tm.m.probeFailedProviders(t.Context())
	if p.failed() {
		t.Fatal("probe did not return a responsive provider to service")
	}
}
