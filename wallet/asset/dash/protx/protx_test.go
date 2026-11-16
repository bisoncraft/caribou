// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package protx

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// The vectors in testdata are transactions built and accepted by Dash Core on
// regtest (see util/testing/dash). Each file holds the raw transaction and the
// fields dashd decoded from it. regfund, regext, updserv, updreg and revoke
// come from v23.1.4. regshared, regshared_nonet, regevo, regevo_multi,
// upshare, upsharedreg, dis and dis_standby come from v24.0.0 with V24 active
// (the harness `V24_VBPARAMS`), using throwaway keys: the raw transaction and
// the one payload key of `getrawtransaction <txid> 1` were copied from the
// session that mined them, keeping txid, version, type and extraPayloadSize.
type vector struct {
	Hex   string `json:"hex"`
	Dashd struct {
		TxID           string               `json:"txid"`
		Version        uint16               `json:"version"`
		Type           uint16               `json:"type"`
		ProRegTx       *dashdProReg         `json:"proRegTx"`
		ProUpServ      *dashdProUpServ      `json:"proUpServTx"`
		ProUpReg       *dashdProUpReg       `json:"proUpRegTx"`
		ProUpRev       *dashdProUpRev       `json:"proUpRevTx"`
		ProDis         *dashdProDis         `json:"proDisTx"`
		ProUpShare     *dashdProUpShare     `json:"proUpShareTx"`
		ProUpSharedReg *dashdProUpSharedReg `json:"proUpSharedRegTx"`
		PayloadLen     int                  `json:"extraPayloadSize"`
	} `json:"dashd"`
	CollateralAddress string `json:"collateralAddress"`
}

type dashdProReg struct {
	Version         uint16 `json:"version"`
	Type            uint16 `json:"type"`
	CollateralHash  string `json:"collateralHash"`
	CollateralIndex uint32 `json:"collateralIndex"`
	Service         string `json:"service"`
	OwnerAddress    string `json:"ownerAddress"`
	VotingAddress   string `json:"votingAddress"`
	PayoutAddress   string `json:"payoutAddress"`
	PubKeyOperator  string `json:"pubKeyOperator"`
	OperatorReward  uint16 `json:"operatorReward"`
	InputsHash      string `json:"inputsHash"`

	// Version 3 only.
	Addresses         map[string][]string `json:"addresses"`
	Payouts           []dashdPayout       `json:"payouts"`
	Shares            []dashdShare        `json:"shares"`
	EarlyPeriodBlocks uint32              `json:"earlyPeriodBlocks"`
	EarlyPenalty      int64               `json:"earlyPenalty"`
	PlatformNodeID    string              `json:"platformNodeID"`
}

type dashdPayout struct {
	Address string `json:"address"`
	Script  string `json:"script"`
	Reward  uint16 `json:"reward"`
}

type dashdShare struct {
	Amount        int64  `json:"amount"`
	RefundAddress string `json:"refundAddress"`
	RefundScript  string `json:"refundScript"`
	RewardAddress string `json:"rewardAddress"`
	RewardScript  string `json:"rewardScript"`
	OwnerAddress  string `json:"ownerAddress"`
}

type dashdProDis struct {
	Version    uint16 `json:"version"`
	ProTxHash  string `json:"proTxHash"`
	ActorIndex uint16 `json:"actorIndex"`
	SigCount   int    `json:"sigCount"`
}

type dashdProUpShare struct {
	Version       uint16 `json:"version"`
	ProTxHash     string `json:"proTxHash"`
	ShareIndex    uint16 `json:"shareIndex"`
	RewardAddress string `json:"rewardAddress"`
	InputsHash    string `json:"inputsHash"`
}

type dashdProUpSharedReg struct {
	Version        uint16 `json:"version"`
	ProTxHash      string `json:"proTxHash"`
	VotingAddress  string `json:"votingAddress"`
	PubKeyOperator string `json:"pubKeyOperator"`
	InputsHash     string `json:"inputsHash"`
	SigCount       int    `json:"sigCount"`
}

type dashdProUpServ struct {
	ProTxHash  string `json:"proTxHash"`
	Service    string `json:"service"`
	InputsHash string `json:"inputsHash"`
}

type dashdProUpReg struct {
	ProTxHash      string `json:"proTxHash"`
	VotingAddress  string `json:"votingAddress"`
	PayoutAddress  string `json:"payoutAddress"`
	PubKeyOperator string `json:"pubKeyOperator"`
	InputsHash     string `json:"inputsHash"`
}

