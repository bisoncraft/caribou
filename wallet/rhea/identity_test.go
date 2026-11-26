package rhea

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset/eth/evmkey"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/ethereum/go-ethereum/crypto"
)

// evmAssetSeed mirrors core.AssetSeedAndPass for an EVM asset: blake256 of
// the app seed followed by the big-endian asset ID 60. Core cannot be
// imported here; its derivation produces the same asset seed for the vector
// below.
func evmAssetSeed(appSeed []byte) []byte {
	b := make([]byte, len(appSeed)+4)
	copy(b, appSeed)
	binary.BigEndian.PutUint32(b[len(appSeed):], 60)
	s := blake256.Sum256(b)
	return s[:]
}

// TestDerivationV1Vector is the published derivation v1 vector: fixed app
// seed → EVM asset seed → m/44'/60'/0'/0/0 → private key → identity. The
// identity must equal the EVM wallet address for the same seed. v1 is
// immutable once released; a failure here means existing lending accounts
// would no longer be found, not that the vector needs updating.
func TestDerivationV1Vector(t *testing.T) {
	appSeed := make([]byte, 64)
	for i := range appSeed {
		appSeed[i] = byte(i)
	}
	const (
		wantAssetSeed = "f54ac2b62537d6c3e65498ff6d18c7a377fed3883e5c27bc85e226ada9a1ffc2"
		wantKey       = "68b8e4959fd454e80f441017483e51d9ce38deca146fb6cc2c38623f39ed0673"
		wantIdentity  = Identity("4500538e8c3060947e71621e9278cbde8ef387d3")
	)
	seed := evmAssetSeed(appSeed)

	k, err := DeriveKey(DerivationV1, seed)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}

	// The identity is the EVM wallet's address: same seed, account 0 of
	// the evmkey derivation the eth wallet uses.
	raw, zero, err := evmkey.PrivKeyFromSeedAccount(seed, 0)
	if err != nil {
		t.Fatalf("PrivKeyFromSeedAccount: %v", err)
	}
	priv, err := crypto.ToECDSA(raw)
	if err != nil {
		t.Fatalf("ToECDSA: %v", err)
	}
	zero()
	if want := IdentityFromAddress(crypto.PubkeyToAddress(priv.PublicKey)); k.Identity() != want {
		t.Fatalf("identity %s differs from the EVM wallet address %s", k.Identity(), want)
	}

	if hex.EncodeToString(seed) != wantAssetSeed {
		t.Fatalf("asset seed %x, want %s", seed, wantAssetSeed)
	}
	if got := hex.EncodeToString(crypto.FromECDSA(k.priv)); got != wantKey {
		t.Fatalf("key %s, want %s", got, wantKey)
	}
	if k.Identity() != wantIdentity {
		t.Fatalf("identity %s, want %s", k.Identity(), wantIdentity)
	}
	id, err := DeriveIdentity(DerivationV1, seed)
	if err != nil || id != wantIdentity {
		t.Fatalf("DeriveIdentity = %s, %v", id, err)
	}
	if _, err := DeriveKey(DerivationVersion(2), seed); err == nil {
		t.Fatalf("unknown derivation version accepted")
	}
	k.Zero()
	if _, err := k.SignCreation(); err == nil {
		t.Fatalf("zeroed key signed")
	}
}

func TestParseIdentity(t *testing.T) {
	const want = Identity("242c9fc3162321d326892cfe044fc0d9ffc3e9b0")
	for _, s := range []string{
		"242c9fc3162321d326892cfe044fc0d9ffc3e9b0",
		"0x242C9FC3162321D326892CFE044FC0D9FFC3E9B0",
		"0X242c9fc3162321D326892cfe044fc0d9ffc3e9b0",
	} {
		id, err := ParseIdentity(s)
		if err != nil || id != want {
			t.Fatalf("ParseIdentity(%q) = %q, %v", s, id, err)
		}
	}
	for _, s := range []string{"", "0x", "242c9fc3162321d326892cfe044fc0d9ffc3e9b", "242c9fc3162321d326892cfe044fc0d9ffc3e9bz", "0x0x242c9fc3162321d326892cfe044fc0d9ffc3e9"} {
		if _, err := ParseIdentity(s); err == nil {
			t.Fatalf("ParseIdentity(%q) accepted", s)
		}
	}
	if got := IdentityFromAddress(want.Address()); got != want {
		t.Fatalf("address round trip %s", got)
	}
}

func TestWalletJSON(t *testing.T) {
	id := Identity("242c9fc3162321d326892cfe044fc0d9ffc3e9b0")
	b, err := json.Marshal(id.Wallet())
	if err != nil || string(b) != `{"EVM":"242c9fc3162321d326892cfe044fc0d9ffc3e9b0"}` {
		t.Fatalf("wallet JSON %s, %v", b, err)
	}
	if string(CreationMessage(id)) != `[{"EVM":"242c9fc3162321d326892cfe044fc0d9ffc3e9b0"}]` {
		t.Fatalf("creation message %s", CreationMessage(id))
	}
	var ws []Wallet
	if err := json.Unmarshal([]byte(`[{"WebAuthn":"p256:abc"},{"Near":"bonjupiglet.near"}]`), &ws); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if ws[0].Kind != "WebAuthn" || ws[1].ID != "bonjupiglet.near" {
		t.Fatalf("wrong wallets %+v", ws)
	}
	for _, bad := range []string{`{}`, `{"EVM":"a","Near":"b"}`, `{"EVM":1}`, `"EVM"`} {
		var w Wallet
		if err := json.Unmarshal([]byte(bad), &w); err == nil {
			t.Fatalf("wallet %s accepted", bad)
		}
	}
}

func testKey(t *testing.T) *Key {
	t.Helper()
	seed := make([]byte, 32)
	seed[0] = 1
	k, err := DeriveKey(DerivationV1, seed)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	t.Cleanup(k.Zero)
	return k
}

func TestSignRecoverPersonal(t *testing.T) {
	k := testKey(t)
	msg := []byte(`{"nonce":"1"}`)
	sig, err := k.signPersonal(msg)
	if err != nil {
		t.Fatalf("signPersonal: %v", err)
	}
	if len(sig) != 130 || strings.ToLower(sig) != sig || strings.HasPrefix(sig, "0x") {
		t.Fatalf("signature encoding %q", sig)
	}
	if v := sig[128:]; v != "1b" && v != "1c" {
		t.Fatalf("recovery byte %s, want 1b or 1c", v)
	}
	id, err := RecoverPersonal(msg, sig)
	if err != nil || id != k.Identity() {
		t.Fatalf("RecoverPersonal = %s, %v; want %s", id, err, k.Identity())
	}
	if id, _ := RecoverPersonal([]byte(`{"nonce":"2"}`), sig); id == k.Identity() {
		t.Fatalf("signature recovered for a different message")
	}

	raw, _ := hex.DecodeString(sig)
	raw[64] -= 27 // raw 0/1 recovery byte
	if _, err := RecoverPersonal(msg, hex.EncodeToString(raw)); err == nil {
		t.Fatalf("raw recovery byte accepted")
	}
	for _, bad := range []string{"", sig[:128], sig + "00", "zz" + sig[2:]} {
		if _, err := RecoverPersonal(msg, bad); err == nil {
			t.Fatalf("signature %q accepted", bad)
		}
	}

	csig, err := k.SignCreation()
	if err != nil {
		t.Fatalf("SignCreation: %v", err)
	}
	if id, err := RecoverPersonal(CreationMessage(k.Identity()), csig); err != nil || id != k.Identity() {
		t.Fatalf("creation signature recovers %s, %v", id, err)
	}
}
