//go:build harness

package ltc

// Regnet tests expect the LTC test harness to be running. The harness miner
// must be OFF.
//
// Sim harness info:
// The harness has three wallets, alpha, beta, and gamma.
// All three wallets have confirmed UTXOs.
// The beta wallet has only coinbase outputs.
// The alpha wallet has coinbase outputs too, but has sent some to the gamma
//   wallet, so also has some change outputs.
// The gamma wallet has regular transaction outputs of varying size and
// confirmation count. Value:Confirmations =
// 10:8, 18:7, 5:6, 7:5, 1:4, 15:3, 3:2, 25:1

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/btc/livetest"
)

// const alphaAddress = "rltc1qjld4f85m96rr77035c5yuhkz8apxlkkla0ftmz"

var (
	tLotSize uint64 = 1e6
	tLTC            = &util.Asset{
		ID:         2,
		Symbol:     "ltc",
		Version:    version,
		MaxFeeRate: 10,
		SwapConf:   1,
	}

	ltcUsr, _        = user.Current()
	ltcHarnessCtlDir = filepath.Join(ltcUsr.HomeDir, "dextest", "ltc", "harness-ctl")
	ltcPW            = []byte("abc")
)

func TestWallet(t *testing.T) {
	livetest.Run(t, &livetest.Config{
		NewWallet: NewWallet,
		LotSize:   tLotSize,
		Asset:     tLTC,
	})
}

func TestSPVWallet(t *testing.T) {
	const lotSize = 1e6

	spvDir := t.TempDir()

	createLTCWallet := func(cfg *asset.WalletConfig, name string, logger util.Logger) error {
		seed := encode.RandomBytes(32)

		err := (&Driver{}).Create(&asset.CreateWalletParams{
			Type:    walletTypeSPV,
			Seed:    seed,
			Pass:    ltcPW, // matches walletPassword in livetest.go -> Run
			DataDir: cfg.DataDir,
			Net:     util.Simnet,
			Logger:  logger,
		})
		if err != nil {
			return fmt.Errorf("error creating LTC SPV wallet: %w", err)
		}

		w, err := NewWallet(cfg, logger, util.Regtest)
		if err != nil {
			t.Fatalf("error opening LTC SPV wallet: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cm := util.NewConnectionMaster(w)
		if err = cm.Connect(ctx); err != nil {
			t.Fatalf("error connecting LTC SPV wallet: %v", err)
		}
		defer cm.Disconnect()

		addr, err := w.DepositAddress()
		if err != nil {
			return fmt.Errorf("DepositAddress error: %w", err)
		}

		if err := ltcLoadAddress(addr); err != nil {
			return fmt.Errorf("ltcLoadAddress error: %w", err)
		}

		time.Sleep(time.Second * 3)

		// Bound the sync poll. Previously the loop ran indefinitely; if the
		// harness never reaches Synced, the whole test would hang silently
		// until the outer test timeout. 2 minutes is well above normal
		// regnet sync time but short enough to surface real breakage.
		syncDeadline := time.Now().Add(2 * time.Minute)
		for {
			ss, err := w.SyncStatus()
			if err != nil {
				return fmt.Errorf("SyncStatus error: %w", err)
			}
			if ss.Synced {
				break
			}
			if time.Now().After(syncDeadline) {
				return fmt.Errorf("%s LTC SPV did not sync within deadline (progress %.1f%%)", name, ss.BlockProgress()*100)
			}
			fmt.Printf("%s LTC SPV sync progress %.1f%%\n", name, ss.BlockProgress()*100)
			time.Sleep(time.Second)
		}

		bal, _ := w.Balance()
		logger.Infof("%s with address %s is synced, balance = %+v", name, addr, bal)

		return nil
	}

	spvConstructor := func(cfg *asset.WalletConfig, logger util.Logger, network util.Network) (asset.Wallet, error) {
		token := hex.EncodeToString(encode.RandomBytes(4))
		cfg.Type = walletTypeSPV
		cfg.DataDir = filepath.Join(spvDir, token)

		name := ltcParseName(cfg.Settings)

		// The LTC regtest alpha node listens on 20585; the SPV wallet's
		// default peer list already includes that address, so no override
		// is required unless we need the beta node specifically.
		if name == "beta" {
			cfg.Settings["peer"] = "localhost:20586" // beta node listen port
		}

		if err := createLTCWallet(cfg, name, logger); err != nil {
			return nil, fmt.Errorf("createLTCWallet error: %w", err)
		}

		return NewWallet(cfg, logger, network)
	}

	fmt.Println("////////// LTC SPV WALLET W/O SPLIT //////////")
	livetest.Run(t, &livetest.Config{
		NewWallet: spvConstructor,
		LotSize:   lotSize,
		Asset:     tLTC,
		SPV:       true,
	})

	fmt.Println("////////// LTC SPV WALLET WITH SPLIT //////////")
	livetest.Run(t, &livetest.Config{
		NewWallet: spvConstructor,
		LotSize:   lotSize,
		Asset:     tLTC,
		SPV:       true,
		SplitTx:   true,
	})
}

// ltcLoadAddress funds addr from the LTC harness alpha wallet, sending the
// standard set of outputs used by livetest.Run, then mines a block.
func ltcLoadAddress(addr string) error {
	for _, v := range []string{"10", "18", "5", "7", "1", "15", "3", "25"} {
		if err := ltcRunCmd("./alpha", "sendtoaddress", addr, v); err != nil {
			return err
		}
	}
	return ltcRunCmd("./mine-alpha", "1")
}

func ltcRunCmd(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.Dir = ltcHarnessCtlDir
	return cmd.Run()
}

// ltcParseName derives a wallet name from its settings map, mirroring
// the parseName helper in the BTC livetest package.
func ltcParseName(settings map[string]string) string {
	name := settings["walletname"]
	if name == "" {
		name = filepath.Base(settings["datadir"])
	}
	return name
}
