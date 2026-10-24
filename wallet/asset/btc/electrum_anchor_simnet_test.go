//go:build harness

package btc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/rpcclient/v8"
)

// harnessCheckpoint builds a checkpoint record for the harness node's current
// tip from its RPC: the hash is what getblockhash reports (the chain's own
// block identity, X11 on Dash) and the header comes from getblockheader.
func harnessCheckpoint(t *testing.T, cl *rpcclient.Client) netparams.Checkpoint {
	t.Helper()
	var height int64
	res, err := cl.RawRequest(tCtx, "getblockcount", nil)
	if err != nil {
		t.Fatalf("getblockcount: %v", err)
	}
	if err := json.Unmarshal(res, &height); err != nil {
		t.Fatalf("getblockcount result: %v", err)
	}
	heightJSON, _ := json.Marshal(height)
	res, err = cl.RawRequest(tCtx, "getblockhash", []json.RawMessage{heightJSON})
	if err != nil {
		t.Fatalf("getblockhash %d: %v", height, err)
	}
	var hashStr string
	if err := json.Unmarshal(res, &hashStr); err != nil {
		t.Fatalf("getblockhash result: %v", err)
	}
	hash, err := chainhash.NewHashFromStr(hashStr)
	if err != nil {
		t.Fatalf("block hash %q: %v", hashStr, err)
	}
	res, err = cl.RawRequest(tCtx, "getblockheader", []json.RawMessage{res, json.RawMessage("false")})
	if err != nil {
		t.Fatalf("getblockheader %s: %v", hashStr, err)
	}
	var hdrHex string
	if err := json.Unmarshal(res, &hdrHex); err != nil {
		t.Fatalf("getblockheader result: %v", err)
	}
	hdrBytes, err := hex.DecodeString(hdrHex)
	if err != nil {
		t.Fatalf("header hex: %v", err)
	}
	var hdr wire.BlockHeader
	if err := hdr.Deserialize(bytes.NewReader(hdrBytes)); err != nil {
		t.Fatalf("header deserialize: %v", err)
	}
	return netparams.Checkpoint{Height: int32(height), Hash: hash, Header: &hdr}
}

// setSimnetCheckpoints installs cps as the simnet checkpoint list for the
// test's duration. The electrum backend reads the list when a wallet opens, so
// no wallet on these params may be connected when it changes.
func setSimnetCheckpoints(t *testing.T, params *netparams.ChainParams, cps ...netparams.Checkpoint) {
	t.Helper()
	prev := params.Checkpoints
	params.Checkpoints = cps
	t.Cleanup(func() { params.Checkpoints = prev })
}

