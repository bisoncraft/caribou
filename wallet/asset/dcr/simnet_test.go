//go:build harness

package dcr

// Simnet tests expect the DCR test harness to be running.
//
// Sim harness info:
// The harness has two wallets, alpha and beta
// Both wallets have confirmed UTXOs.
// The alpha wallet has large coinbase outputs and smaller change outputs.
// The beta wallet has mature vote outputs and regular transaction outputs of
// varying size and confirmation count. Value:Confirmations =
// 10:8, 18:7, 5:6, 7:5, 1:4, 15:3, 3:2, 25:1

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"decred.org/dcrwallet/v5/wallet"
	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/wire"
)

const (
	alphaAddress = "SsWKp7wtdTZYabYFYSc9cnxhwFEjA5g4pFc"
	betaAddress  = "Ssge52jCzbixgFC736RSTrwAnvH3a4hcPRX"
	gammaSeed    = "1285a47d6a59f9c548b2a72c2c34a2de97967bede3844090102bbba76707fe9d"
	vspAddr      = "http://127.0.0.1:19591"
)

var (
	tLogger   util.Logger
	tCtx      context.Context
	tLotSize  uint64 = 1e7
	tRateStep uint64 = 100
	tDCR             = &util.Asset{
		ID:         42,
		Symbol:     "dcr",
		MaxFeeRate: 10,
		SwapConf:   1,
	}
	walletPassword = []byte("abc")
)

func mineAlpha() error {
	return exec.Command("tmux", "send-keys", "-t", "dcr-harness:0", "./mine-alpha 1", "C-m").Run()
}

