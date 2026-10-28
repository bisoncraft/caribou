package masternode

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/assets"
	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	_ "github.com/bisoncraft/caribou/wallet/utxowallet/walletdb/bdb"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
)

var (
	testSeed = bytes.Repeat([]byte{0x2a}, hdkeychain.MinSeedBytes)
	pubPass  = []byte("public")
	privPass = []byte("private")
)

// pathRecorder records the path it is asked to derive.
type pathRecorder struct{ path []uint32 }

func (r *pathRecorder) DeriveFromRootPath(path []uint32) (*btcec.PrivateKey, error) {
	r.path = path
	return nil, errors.New("recorded")
}

// newTestWallet creates an unlocked Dash wallet from seed. The key values
// don't depend on the network, only the coin type used in the path does.
func newTestWallet(t *testing.T, params *netparams.ChainParams, seed []byte) *wallet.Wallet {
	t.Helper()
	loader := wallet.NewLoader(params, t.TempDir(), true, 10*time.Second, 250,
		wallet.WithScryptOptions(&waddrmgr.FastScryptOptions))
	w, err := loader.CreateNewWallet(pubPass, privPass, seed, time.Now())
	if err != nil {
		t.Fatalf("create wallet: %v", err)
	}
	t.Cleanup(func() {
		if err := loader.UnloadWallet(); err != nil {
			t.Errorf("unload wallet: %v", err)
		}
	})
	if err := w.Unlock(privPass, nil); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	return w
}