type dashdProUpRev struct {
	ProTxHash  string `json:"proTxHash"`
	Reason     uint16 `json:"reason"`
	InputsHash string `json:"inputsHash"`
}

// regtestParams has the Dash regtest address prefixes ('y' for P2PKH).
func regtestParams() *chaincfg.Params {
	p := chaincfg.RegressionNetParams
	p.PubKeyHashAddrID = 140
	p.ScriptHashAddrID = 19
	return &p
}

func loadVector(t testing.TB, name string) (*vector, *bisonwire.Tx) {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	v := new(vector)
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}

	txBytes := mustHex(t, v.Hex)
	tx := &bisonwire.Tx{Chain: "dash"}
	if err := tx.Deserialize(bytes.NewReader(txBytes)); err != nil {
		t.Fatalf("%s: deserialize tx: %v", name, err)
	}
	if got := tx.TxHash().String(); got != v.Dashd.TxID {
		t.Fatalf("%s: txid %s, dashd says %s", name, got, v.Dashd.TxID)
	}
	if int(tx.Type) != int(v.Dashd.Type) || len(tx.ExtraPayload) != v.Dashd.PayloadLen {
		t.Fatalf("%s: type %d payload %d bytes, dashd says %d and %d",
			name, tx.Type, len(tx.ExtraPayload), v.Dashd.Type, v.Dashd.PayloadLen)
	}
	return v, tx
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func mustHash(t *testing.T, s string) chainhash.Hash {
	t.Helper()
	h, err := chainhash.NewHashFromStr(s)
	if err != nil {
		t.Fatalf("bad hash %q: %v", s, err)
	}
	return *h
}

// keyIDAddress encodes a key id as a Dash regtest P2PKH address.
func keyIDAddress(t *testing.T, id [KeyIDSize]byte) string {
	t.Helper()
	a, err := btcutil.NewAddressPubKeyHash(id[:], regtestParams())
	if err != nil {
		t.Fatal(err)
	}
	return a.EncodeAddress()
}

// scriptAddress encodes the address a payout script pays to.
func scriptAddress(t *testing.T, script []byte) string {
	t.Helper()
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, regtestParams())
	if err != nil || len(addrs) != 1 {
		t.Fatalf("payout script %x: %d addresses, err %v", script, len(addrs), err)
	}
	return addrs[0].EncodeAddress()
}

// roundTrip checks that serializing the decoded payload reproduces the bytes
// dashd produced.
func roundTrip(t *testing.T, tx *bisonwire.Tx, p interface{ Serialize(w io.Writer) error }) {
	t.Helper()
	var buf bytes.Buffer
	if err := p.Serialize(&buf); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), tx.ExtraPayload) {
		t.Fatalf("round trip differs:\n got %x\nwant %x", buf.Bytes(), tx.ExtraPayload)
	}
}

func TestProRegTxInternalCollateral(t *testing.T) {
	t.Parallel()

	v, tx := loadVector(t, "regfund")
	var p ProRegTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, tx, &p)

	d := v.Dashd.ProRegTx
	if p.Version != Version || p.MNType != TypeRegular || p.Mode != 0 {
		t.Fatalf("version %d type %d mode %d", p.Version, p.MNType, p.Mode)
	}
	if p.Collateral.Hash != (chainhash.Hash{}) || p.Collateral.Index != d.CollateralIndex {
		t.Fatalf("collateral %v, want zero hash and index %d", p.Collateral, d.CollateralIndex)
	}
	checkProRegFields(t, &p, d)
	if len(p.PayloadSig) != 0 {
		t.Fatalf("internal collateral has a %d byte payloadSig", len(p.PayloadSig))
	}
	if got := InputsHash(tx.TxIn); got != p.InputsHash {
		t.Fatalf("InputsHash %s, payload has %s", got, p.InputsHash)
	}
}

func TestProRegTxExternalCollateral(t *testing.T) {
	t.Parallel()

	v, tx := loadVector(t, "regext")
	var p ProRegTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, tx, &p)

	d := v.Dashd.ProRegTx
	if p.Collateral.Hash != mustHash(t, d.CollateralHash) || p.Collateral.Index != d.CollateralIndex {
		t.Fatalf("collateral %v, want %s:%d", p.Collateral, d.CollateralHash, d.CollateralIndex)
	}
	checkProRegFields(t, &p, d)
	if got := InputsHash(tx.TxIn); got != p.InputsHash {
		t.Fatalf("InputsHash %s, payload has %s", got, p.InputsHash)
	}

	// The collateral key signed CollateralMessage, so the signature must
	// recover to the collateral address. This pins both the message format
	// and PayloadHash.
	if len(p.PayloadSig) != 65 {
		t.Fatalf("payloadSig is %d bytes, want 65", len(p.PayloadSig))
	}
	msg, err := p.CollateralMessage(regtestParams())
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ecdsa.RecoverCompact(p.PayloadSig, signedMessageHash(msg))
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	addr, err := btcutil.NewAddressPubKeyHash(
		btcutil.Hash160(pub.SerializeCompressed()), regtestParams())
	if err != nil {
		t.Fatal(err)
	}
	if got := addr.EncodeAddress(); got != v.CollateralAddress {
		t.Fatalf("signature recovers to %s, collateral is %s (message %q)",
			got, v.CollateralAddress, msg)
	}
}

