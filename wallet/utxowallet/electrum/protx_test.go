// Copyright (c) 2026 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
)

// The fixtures in testdata are what a Dash Core v24 regtest node and the
// ElectrumX in front of it returned, captured unedited, so these tests run
// against the real shapes and real bytes.

func readFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

type specialTx struct{ Name, Txid, Hex string }

func specialTxs(t *testing.T) []specialTx {
	t.Helper()
	var txs []specialTx
	readFixture(t, "dash_special_txs.json", &txs)
	if len(txs) != 3 {
		t.Fatalf("%d fixtures", len(txs))
	}
	return txs
}

// TestVerifyTxidDashSpecial is the check GetRawTransaction makes of a Dash
// special transaction, whose txid covers the payload that follows its lock time:
// a masternode registration, a coinbase that carries the quorum commitment and a
// dissolution. Decoding them as a plain transaction leaves the payload unread, so
// it was refused as trailing bytes.
func TestVerifyTxidDashSpecial(t *testing.T) {
	for _, tx := range specialTxs(t) {
		t.Run(tx.Name, func(t *testing.T) {
			if err := verifyTxidMatchesHex(bisonwire.ChainDASH, tx.Txid, tx.Hex); err != nil {
				t.Fatalf("a real %s was refused: %v", tx.Name, err)
			}
			// Read as a Bitcoin transaction it is the bug this guards against.
			if err := verifyTxidMatchesHex(bisonwire.ChainBTC, tx.Txid, tx.Hex); err == nil {
				t.Fatal("expected the plain decoding to refuse a special transaction")
			}

			raw, _ := hex.DecodeString(tx.Hex)
			// A changed payload byte, which only the payload hash notices.
			tampered := bytes.Clone(raw)
			tampered[len(tampered)-5] ^= 1
			err := verifyTxidMatchesHex(bisonwire.ChainDASH, tx.Txid, hex.EncodeToString(tampered))
			if err == nil || !strings.Contains(err.Error(), "hashes to") {
				t.Fatalf("a changed payload was accepted: %v", err)
			}
			// Appended bytes.
			err = verifyTxidMatchesHex(bisonwire.ChainDASH, tx.Txid, tx.Hex+"00")
			if err == nil || !strings.Contains(err.Error(), "trailing") {
				t.Fatalf("appended bytes were accepted: %v", err)
			}
			// Truncated.
			if err := verifyTxidMatchesHex(bisonwire.ChainDASH, tx.Txid, tx.Hex[:len(tx.Hex)-2]); err == nil {
				t.Fatal("a truncated transaction was accepted")
			}
			// Another txid.
			other := []byte(tx.Txid)
			other[0] ^= 1 // still hex: '0'-'9a-f' flip of the lowest bit
			if err := verifyTxidMatchesHex(bisonwire.ChainDASH, string(other), tx.Hex); err == nil {
				t.Fatal("bytes were accepted for another txid")
			}
		})
	}
}

// A plain Dash transaction is checked as before.
func TestVerifyTxidDashPlain(t *testing.T) {
	msg := wire.NewMsgTx(2)
	msg.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 1}})
	msg.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})
	tx := &bisonwire.Tx{Chain: "dash", MsgTx: *msg}
	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	if err := verifyTxidMatchesHex(bisonwire.ChainDASH, tx.TxHash().String(), hex.EncodeToString(b.Bytes())); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyTxidDashBytes requires the bytes the server sent, not their decoded
// meaning, to hash to the txid. bisonwire accepts encodings that dashd would not
// (a non-minimal compact size, a version with its sign bit set that is read as 16
// bits) and writes them back canonically, so hashing the decoded transaction
// would accept other bytes for a real txid.
func TestVerifyTxidDashBytes(t *testing.T) {
	msg := wire.NewMsgTx(2)
	msg.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 1}})
	msg.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})
	tx := &bisonwire.Tx{Chain: "dash", MsgTx: *msg}
	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	honest := b.Bytes()
	txid := tx.TxHash().String()
	if err := verifyTxidMatchesHex(bisonwire.ChainDASH, txid, hex.EncodeToString(honest)); err != nil {
		t.Fatal(err)
	}

	// The input count 01 written as fd 01 00.
	nonMinimal := append(append(append([]byte{}, honest[:4]...), 0xfd, 0x01, 0x00), honest[5:]...)
	// A version whose low 16 bits have the top bit set, 01 80 00 00 for the
	// honest 01 80 ff ff.
	signVersion := &bisonwire.Tx{Chain: "dash", MsgTx: *wire.NewMsgTx(-32767)}
	signVersion.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 1}})
	signVersion.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})
	var sv bytes.Buffer
	if err := signVersion.Serialize(&sv); err != nil {
		t.Fatal(err)
	}
	svHonest := sv.Bytes()
	svOther := append([]byte{svHonest[0], svHonest[1], 0, 0}, svHonest[4:]...)

	for _, tt := range []struct {
		name, txid string
		sent       []byte
	}{
		{"non-minimal compact size", txid, nonMinimal},
		{"version read as 16 bits", signVersion.TxHash().String(), svOther},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The point of the test is that the shape check passes.
			probe := &bisonwire.Tx{Chain: "dash"}
			if err := probe.Deserialize(bytes.NewReader(tt.sent)); err != nil {
				t.Fatalf("bisonwire no longer reads these bytes, so this test proves nothing: %v", err)
			}
			if err := verifyTxidMatchesHex(bisonwire.ChainDASH, tt.txid, hex.EncodeToString(tt.sent)); err == nil {
				t.Fatal("other bytes were accepted for the txid")
			}
		})
	}
}