func tBackend(t *testing.T, name string, isInternal bool, blkFunc func(string)) (*ExchangeWallet, *util.ConnectionMaster) {
	t.Helper()
	user, err := user.Current()
	if err != nil {
		t.Fatalf("error getting current user: %v", err)
	}
	settings := make(map[string]string)
	if !isInternal {
		cfgPath := filepath.Join(user.HomeDir, "dextest", "dcr", name, name+".conf")
		var err error
		settings, err = config.Parse(cfgPath)
		if err != nil {
			t.Fatalf("error reading config options: %v", err)
		}
	}
	notes := make(chan asset.WalletNotification, 1)
	walletCfg := &asset.WalletConfig{
		Settings: settings,
		Emit:     asset.NewWalletEmitter(notes, BipID, tLogger),
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
		createSPVWallet(walletPassword, seed, dataDir, 0, 0, wallet.DefaultGapLimit, chaincfg.SimNetParams())
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

	go func() {
		for {
			select {
			case ni := <-notes:
				switch ni.(type) {
				case *asset.TipChangeNote:
					blkFunc(name)
				}
			case <-tCtx.Done():
				return
			}
		}
	}()

	if isInternal {
		i := 0
		for {
			ss, err := backend.SyncStatus()
			if err != nil {
				t.Fatal(err)
			}
			if ss.Synced {
				break
			}
			if i == 5 {
				t.Fatal("spv wallet not synced after 5 seconds")
			}
			i++
			time.Sleep(time.Second)
		}
	}

	if nativeWallet, is := backend.(*NativeWallet); is {
		return nativeWallet.ExchangeWallet, cm
	}

	return backend.(*ExchangeWallet), cm
}

type testRig struct {
	backends          map[string]*ExchangeWallet
	connectionMasters map[string]*util.ConnectionMaster
}

func newTestRig(t *testing.T, blkFunc func(string)) *testRig {
	t.Helper()
	rig := &testRig{
		backends:          make(map[string]*ExchangeWallet),
		connectionMasters: make(map[string]*util.ConnectionMaster, 3),
	}
	rig.backends["alpha"], rig.connectionMasters["alpha"] = tBackend(t, "alpha", false, blkFunc)
	rig.backends["beta"], rig.connectionMasters["beta"] = tBackend(t, "beta", false, blkFunc)
	rig.backends["gamma"], rig.connectionMasters["gamma"] = tBackend(t, "gamma", true, blkFunc)
	return rig
}

// alpha is an external wallet connected to dcrd.
func (rig *testRig) alpha() *ExchangeWallet {
	return rig.backends["alpha"]
}

// beta is an external spv wallet.
func (rig *testRig) beta() *ExchangeWallet {
	return rig.backends["beta"]
}

// gamma is an internal spv wallet.
func (rig *testRig) gamma() *ExchangeWallet {
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
		case <-time.NewTimer(time.Second).C:
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
	var shutdown func()
	tCtx, shutdown = context.WithCancel(context.Background())
	doIt := func() int {
		defer shutdown()
		return m.Run()
	}
	os.Exit(doIt())
}

func TestTickets(t *testing.T) {
	rig := newTestRig(t, func(name string) {
		tLogger.Infof("%s has reported a new block", name)
	})
	defer rig.close(t)
	tLogger.Info("Testing ticket methods with the alpha rig.")
	testTickets(t, false, rig.alpha())
	// TODO: beta wallet is spv but has no vsp in its config. Add it and
	// test here.
	tLogger.Info("Testing ticket methods with the gamma rig.")
	testTickets(t, true, rig.gamma())
}

func testTickets(t *testing.T, isInternal bool, ew *ExchangeWallet) {

	// Test StakeStatus.
	ss, err := ew.StakeStatus()
	if err != nil {
		t.Fatalf("unable to get stake status: %v", err)
	}
	tLogger.Info("The following are stake status before setting vsp or purchasing tickets.")
	spew.Dump(ss)

	// Test SetVSP.
	err = ew.SetVSP(vspAddr)
	if isInternal {
		if err != nil {
			t.Fatalf("unexpected error setting vsp for internal wallet: %v", err)
		}
	} else {
		if err == nil {
			t.Fatal("expected error setting vsp for external wallet")
		}
	}

	// Test PurchaseTickets.
	if err := ew.Unlock(walletPassword); err != nil {
		t.Fatalf("unable to unlock wallet: %v", err)
	}

	if err := ew.PurchaseTickets(3, 20); err != nil {
		t.Fatalf("error purchasing tickets: %v", err)
	}

	var currentDeployments []chaincfg.ConsensusDeployment
	var bestVer uint32
	for ver, deps := range ew.chainParams.Deployments {
		if bestVer == 0 || ver > bestVer {
			currentDeployments = deps
			bestVer = ver
		}
	}

	choices := make(map[string]string)
	for _, d := range currentDeployments {
		choices[d.Vote.Id] = d.Vote.Choices[rand.Int()%len(d.Vote.Choices)].Id
	}

	aPubKey := "034a43df1b95bf1b0dd77b53b8d880d4a5f47376fb036a5be5c1f3ba8d12ef65d7"
	bPubKey := "02028dd2bbabbcd262c3e2326ae27a61fcc935beb2bbc2f0f76f3f5025ba4a3c5d"

	treasuryPolicy := map[string]string{
		aPubKey: "yes",
		bPubKey: "no",
	}

	var tspendPolicy map[string]string
	if spvw, is := ew.wallet.(*spvWallet); is {
		if dcrw, is := spvw.dcrWallet.(*extendedWallet); is {
			txIn := &wire.TxIn{SignatureScript: encode.RandomBytes(66 + secp256k1.PubKeyBytesLenCompressed)}
			tspendA := &wire.MsgTx{Expiry: math.MaxUint32 - 1, TxIn: []*wire.TxIn{txIn}}
			tspendB := &wire.MsgTx{Expiry: math.MaxUint32 - 2, TxIn: []*wire.TxIn{txIn}}
			txHashA, txHashB := tspendA.TxHash(), tspendB.TxHash()
			tspendPolicy = map[string]string{
				txHashA.String(): "yes",
				txHashB.String(): "no",
			}
			for _, tx := range []*wire.MsgTx{tspendA, tspendB} {
				if err := dcrw.AddTSpend(*tx); err != nil {
					t.Fatalf("Error adding tspend: %v", err)
				}
			}
		}
	}

	// Test SetVotingPreferences.
	if err := ew.SetVotingPreferences(choices, tspendPolicy, treasuryPolicy); err != nil {
		t.Fatalf("Error setting voting preferences: %v", err)
	}

	// Test StakeStatus again.
	ss, err = ew.StakeStatus()
	if err != nil {
		t.Fatalf("Unable to get stake status: %v", err)
	}
	tLogger.Info("The following are stake status after setting vsp and purchasing tickets.")
	spew.Dump(ss)

	if len(ss.Stances.Agendas) != len(choices) {
		t.Fatalf("wrong number of vote choices. expected %d, got %d", len(choices), len(ss.Stances.Agendas))
	}

	for _, agenda := range ss.Stances.Agendas {
		choiceID, found := choices[agenda.ID]
		if !found {
			t.Fatalf("unknown agenda %s", agenda.ID)
		}
		if agenda.CurrentChoice != choiceID {
			t.Fatalf("wrong choice reported. expected %s, got %s", choiceID, agenda.CurrentChoice)
		}
	}

	if len(ss.Stances.TreasuryKeys) != len(treasuryPolicy) {
		t.Fatalf("wrong number of treasury keys. expected %d, got %d", len(treasuryPolicy), len(ss.Stances.TreasuryKeys))
	}

	for _, tp := range ss.Stances.TreasuryKeys {
		policy, found := treasuryPolicy[tp.Key]
		if !found {
			t.Fatalf("unknown treasury key %s", tp.Key)
		}
		if tp.Policy != policy {
			t.Fatalf("wrong policy reported. expected %s, got %s", policy, tp.Policy)
		}
	}

	if len(ss.Stances.TreasurySpends) != len(tspendPolicy) {
		t.Fatalf("wrong number of tspends. expected %d, got %d", len(tspendPolicy), len(ss.Stances.TreasurySpends))
	}

	for _, tspend := range ss.Stances.TreasurySpends {
		policy, found := tspendPolicy[tspend.Hash]
		if !found {
			t.Fatalf("unknown tspend tx %s", tspend.Hash)
		}
		if tspend.CurrentPolicy != policy {
			t.Fatalf("wrong policy reported. expected %s, got %s", policy, tspend.CurrentPolicy)
		}
	}
}

func TestExternalFeeRate(t *testing.T) {
	fetchRateWithTimeout(t, util.Mainnet)
	fetchRateWithTimeout(t, util.Testnet)
}

func fetchRateWithTimeout(t *testing.T, net util.Network) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	feeRate, err := fetchFeeFromOracle(ctx, net, 2)
	if err != nil {
		t.Fatalf("error fetching %s fees: %v", net, err)
	}
	fmt.Printf("##### Fee rate fetched for %s! %.8f DCR/kB \n", net, feeRate)
}