func checkProRegFields(t *testing.T, p *ProRegTx, d *dashdProReg) {
	t.Helper()

	if got := keyIDAddress(t, p.KeyIDOwner); got != d.OwnerAddress {
		t.Errorf("owner %s, want %s", got, d.OwnerAddress)
	}
	if got := keyIDAddress(t, p.KeyIDVoting); got != d.VotingAddress {
		t.Errorf("voting %s, want %s", got, d.VotingAddress)
	}
	if got := hex.EncodeToString(p.PubKeyOperator[:]); got != d.PubKeyOperator {
		t.Errorf("operator key %s, want %s", got, d.PubKeyOperator)
	}
	if p.OperatorReward != d.OperatorReward {
		t.Errorf("operator reward %d, want %d", p.OperatorReward, d.OperatorReward)
	}
	if got := p.Service.IP[12:]; !bytes.Equal(got, []byte{127, 0, 0, 1}) ||
		strconv.Itoa(int(p.Service.Port)) != d.Service[len("127.0.0.1:"):] {
		t.Errorf("service %v:%d, want %s", got, p.Service.Port, d.Service)
	}
	if p.InputsHash != mustHash(t, d.InputsHash) {
		t.Errorf("inputs hash %s, want %s", p.InputsHash, d.InputsHash)
	}
	if got := scriptAddress(t, p.ScriptPayout); got != d.PayoutAddress {
		t.Errorf("payout %s, want %s", got, d.PayoutAddress)
	}
}

func TestProUpServTx(t *testing.T) {
	t.Parallel()

	v, tx := loadVector(t, "updserv")
	var p ProUpServTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, tx, &p)

	d := v.Dashd.ProUpServ
	if p.ProTxHash != mustHash(t, d.ProTxHash) || p.InputsHash != mustHash(t, d.InputsHash) {
		t.Fatalf("proTxHash %s inputsHash %s, want %s and %s",
			p.ProTxHash, p.InputsHash, d.ProTxHash, d.InputsHash)
	}
	if got := p.Service.IP[12:]; !bytes.Equal(got, []byte{127, 0, 0, 1}) ||
		strconv.Itoa(int(p.Service.Port)) != d.Service[len("127.0.0.1:"):] {
		t.Fatalf("service %v:%d, want %s", got, p.Service.Port, d.Service)
	}
	if got := InputsHash(tx.TxIn); got != p.InputsHash {
		t.Fatalf("InputsHash %s, payload has %s", got, p.InputsHash)
	}
	checkSignedBodyHash(t, tx, p.PayloadHash, BLSSigSize)
}

func TestProUpRegTx(t *testing.T) {
	t.Parallel()

	v, tx := loadVector(t, "updreg")
	var p ProUpRegTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, tx, &p)

	d := v.Dashd.ProUpReg
	if p.ProTxHash != mustHash(t, d.ProTxHash) || p.InputsHash != mustHash(t, d.InputsHash) {
		t.Fatalf("proTxHash %s inputsHash %s", p.ProTxHash, p.InputsHash)
	}
	if got := keyIDAddress(t, p.KeyIDVoting); got != d.VotingAddress {
		t.Fatalf("voting %s, want %s", got, d.VotingAddress)
	}
	if got := hex.EncodeToString(p.PubKeyOperator[:]); got != d.PubKeyOperator {
		t.Fatalf("operator key %s, want %s", got, d.PubKeyOperator)
	}
	if got := scriptAddress(t, p.ScriptPayout); got != d.PayoutAddress {
		t.Fatalf("payout %s, want %s", got, d.PayoutAddress)
	}
	if p.Mode != 0 {
		t.Fatalf("mode %d, want 0", p.Mode)
	}
	if got := InputsHash(tx.TxIn); got != p.InputsHash {
		t.Fatalf("InputsHash %s, payload has %s", got, p.InputsHash)
	}

	// The owner key signed PayloadHash directly, so the signature must
	// recover to the owner registered by the ProRegTx this updates.
	reg, regTx := loadVector(t, "regfund")
	var regPayload ProRegTx
	if err := Parse(regTx.ExtraPayload, &regPayload); err != nil {
		t.Fatal(err)
	}
	if regTx.TxHash() != p.ProTxHash {
		t.Fatalf("updates %s, but the vector registration is %s", p.ProTxHash, regTx.TxHash())
	}
	hash, err := p.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.PayloadSig) != 65 {
		t.Fatalf("payloadSig is %d bytes, want 65", len(p.PayloadSig))
	}
	pub, _, err := ecdsa.RecoverCompact(p.PayloadSig, hash[:])
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	var keyID [KeyIDSize]byte
	copy(keyID[:], btcutil.Hash160(pub.SerializeCompressed()))
	if got := keyIDAddress(t, keyID); got != reg.Dashd.ProRegTx.OwnerAddress {
		t.Fatalf("signature recovers to %s, owner is %s", got, reg.Dashd.ProRegTx.OwnerAddress)
	}
}