func protxFixture(t *testing.T, name string) (chainhash.Hash, json.RawMessage) {
	t.Helper()
	var raw json.RawMessage
	readFixture(t, name, &raw)
	var info struct {
		ProTxHash string `json:"proTxHash"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	h, err := chainhash.NewHashFromStr(info.ProTxHash)
	if err != nil {
		t.Fatal(err)
	}
	return *h, raw
}

func dialFake(t *testing.T, f *fakeServer, params interface{ String() string }) *Node {
	t.Helper()
	node, err := NewNode(t.Context(), f.addr(), "", params.String(), bisonwire.ChainDASH)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	t.Cleanup(node.Close)
	return node
}

func TestProTxInfo(t *testing.T) {
	f, params := newFakeServer(t, 3)
	hash, raw := protxFixture(t, "protx_info_shared.json")
	f.mtx.Lock()
	f.protx[hash.String()] = raw
	f.mtx.Unlock()
	node := dialFake(t, f, params.GenesisHash)

	got, err := node.ProTxInfo(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProTxHash != hash.String() || got.CollateralHash != hash.String() || got.CollateralIndex != 2 || got.Type != "Regular" {
		t.Errorf("identity: %+v", got)
	}
	st := got.State
	if st.Version != 3 || st.RegisteredHeight != 798 || st.LastPaidHeight != 910 || st.PoSeBanHeight != -1 ||
		st.PoSeRevivedHeight != -1 || st.PoSePenalty != 0 || st.RevocationReason != 0 {
		t.Errorf("state: %+v", st)
	}
	if st.EarlyPeriodBlocks != 10 || st.EarlyPenalty != 500000000 {
		t.Errorf("early terms: %d blocks, %d", st.EarlyPeriodBlocks, st.EarlyPenalty)
	}
	if st.VotingAddress != "yQnqsAF4zdgdKFvFbvNAXB3TKTV8wGqRVJ" ||
		st.PubKeyOperator != "83a1f70702ebc322b7f4943b85fcd6f5e94bbbb3fdd047e86c62436360b98f6e6efa07f2bd736f4659dd9186bac15b3e" {
		t.Errorf("keys: %q %q", st.VotingAddress, st.PubKeyOperator)
	}
	if len(st.Shares) != 2 {
		t.Fatalf("%d shares", len(st.Shares))
	}
	if sh := st.Shares[1]; sh.Amount != 50000000000 ||
		sh.RefundScript != "76a9145131da915d6ac2cddd6684b663552673b8f6770e88ac" ||
		sh.RewardScript != "76a9145131da915d6ac2cddd6684b663552673b8f6770e88ac" ||
		sh.OwnerAddress != "yP1aEFNBAxGBUnr6MXYbTLw7i4Absfcxmv" {
		t.Errorf("share 1: %+v", sh)
	}
	if got.Confirmations != 113 {
		t.Errorf("confirmations %d", got.Confirmations)
	}

	// A banned one: the ban height is the registration height and there are
	// no addresses.
	bh, braw := protxFixture(t, "protx_info_shared_banned.json")
	f.mtx.Lock()
	f.protx[bh.String()] = braw
	f.mtx.Unlock()
	banned, err := node.ProTxInfo(t.Context(), bh)
	if err != nil {
		t.Fatal(err)
	}
	if banned.State.PoSeBanHeight != banned.State.RegisteredHeight || banned.State.PoSeBanHeight != 822 {
		t.Errorf("banned: %+v", banned.State)
	}
}

func TestProTxInfoNotFound(t *testing.T) {
	f, params := newFakeServer(t, 3)
	node := dialFake(t, f, params.GenesisHash)

	// What ElectrumX returned for a dissolved masternode.
	var fixture RPCError
	readFixture(t, "protx_info_notfound.json", &fixture)
	if fixture.Code != 2 || !strings.Contains(fixture.Message, "not found") {
		t.Fatalf("fixture: %+v", fixture)
	}
	f.mtx.Lock()
	f.errs["protx.info"] = "[2]" + fixture.Message
	f.mtx.Unlock()
	if _, err := node.ProTxInfo(t.Context(), chainhash.Hash{1}); !errors.Is(err, ErrProTxNotFound) {
		t.Fatalf("got %v", err)
	}

	// Another failure with the same code is not a missing registration, nor is
	// the same words under another code, nor is dashd's own "Method not found".
	for _, msg := range []string{
		"[2]daemon error: DaemonError({'code': -1, 'message': 'busy'})",
		"[1]x not found",
		"[2]daemon error: DaemonError({'code': -32601, 'message': 'Method not found'})",
	} {
		f.mtx.Lock()
		f.errs["protx.info"] = msg
		f.mtx.Unlock()
		_, err := node.ProTxInfo(t.Context(), chainhash.Hash{1})
		if err == nil || errors.Is(err, ErrProTxNotFound) || errors.Is(err, ErrProTxUnsupported) {
			t.Fatalf("%q: got %v", msg, err)
		}
	}
	// The registration is looked up by its hash in display order: the fake knows
	// only the one it was given, in that order.
	hash, raw := protxFixture(t, "protx_info_shared.json")
	f.mtx.Lock()
	delete(f.errs, "protx.info")
	f.protx[hash.String()] = raw
	f.mtx.Unlock()
	if _, err := node.ProTxInfo(t.Context(), hash); err != nil {
		t.Fatalf("found by its display hash: %v", err)
	}
	reversed := hash
	for i := range reversed {
		reversed[i] = hash[len(hash)-1-i]
	}
	if _, err := node.ProTxInfo(t.Context(), reversed); !errors.Is(err, ErrProTxNotFound) {
		t.Fatalf("a reversed hash found something: %v", err)
	}
}

// A reply that is not about the registration asked for is an error, not state:
// a null result decodes to an empty registration whose heights read as 0 rather
// than "does not apply", and a well formed reply about another one is not this
// one's.
func TestProTxInfoMustBeForTheHashAsked(t *testing.T) {
	f, params := newFakeServer(t, 3)
	node := dialFake(t, f, params.GenesisHash)
	hash, raw := protxFixture(t, "protx_info_shared.json")
	other, _ := protxFixture(t, "protx_info_shared_banned.json")

	f.mtx.Lock()
	f.protx[hash.String()] = json.RawMessage("null")
	f.mtx.Unlock()
	if info, err := node.ProTxInfo(t.Context(), hash); err == nil {
		t.Fatalf("a null result was returned as state: %+v", info)
	}

	f.mtx.Lock()
	f.protx[other.String()] = raw // the reply is about hash
	f.mtx.Unlock()
	if info, err := node.ProTxInfo(t.Context(), other); err == nil {
		t.Fatalf("a reply about another registration was returned: %+v", info)
	}
}

func TestProTxUnsupported(t *testing.T) {
	f, params := newFakeServer(t, 3)
	var fixture RPCError
	readFixture(t, "protx_method_not_found.json", &fixture)
	if fixture.Code != -32601 {
		t.Fatalf("fixture: %+v", fixture)
	}
	f.mtx.Lock()
	f.noProtx = true
	f.mtx.Unlock()
	node := dialFake(t, f, params.GenesisHash)

	for range 3 {
		if _, err := node.ProTxInfo(t.Context(), chainhash.Hash{1}); !errors.Is(err, ErrProTxUnsupported) {
			t.Fatalf("got %v", err)
		}
	}
	f.mtx.Lock()
	n := f.calls["protx.info"]
	f.mtx.Unlock()
	if n != 1 {
		t.Fatalf("the server was asked %d times; the answer is remembered after the first", n)
	}
	// Everything else the node does still works.
	if _, err := node.GetHistory(t.Context(), "00"); err != nil {
		t.Fatalf("the node stopped working after a protx refusal: %v", err)
	}

	// A new node, as after a rotation, asks afresh.
	hash, raw := protxFixture(t, "protx_info_shared.json")
	f.mtx.Lock()
	f.noProtx = false
	f.protx[hash.String()] = raw
	f.mtx.Unlock()
	fresh := dialFake(t, f, params.GenesisHash)
	if _, err := fresh.ProTxInfo(t.Context(), hash); err != nil {
		t.Fatalf("a new node does not ask afresh: %v", err)
	}
}

// TestMerkleRootDeepTrees folds proofs of several levels, which a block of two
// transactions does not exercise, against trees built here the way a block's is:
// pairs are hashed up, and an odd last node is paired with itself.
func TestMerkleRootDeepTrees(t *testing.T) {
	pair := func(a, b chainhash.Hash) chainhash.Hash {
		var cat [64]byte
		copy(cat[:32], a[:])
		copy(cat[32:], b[:])
		return chainhash.DoubleHashH(cat[:])
	}
	for _, n := range []int{2, 3, 5, 8, 11, 16, 21} {
		level := make([]chainhash.Hash, n)
		for i := range level {
			level[i] = chainhash.DoubleHashH([]byte{byte(n), byte(i)})
		}
		leaves := append([]chainhash.Hash(nil), level...)
		// The levels of the tree, leaves first.
		tree := [][]chainhash.Hash{level}
		for len(level) > 1 {
			if len(level)%2 == 1 {
				level = append(append([]chainhash.Hash(nil), level...), level[len(level)-1])
			}
			var next []chainhash.Hash
			for i := 0; i < len(level); i += 2 {
				next = append(next, pair(level[i], level[i+1]))
			}
			tree = append(tree, next)
			level = next
		}
		root := tree[len(tree)-1][0]

		for i := range leaves {
			var proof MerkleProof
			proof.Pos = uint32(i)
			idx := i
			for _, lv := range tree[:len(tree)-1] {
				if len(lv)%2 == 1 {
					lv = append(append([]chainhash.Hash(nil), lv...), lv[len(lv)-1])
				}
				proof.Merkle = append(proof.Merkle, lv[idx^1].String())
				idx >>= 1
			}
			got, err := MerkleRoot(leaves[i], &proof)
			if err != nil || got != root {
				t.Fatalf("%d leaves, leaf %d: %v %v", n, i, got, err)
			}
		}
	}
}

// TestGetRawTransactionDashSpecial is the same check where it is made, with the
// chain the node was dialled for choosing how the bytes are read.
func TestGetRawTransactionDashSpecial(t *testing.T) {
	f, params := newFakeServer(t, 3)
	tx := specialTxs(t)[0]
	f.mtx.Lock()
	f.txs[tx.Txid] = tx.Hex
	f.mtx.Unlock()

	node := dialFake(t, f, params.GenesisHash)
	got, err := node.GetRawTransaction(t.Context(), tx.Txid)
	if err != nil || got != tx.Hex {
		t.Fatalf("a Dash node refused a registration: %v", err)
	}
	btc, err := NewNode(t.Context(), f.addr(), "", params.GenesisHash.String(), bisonwire.ChainBTC)
	if err != nil {
		t.Fatal(err)
	}
	defer btc.Close()
	if _, err := btc.GetRawTransaction(t.Context(), tx.Txid); err == nil {
		t.Fatal("a Bitcoin node accepted a Dash special transaction")
	}
}

// TestMerkleRoot folds the proof of a real registration to the root in the
// header of its block.
func TestMerkleRoot(t *testing.T) {
	var fx struct {
		MerkleProof
		Txid   string `json:"txid"`
		Header string `json:"header"`
	}
	readFixture(t, "merkle_proregtx.json", &fx)
	txid, err := chainhash.NewHashFromStr(fx.Txid)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(fx.Header)
	var hdr wire.BlockHeader
	if err := hdr.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}

	got, err := MerkleRoot(*txid, &fx.MerkleProof)
	if err != nil {
		t.Fatal(err)
	}
	if got != hdr.MerkleRoot {
		t.Fatalf("root %s, header %s", got, hdr.MerkleRoot)
	}

	// Each part of the proof matters.
	flipped := fx.MerkleProof
	flipped.Pos ^= 1
	if r, _ := MerkleRoot(*txid, &flipped); r == hdr.MerkleRoot {
		t.Error("the position is not used")
	}
	sib := fx.MerkleProof
	sib.Merkle = []string{strings.Replace(sib.Merkle[0], sib.Merkle[0][:2], "ff", 1)}
	if r, _ := MerkleRoot(*txid, &sib); r == hdr.MerkleRoot {
		t.Error("the sibling is not used")
	}
	other := *txid
	other[0] ^= 1
	if r, _ := MerkleRoot(other, &fx.MerkleProof); r == hdr.MerkleRoot {
		t.Error("the transaction is not used")
	}
	// Position bits beyond the branch are not a position in a tree of that depth.
	for _, pos := range []uint32{2, 3, 1 << 31} {
		far := fx.MerkleProof
		far.Pos = pos
		if _, err := MerkleRoot(*txid, &far); err == nil {
			t.Errorf("position %d was accepted for a branch of %d", pos, len(far.Merkle))
		}
	}
	if _, err := MerkleRoot(*txid, &MerkleProof{Pos: 1}); err == nil {
		t.Error("a position of 1 was accepted with no branch")
	}
	bad := fx.MerkleProof
	bad.Merkle = []string{"zz"}
	if _, err := MerkleRoot(*txid, &bad); err == nil {
		t.Error("a malformed sibling was accepted")
	}
	// A block of one transaction has an empty branch: the root is the txid.
	if r, err := MerkleRoot(*txid, &MerkleProof{}); err != nil || r != *txid {
		t.Errorf("empty branch: %v %v", r, err)
	}
}
