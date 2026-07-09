//go:build harness

package btc

// Simnet tests expect the BTC test harness to be running.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/util/feeratefetcher"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog"
	"github.com/decred/dcrd/rpcclient/v8"
)

const (
	gammaSeed = "1285a47d6a59f9c548b2a72c2c34a2de97967bede3844090102bbba76707fe9d"
)

var (
	tLogger        util.Logger
	tCtx           context.Context
	walletPassword = []byte("abc")
)

// mineAlpha sends the mine-alpha script to the btc-harness tmux session. The
// script lives in the harness-ctl window (window 2); window 0 runs the alpha
// daemon in the foreground and would swallow the keystrokes.
func mineAlpha() error {
	return exec.Command("tmux", "send-keys", "-t", "btc-harness:2", "./mine-alpha 1", "C-m").Run()
}

// harnessNode is a thin funding node backed by a harness bitcoind's RPC
// interface. It provides the small surface the simnet tests need from the
// alpha/beta funders — connect, unlock, and send — without depending on any
// wallet implementation. Wallet-under-test coverage lives in the electrum,
// neutrino, and native-reconnect simnet tests; this only drives the funder.
type harnessNode struct {
	cl *rpcclient.Client
}

// Connect satisfies util.Connector so the node can be managed by a
// util.ConnectionMaster. The RPC client is HTTP-POST based, so there is no
// persistent connection to maintain; the client is shut down when ctx is
// canceled (i.e. on ConnectionMaster.Disconnect).
func (h *harnessNode) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		h.cl.Shutdown()
	}()
	return &wg, nil
}

// Unlock unlocks the node's wallet via walletpassphrase.
func (h *harnessNode) Unlock(pw []byte) error {
	passJSON, _ := json.Marshal(string(pw))
	_, err := h.cl.RawRequest(tCtx, "walletpassphrase", []json.RawMessage{passJSON, json.RawMessage("600")})
	return err
}

// Send sends value (in atoms) to addr via sendtoaddress and returns the funding
// output. feeRate is ignored; the harness node uses its own fee policy.
func (h *harnessNode) Send(addr string, val, _ uint64) (asset.Coin, error) {
	addrJSON, _ := json.Marshal(addr)
	amtJSON, _ := json.Marshal(btcutil.Amount(val).ToBTC())
	b, err := h.cl.RawRequest(tCtx, "sendtoaddress", []json.RawMessage{addrJSON, amtJSON})
	if err != nil {
		return nil, err
	}
	var txID string
	if err := json.Unmarshal(b, &txID); err != nil {
		return nil, fmt.Errorf("error decoding sendtoaddress result: %w", err)
	}
	txHash, err := chainhash.NewHashFromStr(txID)
	if err != nil {
		return nil, fmt.Errorf("error decoding txid %q: %w", txID, err)
	}
	return NewOutput(txHash, 0, val), nil
}

// tBackend connects a harnessNode to the named harness bitcoind (alpha, beta)
// using its dextest config file, and wraps it in a ConnectionMaster. isInternal
// and blkFunc are retained for call-site compatibility and unused.
func tBackend(t *testing.T, name string, _ bool, _ func(string, error)) (*harnessNode, *util.ConnectionMaster) {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("error getting current user: %v", err)
	}
	cfgPath := filepath.Join(u.HomeDir, "dextest", "btc", name, name+".conf")
	var rpcCfg RPCConfig
	if err := config.ParseInto(cfgPath, &rpcCfg); err != nil {
		t.Fatalf("error reading %s config: %v", name, err)
	}
	if err := CheckRPCConfig(&rpcCfg, name, util.Simnet, RPCPorts); err != nil {
		t.Fatalf("bad %s rpc config: %v", name, err)
	}
	cl, err := rpcclient.New(&rpcclient.ConnConfig{
		HTTPPostMode: true,
		DisableTLS:   true,
		// The harness bitcoind is multi-wallet, so wallet RPCs must target the
		// /wallet/<name> URI path. An empty walletname selects the default wallet.
		Host: rpcCfg.RPCBind + "/wallet/" + rpcCfg.WalletName,
		User: rpcCfg.RPCUser,
		Pass: rpcCfg.RPCPass,
	}, nil)
	if err != nil {
		t.Fatalf("error creating %s rpc client: %v", name, err)
	}

	node := &harnessNode{cl: cl}
	cm := util.NewConnectionMaster(node)
	if err := cm.Connect(tCtx); err != nil {
		t.Fatalf("error connecting %s backend: %v", name, err)
	}
	return node, cm
}

func randBytes(l int) []byte {
	b := make([]byte, l)
	rand.Read(b)
	return b
}

func TestMain(m *testing.M) {
	tLogger = util.StdOutLogger("TEST", util.LevelTrace)
	// Connect no longer sets up the utxowallet (btclog) subsystem logging, so
	// route it to stdout here to keep NTRNO/TXMGR/CHAIN/… visible in tests.
	utxowallet.UseLogger(btclog.NewBackend(os.Stdout))
	var shutdown func()
	tCtx, shutdown = context.WithCancel(context.Background())
	doIt := func() int {
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestExternalFeeRate(t *testing.T) {
	fetchRateWithTimeout(t, util.Mainnet)
	fetchRateWithTimeout(t, util.Testnet)
}

func fetchRateWithTimeout(t *testing.T, net util.Network) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()
	f := feeratefetcher.New(BitcoinFeeSources(), tLogger)
	feeRate := f.Rate(ctx)
	if feeRate == 0 {
		t.Fatalf("no fee rate fetched for %s", net)
	}
	fmt.Printf("##### Fee rate fetched for %s! %d Sats/vB \n", net, feeRate)
}