func TestProUpRevTx(t *testing.T) {
	t.Parallel()

	v, tx := loadVector(t, "revoke")
	var p ProUpRevTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, tx, &p)

	d := v.Dashd.ProUpRev
	if p.ProTxHash != mustHash(t, d.ProTxHash) || p.InputsHash != mustHash(t, d.InputsHash) ||
		p.Reason != d.Reason {

		t.Fatalf("proTxHash %s inputsHash %s reason %d", p.ProTxHash, p.InputsHash, p.Reason)
	}
	if got := InputsHash(tx.TxIn); got != p.InputsHash {
		t.Fatalf("InputsHash %s, payload has %s", got, p.InputsHash)
	}
	checkSignedBodyHash(t, tx, p.PayloadHash, BLSSigSize)
}

// checkSignedBodyHash checks that an operator-signed payload's PayloadHash is
// the hash of the payload without its trailing sigLen byte signature, which is
// the digest the operator's BLS signature covers.
func checkSignedBodyHash(t *testing.T, tx *bisonwire.Tx, payloadHash func() (chainhash.Hash, error), sigLen int) {
	t.Helper()
	got, err := payloadHash()
	if err != nil {
		t.Fatal(err)
	}
	body := tx.ExtraPayload[:len(tx.ExtraPayload)-sigLen]
	if want := chainhash.DoubleHashH(body); got != want {
		t.Fatalf("PayloadHash %s, want hash of the payload without its signature %s", got, want)
	}
}

// TestPayloadHashExcludesSig checks that the hash a signature commits to does
// not change with the signature, which is what lets the signer fill it in last.
func TestPayloadHashExcludesSig(t *testing.T) {
	t.Parallel()

	_, tx := loadVector(t, "regext")
	var p ProRegTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}
	with, err := p.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	p.PayloadSig = nil
	without, err := p.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if with != without {
		t.Fatalf("payload hash depends on payloadSig: %s vs %s", with, without)
	}
	p.OperatorReward++
	changed, err := p.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if changed == with {
		t.Fatal("payload hash ignores operatorReward")
	}
}

func TestInputsHashCommitsToOrderAndIndex(t *testing.T) {
	t.Parallel()

	a := &wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}}
	b := &wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{2}, Index: 1}}
	c := &wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{2}, Index: 2}}

	base := InputsHash([]*wire.TxIn{a, b})
	for name, ins := range map[string][]*wire.TxIn{
		"reordered": {b, a},
		"index":     {a, c},
		"fewer":     {a},
	} {
		if InputsHash(ins) == base {
			t.Errorf("%s inputs hash equal to the original", name)
		}
	}
}

// payload is implemented by every payload type.
type payload interface {
	payloadDecoder
	Serialize(w io.Writer) error
}

// hashedPayload is a payload that has a PayloadHash.
type hashedPayload interface {
	payload
	PayloadHash() (chainhash.Hash, error)
}

