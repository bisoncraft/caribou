//go:build harness

package btc

// Simnet tests expect the BTC test harness to be running.

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/config"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog"
)

const (
	gammaSeed = "1285a47d6a59f9c548b2a72c2c34a2de97967bede3844090102bbba76707fe9d"
)

var (
	tLogger   util.Logger
	tCtx      context.Context
	tLotSize  uint64 = 1e7
	tRateStep uint64 = 100
	tBTC             = &util.Asset{
		ID:         0,
		Symbol:     "btc",
		Version:    version,
		MaxFeeRate: 10,
		SwapConf:   1,
	}
	walletPassword = []byte("abc")
)

func mineAlpha() error {
	return exec.Command("tmux", "send-keys", "-t", "btc-harness:0", "./mine-alpha 1", "C-m").Run()
}

func mineBeta() error {
	return exec.Command("tmux", "send-keys", "-t", "btc-harness:0", "./mine-beta 1", "C-m").Run()
}

func tBackend(t *testing.T, name string, isInternal bool, blkFunc func(string, error)) (*ExchangeWalletFullNode, *util.ConnectionMaster) {
	t.Helper()
	user, err := user.Current()
	if err != nil {
		t.Fatalf("error getting current user: %v", err)
	}
	settings := make(map[string]string)
	if !isInternal {
		cfgPath := filepath.Join(user.HomeDir, "dextest", "btc", name, name+".conf")
		settings, err = config.Parse(cfgPath)
		if err != nil {
			t.Fatalf("error reading config options: %v", err)
		}
	}

	noteChan := make(chan asset.WalletNotification, 128)
	go func() {
		for {
			select {
			case <-noteChan:
			case <-tCtx.Done():
				return
			}
		}
	}()

	walletCfg := &asset.WalletConfig{
		Settings: settings,
		Emit:     asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), 0, tLogger),
		PeersChange: func(num uint32, err error) {
			t.Logf("peer count = %d, err = %v", num, err)
		},
	}
	if isInternal {
		seed, err := hex.DecodeString(gammaSeed)
		if err != nil {
			t.Fatal(err)
		}
		dataDir := t.TempDir()
		regtestDir := filepath.Join(dataDir, chaincfg.RegressionNetParams.Name)
		err = createElectrumWallet(walletPassword, seed, DefaultWalletBirthday, regtestDir, tLogger, 0, 0, &chaincfg.RegressionNetParams)
		if err != nil {
			t.Fatal(err)
		}
		walletCfg.Type = walletTypeSPV
		walletCfg.DataDir = dataDir
	}
	var backend asset.Wallet
	backend, err = NewWallet(walletCfg, tLogger, util.Simnet)
	if err != nil {
		t.Fatalf("error creating backend: %v", err)
	}
	cm := util.NewConnectionMaster(backend)
	err = cm.Connect(tCtx)
	if err != nil {
		t.Fatalf("error connecting backend: %v", err)
	}

	if isInternal {
		i := 0
		for {
			syncStatus, err := backend.SyncStatus()
			if err != nil {
				t.Fatal(err)
			}
			if syncStatus.Synced {
				break
			}
			if i == 5 {
				t.Fatal("spv wallet not synced after 5 seconds")
			}
			i++
			time.Sleep(time.Second)
		}

		spv := backend.(*ExchangeWalletSPV)
		fullNode := &ExchangeWalletFullNode{
			intermediaryWallet: spv.intermediaryWallet,
			authAddOn:          spv.authAddOn,
		}

		return fullNode, cm
	}

	accelerator := backend.(*ExchangeWalletAccelerator)
	return accelerator.ExchangeWalletFullNode, cm
}

type testRig struct {
	backends          map[string]*ExchangeWalletFullNode
	connectionMasters map[string]*util.ConnectionMaster
}

func newTestRig(t *testing.T, blkFunc func(string, error)) *testRig {
	t.Helper()
	rig := &testRig{
		backends:          make(map[string]*ExchangeWalletFullNode),
		connectionMasters: make(map[string]*util.ConnectionMaster, 3),
	}
	rig.backends["alpha"], rig.connectionMasters["alpha"] = tBackend(t, "alpha", false, blkFunc)
	rig.backends["beta"], rig.connectionMasters["beta"] = tBackend(t, "beta", false, blkFunc)
	rig.backends["gamma"], rig.connectionMasters["gamma"] = tBackend(t, "gamma", true, blkFunc)

	gammaAddr, err := rig.backends["gamma"].DepositAddress()
	if err != nil {
		t.Fatalf("error getting gamma deposit address: %v", err)
	}

	_, err = rig.alpha().Send(gammaAddr, toSatoshi(100), 10)
	if err != nil {
		t.Fatalf("error sending to gamma: %v", err)
	}

	mineAlpha()

	return rig
}

func (rig *testRig) alpha() *ExchangeWalletFullNode {
	return rig.backends["alpha"]
}
func (rig *testRig) beta() *ExchangeWalletFullNode {
	return rig.backends["beta"]
}
func (rig *testRig) gamma() *ExchangeWalletFullNode {
	return rig.backends["gamma"]
}
func (rig *testRig) close(t *testing.T) {
	t.Helper()
	for name, cm := range rig.connectionMasters {
		closed := make(chan struct{})
		go func() {
			cm.Disconnect()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.NewTimer(60 * time.Second).C:
			t.Fatalf("failed to disconnect from %s", name)
		}
	}
}

func randBytes(l int) []byte {
	b := make([]byte, l)
	rand.Read(b)
	return b
}

func waitNetwork() {
	time.Sleep(time.Second * 3 / 2)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	feeRate, err := externalFeeRate(ctx, net)
	if err != nil {
		t.Fatalf("error fetching %s fees: %v", net, err)
	}
	fmt.Printf("##### Fee rate fetched for %s! %d Sats/vB \n", net, feeRate)
}