// refKey derives m/9'/coin'/3'/subFeature'/n with hdkeychain directly.
func refKey(t *testing.T, seed []byte, coinType, subFeature, n uint32) *btcec.PrivateKey {
	t.Helper()
	k, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []uint32{
		hardened + 9, hardened + coinType, hardened + 3, hardened + subFeature, n,
	} {
		if k, err = k.Derive(idx); err != nil {
			t.Fatal(err)
		}
	}
	priv, err := k.ECPrivKey()
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// TestKeyPaths pins the exact DIP-9 path, including which levels are
// hardened. A non-hardened sub-feature or a hardened index would derive a
// different, wrong key family.
func TestKeyPaths(t *testing.T) {
	const h = hardened
	tests := []struct {
		name string
		fn   func(KeyDeriver, uint32, uint32) (*btcec.PrivateKey, error)
		want []uint32
	}{
		{"owner", OwnerKey, []uint32{h + 9, h + 5, h + 3, h + 2, 7}},
		{"voting", VotingKey, []uint32{h + 9, h + 5, h + 3, h + 1, 7}},
	}
	for _, tt := range tests {
		r := &pathRecorder{}
		_, _ = tt.fn(r, 5, 7)
		if len(r.path) != len(tt.want) {
			t.Fatalf("%s: path %v, want %v", tt.name, r.path, tt.want)
		}
		for i := range tt.want {
			if r.path[i] != tt.want[i] {
				t.Fatalf("%s: path %v, want %v", tt.name, r.path, tt.want)
			}
		}
	}
}

func TestIndexAndCoinTypeBounds(t *testing.T) {
	r := &pathRecorder{}
	if _, err := OwnerKey(r, 5, hardened); err == nil || r.path != nil {
		t.Fatalf("hardened index accepted: err=%v path=%v", err, r.path)
	}
	if _, err := VotingKey(r, hardened, 0); err == nil || r.path != nil {
		t.Fatalf("overflowing coin type accepted: err=%v path=%v", err, r.path)
	}
	// The largest valid values still reach the deriver.
	if _, err := OwnerKey(r, hardened-1, hardened-1); err == nil ||
		err.Error() != "recorded" {
		t.Fatalf("max values rejected: %v", err)
	}
}

// TestKeysThroughWallet derives through a real wallet created from a seed and
// compares with an independent hdkeychain derivation, for the mainnet and
// testnet coin types.
func TestKeysThroughWallet(t *testing.T) {
	for _, net := range []string{"mainnet", "testnet"} {
		params := assets.DashParams[net]
		w := newTestWallet(t, params, testSeed)
		coin := params.HDCoinType
		for n := uint32(0); n < 3; n++ {
			owner, err := OwnerKey(w, coin, n)
			if err != nil {
				t.Fatalf("%s owner %d: %v", net, n, err)
			}
			if !owner.Key.Equals(&refKey(t, testSeed, coin, 2, n).Key) {
				t.Fatalf("%s owner %d differs from reference", net, n)
			}
			voting, err := VotingKey(w, coin, n)
			if err != nil {
				t.Fatalf("%s voting %d: %v", net, n, err)
			}
			if !voting.Key.Equals(&refKey(t, testSeed, coin, 1, n).Key) {
				t.Fatalf("%s voting %d differs from reference", net, n)
			}
			if owner.Key.Equals(&voting.Key) {
				t.Fatalf("%s: owner and voting %d are the same key", net, n)
			}
		}
	}
	if assets.DashParams["mainnet"].HDCoinType == assets.DashParams["testnet"].HDCoinType {
		t.Fatal("mainnet and testnet share a coin type")
	}
}

// TestKeysRestoreFromSeed checks that a wallet recreated from the same seed in
// a fresh database yields the same keys, and a different seed does not.
func TestKeysRestoreFromSeed(t *testing.T) {
	params := assets.DashParams["mainnet"]
	coin := params.HDCoinType
	orig := newTestWallet(t, params, testSeed)
	restored := newTestWallet(t, params, testSeed)
	other := newTestWallet(t, params, bytes.Repeat([]byte{0x2b}, hdkeychain.MinSeedBytes))

	a, err := OwnerKey(orig, coin, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OwnerKey(restored, coin, 4)
	if err != nil {
		t.Fatal(err)
	}
	c, err := OwnerKey(other, coin, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Key.Equals(&b.Key) {
		t.Fatal("restored wallet derived a different owner key")
	}
	if a.Key.Equals(&c.Key) {
		t.Fatal("different seeds derived the same owner key")
	}
}

func TestKeysLockedWallet(t *testing.T) {
	params := assets.DashParams["mainnet"]
	w := newTestWallet(t, params, testSeed)
	// Lock is processed asynchronously by the wallet's locker goroutine.
	w.Lock()
	deadline := time.Now().Add(5 * time.Second)
	for !w.Locked() {
		if time.Now().After(deadline) {
			t.Fatal("wallet did not lock")
		}
		time.Sleep(time.Millisecond)
	}
	_, err := OwnerKey(w, params.HDCoinType, 0)
	var merr waddrmgr.ManagerError
	if !errors.As(err, &merr) || merr.ErrorCode != waddrmgr.ErrLocked {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

// TestKeysDashCoreVector checks the owner and voting keys against Dash Core
// v23.1.4's own BIP-32 implementation. The addresses were produced by
// deriveaddresses on pkh(<tprv of testSeed>/<path>) on regtest, so the
// expected values do not come from this repo's code or its reading of DIP-9.
// Only the hash160 is compared since the address version byte depends on the
// network.
func TestKeysDashCoreVector(t *testing.T) {
	tests := []struct {
		name  string
		net   string
		owner bool
		n     uint32
		addr  string
	}{
		{"owner m/9'/5'/3'/2'/0", "mainnet", true, 0, "ycJTQCtTVNT7Dm8R1PEAWs22xoMqp2oFrd"},
		{"owner m/9'/5'/3'/2'/1", "mainnet", true, 1, "yh1BvvAtX94hMvH5wkiUdm5qi5g8wkVkDS"},
		{"voting m/9'/5'/3'/1'/0", "mainnet", false, 0, "yebgKqjBXXJmgc7kQ5r4XrCRrh1JKES8NN"},
		{"owner m/9'/1'/3'/2'/0", "testnet", true, 0, "ybgbRV5FgERAL2JGdvbFd5Cqv3MU9ruNti"},
		{"voting m/9'/1'/3'/1'/7", "testnet", false, 7, "yThD15sAJZYDD8HHG61yE1fq31nYTEd5cJ"},
	}
	wallets := make(map[string]*wallet.Wallet)
	for _, tc := range tests {
		params := assets.DashParams[tc.net]
		w, ok := wallets[tc.net]
		if !ok {
			w = newTestWallet(t, params, testSeed)
			wallets[tc.net] = w
		}
		derive := VotingKey
		if tc.owner {
			derive = OwnerKey
		}
		priv, err := derive(w, params.HDCoinType, tc.n)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want, _, err := base58.CheckDecode(tc.addr)
		if err != nil {
			t.Fatalf("%s: decode address: %v", tc.name, err)
		}
		got := btcutil.Hash160(priv.PubKey().SerializeCompressed())
		if !bytes.Equal(got, want) {
			t.Errorf("%s: key hash160 %x, Dash Core derived %x", tc.name, got, want)
		}
	}
}