var payloadKinds = []struct {
	name   string
	vector string
	// badVersions are payload versions the kind must refuse.
	badVersions []uint16
	// hasType is whether the payload carries a masternode type after its
	// version.
	hasType bool
	// noEvo is whether the kind refuses an EvoNode.
	noEvo bool
	new   func() payload
}{
	{"ProRegTx", "regfund", []uint16{1, 4}, true, true, func() payload { return new(ProRegTx) }},
	{"ProRegTx v3 shared", "regshared", []uint16{1, 4}, true, false, func() payload { return new(ProRegTx) }},
	{"ProRegTx v3 evo", "regevo", []uint16{1, 4}, true, false, func() payload { return new(ProRegTx) }},
	{"ProUpServTx", "updserv", []uint16{1, 3}, true, true, func() payload { return new(ProUpServTx) }},
	{"ProUpRegTx", "updreg", []uint16{1, 3}, false, false, func() payload { return new(ProUpRegTx) }},
	{"ProUpRevTx", "revoke", []uint16{1, 3}, false, false, func() payload { return new(ProUpRevTx) }},
	{"ProDisTx", "dis", []uint16{0, 2}, false, false, func() payload { return new(ProDisTx) }},
	{"ProUpShareTx", "upshare", []uint16{0, 2}, false, false, func() payload { return new(ProUpShareTx) }},
	{"ProUpSharedRegTx", "upsharedreg", []uint16{0, 2}, false, false, func() payload { return new(ProUpSharedRegTx) }},
}

func TestParseRejects(t *testing.T) {
	t.Parallel()

	for _, k := range payloadKinds {
		_, tx := loadVector(t, k.vector)
		good := tx.ExtraPayload

		withVersion := func(v uint16) []byte {
			b := bytes.Clone(good)
			binary.LittleEndian.PutUint16(b, v)
			return b
		}

		type rejectCase struct {
			name    string
			payload []byte
			wantErr error
		}
		tests := []rejectCase{
			{"trailing byte", append(bytes.Clone(good), 0), ErrTrailingData},
			{"truncated", good[:len(good)-10], io.ErrUnexpectedEOF},
			{"empty", nil, io.ErrUnexpectedEOF},
		}
		for _, v := range k.badVersions {
			tests = append(tests, rejectCase{"version " + strconv.Itoa(int(v)), withVersion(v), ErrUnsupportedVersion})
		}
		if k.hasType {
			other := bytes.Clone(good)
			binary.LittleEndian.PutUint16(other[2:], 2)
			tests = append(tests, rejectCase{"unknown masternode type", other, ErrUnsupportedType})
		}
		if k.noEvo {
			evo := bytes.Clone(good)
			binary.LittleEndian.PutUint16(evo[2:], TypeEvo)
			tests = append(tests, rejectCase{"evonode", evo, ErrUnsupportedType})
		}
		for _, tc := range tests {
			t.Run(k.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				err := Parse(tc.payload, k.new())
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error %v, want %v", err, tc.wantErr)
				}
			})
		}
	}
}

// hasService is whether p carries a Service, which Dash stores differently
// from how it was written when it is not a valid address (see Service).
func hasService(p payload) bool {
	switch p := p.(type) {
	case *ProRegTx:
		return p.Version == Version
	case *ProUpServTx:
		return true
	}
	return false
}

// FuzzParse checks that no input panics a decoder, and that anything a decoder
// accepts serializes back to the same bytes, so the hashes recomputed from the
// decoded fields cover exactly the bytes that were received. The one exception
// is a Service Dash would store as the empty address: the decoder normalizes
// it, as Dash does, so only the decoded fields have to survive.
func FuzzParse(f *testing.F) {
	for _, k := range payloadKinds {
		_, tx := loadVector(f, k.vector)
		f.Add(tx.ExtraPayload)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, k := range payloadKinds {
			p := k.new()
			if err := Parse(b, p); err != nil {
				continue
			}
			var buf bytes.Buffer
			if err := p.Serialize(&buf); err != nil {
				t.Fatalf("%s: serialize a parsed payload: %v", k.name, err)
			}
			if bytes.Equal(buf.Bytes(), b) {
				continue
			}
			again := k.new()
			if !hasService(p) || Parse(buf.Bytes(), again) != nil || !reflect.DeepEqual(p, again) {
				t.Fatalf("%s: round trip differs:\n got %x\nwant %x", k.name, buf.Bytes(), b)
			}
		}
	})
}

