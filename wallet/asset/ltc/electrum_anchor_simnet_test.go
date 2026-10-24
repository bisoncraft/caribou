//go:build harness

package ltc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
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
	res, err := cl.RawRequest(ltcCtx, "getblockcount", nil)
	if err != nil {
		t.Fatalf("getblockcount: %v", err)
	}
	if err := json.Unmarshal(res, &height); err != nil {
		t.Fatalf("getblockcount result: %v", err)
	}
	heightJSON, _ := json.Marshal(height)
	res, err = cl.RawRequest(ltcCtx, "getblockhash", []json.RawMessage{heightJSON})
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
	res, err = cl.RawRequest(ltcCtx, "getblockheader", []json.RawMessage{res, json.RawMessage("false")})
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

// TestLTCElectrumAnchoredRestore restores a seed on a header store anchored
// at a checkpoint above the seed's earlier activity. Wallet A receives funds;
// the chain advances; the tip becomes the simnet checkpoint. Wallet B, the
// same seed with a recovery window and a birthday older than every simnet
// header after the stored margin, must sync from the checkpoint, show none of
// the pre-anchor funds, and detect funds received above the anchor. The BTC
// test of the same name also checks the store's start, the birthday block and
// a restart; this one covers the chain-specific electrumx and header path.
func TestLTCElectrumAnchoredRestore(t *testing.T) {
	alpha, alphaCM := newLTCAlphaBackend(t)
	defer alphaCM.Disconnect()
	if err := alpha.Unlock(ltcPassword); err != nil {
		t.Fatalf("unlock alpha: %v", err)
	}
	seed := randLTCBytes(t, 32)

	// Wallet A: funds received below the future anchor.
	wA, cmA := connectLTCElectrumWalletAt(t, t.TempDir(), seed, 0)
	waitLTCElectrumSynced(t, wA)
	addrA, err := wA.DepositAddress()
	if err != nil {
		t.Fatalf("A DepositAddress: %v", err)
	}
	coinA, err := alpha.Send(addrA, toSatoshiLTC(1), 0)
	if err != nil {
		t.Fatalf("alpha.Send to A: %v", err)
	}
	if err := mineLTCAlpha(); err != nil {
		t.Fatalf("mine: %v", err)
	}
	// The receive must be confirmed before it is buried, or it could land
	// in the anchor block, whose transactions the anchored store does see.
	waitLTCTxConfirmed(t, wA, coinA.TxID())
	cmA.Disconnect()

	// Bury A's receive, then anchor at the tip.
	for range 10 {
		if err := mineLTCAlpha(); err != nil {
			t.Fatalf("mine: %v", err)
		}
	}
	cp := harnessCheckpoint(t, alpha.cl)
	setSimnetCheckpoints(t, assets.LTCParams["simnet"], cp)
	t.Logf("anchor checkpoint %d %s", cp.Height, cp.Hash)

	// Wallet B: same seed, fresh data directory, recovery window. Its
	// birthday is an hour ago; with the stored 48-hour margin it predates
	// every simnet header, so the birthday search bottoms out at the anchor.
	wB, cmB := connectLTCElectrumWalletAt(t, t.TempDir(), seed, 20)
	defer cmB.Disconnect()
	waitLTCElectrumSynced(t, wB)
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
	if _, err := alpha.Send(addrB, toSatoshiLTC(2), 0); err != nil {
		t.Fatalf("alpha.Send to B: %v", err)
	}
	if err := mineLTCAlpha(); err != nil {
		t.Fatalf("mine: %v", err)
	}
	waitLTCBalanceAtLeast(t, wB, toSatoshiLTC(2))
}