// TestElectrumAnchoredRestore restores a seed on a header store anchored at a
// checkpoint above the seed's earlier activity. Wallet A receives funds; the
// chain advances; the tip becomes the simnet checkpoint. Wallet B, the same
// seed with a recovery window and a birthday older than every simnet header
// after the stored margin, must start its header store at the checkpoint,
// resolve its birthday block to the checkpoint, sync, show none of the
// pre-anchor funds, and detect funds received above the anchor. A restart on
// the same data directory keeps the store and the balance.
func TestElectrumAnchoredRestore(t *testing.T) {
	alpha, alphaCM := tBackend(t, "alpha", false, nil)
	defer alphaCM.Disconnect()
	if err := alpha.Unlock(walletPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}
	seed := randBytes(32)

	// Wallet A: funds received below the future anchor.
	wA, cmA := connectElectrumWalletAt(t, t.TempDir(), seed, 0)
	waitElectrumSynced(t, wA)
	addrA, err := wA.DepositAddress()
	if err != nil {
		t.Fatalf("A DepositAddress: %v", err)
	}
	coinA, err := alpha.Send(addrA, toSatoshi(1), 10)
	if err != nil {
		t.Fatalf("alpha.Send to A: %v", err)
	}
	if err := mineAlpha(); err != nil {
		t.Fatalf("mineAlpha: %v", err)
	}
	// The receive must be confirmed before it is buried, or it could land
	// in the anchor block, whose transactions the anchored store does see.
	waitElectrumTxConfirmed(t, wA, coinA.TxID())
	cmA.Disconnect()

	// Bury A's receive, then anchor at the tip.
	for range 10 {
		if err := mineAlpha(); err != nil {
			t.Fatalf("mineAlpha: %v", err)
		}
	}
	cp := harnessCheckpoint(t, alpha.cl)
	setSimnetCheckpoints(t, assets.BTCParams["simnet"], cp)
	t.Logf("anchor checkpoint %d %s", cp.Height, cp.Hash)

	// Wallet B: same seed, fresh data directory, recovery window. The
	// birthday is an hour ago; with the stored 48-hour margin it predates
	// every simnet header, so the birthday search must bottom out at the
	// anchor rather than probe below it.
	dataDir := t.TempDir()
	walletDir := filepath.Join(dataDir, chaincfg.RegressionNetParams.Name)
	bday := time.Now().Add(-time.Hour)
	if err := createElectrumWallet(walletPassword, seed, bday, walletDir, tLogger, 0, 0, &chaincfg.RegressionNetParams); err != nil {
		t.Fatalf("createElectrumWallet: %v", err)
	}
	masterDB, err := txdb.New(filepath.Join(dataDir, "txdb"), tLogger.SubLogger("TXDB"))
	if err != nil {
		t.Fatalf("txdb.New: %v", err)
	}
	if _, err = masterDB.Connect(tCtx); err != nil {
		t.Fatalf("txdb.Connect: %v", err)
	}
	t.Cleanup(func() { masterDB.Close() })
	open := func(label string) (*NativeWallet, *util.ConnectionMaster) {
		t.Helper()
		cfg := &asset.WalletConfig{
			Type:        walletTypeElectrum,
			DataDir:     dataDir,
			Settings:    map[string]string{"recoverywindow": "20"},
			Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), 0, tLogger),
			NetworkTxDB: masterDB.NetworkTxDB(BipID),
			PeersChange: func(n uint32, err error) {},
		}
		w := NewNativeWallet(btcElectrumChainConfig(&chaincfg.RegressionNetParams), cfg, tLogger)
		cm := util.NewConnectionMaster(w)
		if err := cm.Connect(tCtx); err != nil {
			t.Fatalf("%s Connect: %v", label, err)
		}
		waitElectrumSynced(t, w)
		start := w.btcw.ChainClient().(interface{ EarliestBlockHeight() int32 }).EarliestBlockHeight()
		if start != cp.Height {
			t.Fatalf("%s: EarliestBlockHeight = %d, want anchor %d", label, start, cp.Height)
		}
		return w, cm
	}

	wB, cmB := open("restore")
	var birthdayBlock waddrmgr.BlockStamp
	err = walletdb.View(wB.btcw.Database(), func(tx walletdb.ReadTx) error {
		bs, _, err := wB.btcw.Manager.BirthdayBlock(tx.ReadBucket([]byte("waddrmgr")))
		birthdayBlock = bs
		return err
	})
	if err != nil {
		t.Fatalf("BirthdayBlock: %v", err)
	}
	if birthdayBlock.Height != cp.Height || birthdayBlock.Hash != *cp.Hash {
		t.Fatalf("birthday block = %d %s, want the anchor %d %s",
			birthdayBlock.Height, birthdayBlock.Hash, cp.Height, cp.Hash)
	}
	// Pre-anchor funds are outside the store; give the recovery scan time to
	// finish, then confirm it found nothing.
	time.Sleep(3 * time.Second)
	bal, err := wB.Balance()
	if err != nil {
		t.Fatalf("B Balance: %v", err)
	}
	if bal.Available != 0 || bal.Immature != 0 {
		t.Fatalf("B sees pre-anchor funds: %+v", bal)
	}

	// Positive control on the scan: recovery saw addrA's pre-anchor-only
	// history and marked it used, so B's next deposit address is past it.
	addrB, err := wB.DepositAddress()
	if err != nil {
		t.Fatalf("B DepositAddress: %v", err)
	}
	if addrB == addrA {
		t.Fatalf("recovery did not discover %s from its pre-anchor history", addrA)
	}

	// Funds above the anchor are detected and confirm.
	if _, err := alpha.Send(addrB, toSatoshi(2), 10); err != nil {
		t.Fatalf("alpha.Send to B: %v", err)
	}
	if err := mineAlpha(); err != nil {
		t.Fatalf("mineAlpha: %v", err)
	}
	waitElectrumBalanceAtLeast(t, wB, toSatoshi(2))
	_, tipBefore, err := wB.btcw.ChainClient().GetBestBlock()
	if err != nil {
		t.Fatalf("GetBestBlock: %v", err)
	}
	cmB.Disconnect()

	// Restart on the same data directory: the stored headers match the
	// anchor, so nothing is discarded, and the balance is already known.
	wB2, cmB2 := open("restart")
	defer cmB2.Disconnect()
	_, tipAfter, err := wB2.btcw.ChainClient().GetBestBlock()
	if err != nil {
		t.Fatalf("GetBestBlock after restart: %v", err)
	}
	if tipAfter < tipBefore {
		t.Fatalf("header tip after restart %d < %d before", tipAfter, tipBefore)
	}
	bal, err = wB2.Balance()
	if err != nil {
		t.Fatalf("B Balance after restart: %v", err)
	}
	if bal.Available < toSatoshi(2) {
		t.Fatalf("B balance after restart = %+v, want >= %d", bal, toSatoshi(2))
	}
	if err := wB2.Unlock(walletPassword); err != nil {
		t.Fatalf("B Unlock: %v", err)
	}
	coin, err := wB2.Send(addrA, toSatoshi(0.5), 10)
	if err != nil {
		t.Fatalf("B Send after restart: %v", err)
	}
	if err := mineAlpha(); err != nil {
		t.Fatalf("mineAlpha: %v", err)
	}
	waitElectrumTxConfirmed(t, wB2, coin.TxID())
}