func TestSerializeRejectsUnsupported(t *testing.T) {
	t.Parallel()

	long := func(n int) []byte { return make([]byte, n) }
	ext := NetInfo{Version: 1}
	tests := []struct {
		name    string
		p       payload
		wantErr error
	}{
		{"ProRegTx version", &ProRegTx{Version: 4}, ErrUnsupportedVersion},
		{"ProRegTx evonode", &ProRegTx{Version: Version, MNType: TypeEvo}, ErrUnsupportedType},
		{"ProRegTx unknown type", &ProRegTx{Version: VersionExtAddr, MNType: 2, NetInfo: ext}, ErrUnsupportedType},
		{"ProRegTx v3 without netInfo", &ProRegTx{Version: VersionExtAddr}, ErrUnsupportedNetInfo},
		{"ProRegTx v3 with a v2 payout script", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, ScriptPayout: []byte{0x51}}, ErrUnsupportedVersion},
		{"ProRegTx v3 with a v2 service", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, Service: Service{Port: 1}}, ErrUnsupportedVersion},
		{"ProRegTx v2 with shares", &ProRegTx{Version: Version, Shares: []Share{{}}}, ErrUnsupportedVersion},
		{"ProRegTx v2 with netInfo", &ProRegTx{Version: Version, NetInfo: ext}, ErrUnsupportedVersion},
		{"ProRegTx v2 with an early penalty", &ProRegTx{Version: Version, EarlyPenalty: 1}, ErrUnsupportedVersion},
		{"ProRegTx platform node on a regular masternode", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, PlatformNodeID: [KeyIDSize]byte{1}}, ErrUnsupportedType},
		{"ProUpServTx version", &ProUpServTx{Version: 3}, ErrUnsupportedVersion},
		{"ProUpServTx evonode", &ProUpServTx{Version: Version, MNType: 1}, ErrUnsupportedType},
		{"ProUpRegTx version", &ProUpRegTx{Version: 3}, ErrUnsupportedVersion},
		{"ProUpRevTx version", &ProUpRevTx{Version: 3}, ErrUnsupportedVersion},
		{"ProDisTx version", &ProDisTx{Version: 2}, ErrUnsupportedVersion},
		{"ProUpShareTx version", &ProUpShareTx{Version: 2}, ErrUnsupportedVersion},
		{"ProUpSharedRegTx version", &ProUpSharedRegTx{Version: 2}, ErrUnsupportedVersion},

		// A payload that cannot be read back must not be written.
		{"ProRegTx script", &ProRegTx{Version: Version, ScriptPayout: long(maxScriptSize + 1)}, ErrFieldTooLong},
		{"ProRegTx sig", &ProRegTx{Version: Version, PayloadSig: long(maxSigSize + 1)}, ErrFieldTooLong},
		{"ProRegTx v3 sig", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, PayloadSig: long(maxSigSize + 1)}, ErrFieldTooLong},
		{"ProRegTx v3 payout script", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, Payouts: []Payout{{Script: long(maxScriptSize + 1)}}}, ErrFieldTooLong},
		{"ProRegTx v3 refund script", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, Shares: []Share{{RefundScript: long(maxScriptSize + 1)}}}, ErrFieldTooLong},
		{"ProRegTx v3 reward script", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, Shares: []Share{{RewardScript: long(maxScriptSize + 1)}}}, ErrFieldTooLong},
		{"ProRegTx v3 payouts", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, Payouts: make([]Payout, maxCount+1)}, ErrFieldTooLong},
		{"ProRegTx v3 shares", &ProRegTx{Version: VersionExtAddr, NetInfo: ext, Shares: make([]Share, maxCount+1)}, ErrFieldTooLong},
		{"ProUpServTx script", &ProUpServTx{Version: Version, ScriptOperatorPay: long(maxScriptSize + 1)}, ErrFieldTooLong},
		{"ProUpRegTx script", &ProUpRegTx{Version: Version, ScriptPayout: long(maxScriptSize + 1)}, ErrFieldTooLong},
		{"ProUpRegTx sig", &ProUpRegTx{Version: Version, PayloadSig: long(maxSigSize + 1)}, ErrFieldTooLong},
		{"ProUpShareTx script", &ProUpShareTx{Version: SharedVersion, ScriptReward: long(maxScriptSize + 1)}, ErrFieldTooLong},
		{"ProUpShareTx sig", &ProUpShareTx{Version: SharedVersion, Sig: long(maxSigSize + 1)}, ErrFieldTooLong},
		{"ProDisTx sigs", &ProDisTx{Version: SharedVersion, Sigs: make([][CompactSigSize]byte, maxCount+1)}, ErrFieldTooLong},
		{"ProUpSharedRegTx sigs", &ProUpSharedRegTx{Version: SharedVersion, Sigs: make([][CompactSigSize]byte, maxCount+1)}, ErrFieldTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := tc.p.Serialize(io.Discard); !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestParseRejectsEveryTruncation checks that no proper prefix of a payload is
// accepted, and that none fails with a bare io.EOF, which a caller could take
// for a clean end of input.
func TestParseRejectsEveryTruncation(t *testing.T) {
	t.Parallel()

	for _, k := range payloadKinds {
		_, tx := loadVector(t, k.vector)
		for n := range len(tx.ExtraPayload) {
			err := Parse(tx.ExtraPayload[:n], k.new())
			if err == nil {
				t.Fatalf("%s: accepted a payload cut to %d of %d bytes", k.name, n, len(tx.ExtraPayload))
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("%s: %d of %d bytes failed with %v, want io.ErrUnexpectedEOF",
					k.name, n, len(tx.ExtraPayload), err)
			}
		}
	}
}

// TestServiceMustSurviveDashRoundTrip checks the addresses Dash stores
// differently from how they are written. Dash hashes its parsed copy, so a
// payload with such an address must decode to the empty Service and hash as
// if it had been written that way, or a signature over Dash's hash would be
// rejected.
func TestServiceMustSurviveDashRoundTrip(t *testing.T) {
	t.Parallel()

	v4 := func(a, b, c, d byte) [16]byte {
		return [16]byte{10: 0xff, 11: 0xff, 12: a, 13: b, 14: c, 15: d}
	}
	prefixed := func(prefix ...byte) [16]byte {
		var ip [16]byte
		copy(ip[:], prefix)
		ip[15] = 1
		return ip
	}

	tests := []struct {
		name  string
		ip    [16]byte
		port  uint16
		empty bool // Dash stores the address as the empty Service
	}{
		{"public ipv4", v4(8, 8, 8, 8), 9999, false},
		{"loopback", v4(127, 0, 0, 1), 19999, false},
		{"public ipv6", prefixed(0x20, 0x01, 0x48, 0x60), 9999, false},
		{"empty address", [16]byte{}, 0, false},
		{"zero port is kept", v4(8, 8, 8, 8), 0, false},
		{"unspecified with a port", [16]byte{}, 9999, true},
		// Found by FuzzParse: an IPv4 address that is not usable.
		{"v4-mapped unspecified", v4(0, 0, 0, 0), 0, true},
		{"v4-mapped broadcast", v4(255, 255, 255, 255), 9999, true},
		{"ipv4 any", v4(0, 0, 0, 0), 19998, true},
		{"ipv4 any without a port", v4(0, 0, 0, 0), 0, true},
		{"ipv4 broadcast", v4(255, 255, 255, 255), 9999, true},
		{"documentation prefix", prefixed(0x20, 0x01, 0x0d, 0xb8), 9999, true},
		{"tor v2 prefix", prefixed(0xfd, 0x87, 0xd8, 0x7e, 0xeb, 0x43), 9999, true},
		{"internal prefix", prefixed(0xfd, 0x6b, 0x88, 0xc0, 0x87, 0x24), 9999, true},
	}

	// The address sits after the version, type and mode of a ProRegTx and its
	// 36 byte collateral, and after the version, type and hash of a ProUpServTx.
	places := []struct {
		name   string
		vector string
		offset int
		new    func() hashedPayload
	}{
		{"ProRegTx", "regfund", 6 + 36, func() hashedPayload { return new(ProRegTx) }},
		{"ProUpServTx", "updserv", 4 + 32, func() hashedPayload { return new(ProUpServTx) }},
	}
	for _, pl := range places {
		_, tx := loadVector(t, pl.vector)
		for _, tc := range tests {
			t.Run(pl.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				b := bytes.Clone(tx.ExtraPayload)
				copy(b[pl.offset:], tc.ip[:])
				binary.BigEndian.PutUint16(b[pl.offset+16:], tc.port)

				p := pl.new()
				if err := Parse(b, p); err != nil {
					t.Fatal(err)
				}
				if tc.empty {
					clean := bytes.Clone(b)
					clear(clean[pl.offset : pl.offset+18])
					want := pl.new()
					if err := Parse(clean, want); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(p, want) {
						t.Fatalf("decoded %+v, want the empty address %+v", p, want)
					}
					gotHash, err := p.PayloadHash()
					if err != nil {
						t.Fatal(err)
					}
					wantHash, err := want.PayloadHash()
					if err != nil {
						t.Fatal(err)
					}
					if gotHash != wantHash {
						t.Fatal("payload hash differs from the one with the empty address")
					}
					return
				}
				var buf bytes.Buffer
				if err := p.Serialize(&buf); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(buf.Bytes(), b) {
					t.Fatal("accepted address does not round trip")
				}
			})
		}
	}

	// Serialize refuses to write one too.
	p := &ProRegTx{Version: Version, Service: Service{Port: 9999}}
	if err := p.Serialize(io.Discard); !errors.Is(err, ErrNonCanonicalAddress) {
		t.Fatalf("serialize: %v, want %v", err, ErrNonCanonicalAddress)
	}
	if _, err := p.PayloadHash(); !errors.Is(err, ErrNonCanonicalAddress) {
		t.Fatalf("payload hash: %v, want %v", err, ErrNonCanonicalAddress)
	}
}

// TestParseLimits pins the bounds on the variable length fields, which stop a
// malformed length prefix from forcing a large allocation.
func TestParseLimits(t *testing.T) {
	t.Parallel()

	_, tx := loadVector(t, "regfund")
	var base ProRegTx
	if err := Parse(tx.ExtraPayload, &base); err != nil {
		t.Fatal(err)
	}

	// Serialize refuses a field over the limit, so the payloads that carry
	// one are spliced together by hand: the fields up to the payout script,
	// the script, the inputs hash and the signature.
	var head bytes.Buffer
	if err := base.serialize(&head, false); err != nil {
		t.Fatal(err)
	}
	scriptLenAt := head.Len() - chainhash.HashSize - 1 - len(base.ScriptPayout)
	build := func(script, sig []byte) []byte {
		var buf bytes.Buffer
		buf.Write(head.Bytes()[:scriptLenAt])
		if err := wire.WriteVarBytes(&buf, 0, script); err != nil {
			t.Fatal(err)
		}
		buf.Write(base.InputsHash[:])
		if err := wire.WriteVarBytes(&buf, 0, sig); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	parse := func(script, sig []byte) error {
		return Parse(build(script, sig), new(ProRegTx))
	}
	var msgErr *wire.MessageError

	if err := parse(make([]byte, maxScriptSize), nil); err != nil {
		t.Errorf("script at the limit: %v", err)
	}
	if err := parse(make([]byte, maxScriptSize+1), nil); !errors.As(err, &msgErr) {
		t.Errorf("script over the limit: %v, want a wire message error", err)
	}
	if err := parse(base.ScriptPayout, make([]byte, BLSSigSize)); err != nil {
		t.Errorf("signature at the limit: %v", err)
	}
	if err := parse(base.ScriptPayout, make([]byte, BLSSigSize+1)); !errors.As(err, &msgErr) {
		t.Errorf("signature over the limit: %v, want a wire message error", err)
	}

	// A length prefix of 16 MB must fail before anything that size is read.
	huge := append(bytes.Clone(head.Bytes()[:scriptLenAt]), 0xfe, 0xff, 0xff, 0xff, 0x00)
	if err := Parse(huge, new(ProRegTx)); !errors.As(err, &msgErr) {
		t.Errorf("huge script length: %v, want a wire message error", err)
	}
}

// signedMessageHash is the digest a Dash message signature covers: the double
// SHA-256 of the length prefixed magic and message.
func signedMessageHash(msg string) []byte {
	var buf bytes.Buffer
	for _, s := range []string{"DarkCoin Signed Message:\n", msg} {
		_ = wire.WriteVarString(&buf, 0, s)
	}
	h := chainhash.DoubleHashB(buf.Bytes())
	return h
}

// TestCollateralMessagePayoutEncoding checks that only the scripts Dash's
// ExtractDestination knows are written as addresses.
func TestCollateralMessagePayoutEncoding(t *testing.T) {
	t.Parallel()

	_, tx := loadVector(t, "regfund")
	var p ProRegTx
	if err := Parse(tx.ExtraPayload, &p); err != nil {
		t.Fatal(err)
	}

	// 1-of-1 bare multisig: OP_1 <33 byte key> OP_1 OP_CHECKMULTISIG.
	key, err := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	if err != nil {
		t.Fatal(err)
	}
	multisig, err := txscript.NewScriptBuilder().AddOp(txscript.OP_1).AddData(key).
		AddOp(txscript.OP_1).AddOp(txscript.OP_CHECKMULTISIG).Script()
	if err != nil {
		t.Fatal(err)
	}
	// btcd reports one address for it, which is what makes the gate matter.
	if _, addrs, _, err := txscript.ExtractPkScriptAddrs(multisig, regtestParams()); err != nil || len(addrs) != 1 {
		t.Fatalf("setup: %d addresses, err %v", len(addrs), err)
	}
	p.ScriptPayout = multisig

	msg, err := p.CollateralMessage(regtestParams())
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := strings.Cut(msg, "|"); got != hex.EncodeToString(multisig) {
		t.Fatalf("payout %q, want hex %x", got, multisig)
	}
}
