package waddrmgr

import (
	"encoding/hex"
	"sync"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
)

// expectedRootKey derives path from the test seed's root key without going
// through the manager, as the reference for DeriveFromRootPath.
func expectedRootKey(t *testing.T, path []uint32) []byte {
	t.Helper()
	key := rootKey
	for _, idx := range path {
		child, err := key.Derive(idx)
		if err != nil {
			t.Fatalf("derive %d: %v", idx, err)
		}
		key = child
	}
	priv, err := key.ECPrivKey()
	if err != nil {
		t.Fatalf("ECPrivKey: %v", err)
	}
	return priv.Serialize()
}

// TestDeriveFromRootPath checks that keys on paths outside any scope, such as
// DIP-9 m/9'/coin'/3'/feature'/index, match a plain BIP-32 derivation of the
// root, that the hardened bit on each level is honored, and that a locked
// manager refuses to derive.
func TestDeriveFromRootPath(t *testing.T) {
	teardown, db, mgr := setupManager(t)
	defer teardown()

	const h = hdkeychain.HardenedKeyStart
	dip9 := []uint32{9 + h, 5 + h, 3 + h, 2 + h, 0}

	derive := func(path []uint32) ([]byte, error) {
		var out []byte
		err := walletdb.View(db, func(tx walletdb.ReadTx) error {
			ns := tx.ReadBucket(waddrmgrNamespaceKey)
			priv, err := mgr.DeriveFromRootPath(ns, path)
			if err != nil {
				return err
			}
			out = priv.Serialize()
			return nil
		})
		return out, err
	}

	// Locked managers must not expose root-derived keys.
	_, err := derive(dip9)
	checkLockedRefusal(t, "locked", err)

	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		return mgr.Unlock(tx.ReadBucket(waddrmgrNamespaceKey), privPassphrase)
	})
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}

	got, err := derive(dip9)
	if err != nil {
		t.Fatalf("derive dip9 path: %v", err)
	}
	if string(got) != string(expectedRootKey(t, dip9)) {
		t.Fatal("dip9 path key does not match BIP-32 derivation of the root")
	}

	// The same indexes without the hardened bit are a different key. This
	// fails if the sub-feature level were silently derived non-hardened.
	nonHardened := []uint32{9, 5, 3, 2, 0}
	got2, err := derive(nonHardened)
	if err != nil {
		t.Fatalf("derive non-hardened path: %v", err)
	}
	if string(got2) == string(got) {
		t.Fatal("hardened and non-hardened paths derived the same key")
	}
	if string(got2) != string(expectedRootKey(t, nonHardened)) {
		t.Fatal("non-hardened path key does not match BIP-32 derivation")
	}

	// Distinct indexes give distinct keys.
	next := []uint32{9 + h, 5 + h, 3 + h, 2 + h, 1}
	got3, err := derive(next)
	if err != nil {
		t.Fatalf("derive next index: %v", err)
	}
	if string(got3) == string(got) {
		t.Fatal("different indexes derived the same key")
	}

	// Locking again must refuse again.
	if err := mgr.Lock(); err != nil {
		t.Fatalf("lock: %v", err)
	}
	_, err = derive(dip9)
	checkLockedRefusal(t, "relocked", err)

	// An empty path would hand out the master root secret.
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		return mgr.Unlock(tx.ReadBucket(waddrmgrNamespaceKey), privPassphrase)
	})
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	_, err = derive(nil)
	checkManagerError(t, "empty path", err, ErrKeyChain)

	// A watch-only manager has no root secret to derive from.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		return mgr.ConvertToWatchingOnly(tx.ReadWriteBucket(waddrmgrNamespaceKey))
	})
	if err != nil {
		t.Fatalf("convert to watching only: %v", err)
	}
	_, err = derive(dip9)
	checkManagerError(t, "watch only", err, ErrWatchingOnly)
}

// checkLockedRefusal requires the explicit locked-manager error. A locked
// manager also fails to decrypt the root (its crypto key is zeroed), which
// carries ErrLocked too, so the description is checked to tell the two apart.
func checkLockedRefusal(t *testing.T, name string, err error) {
	t.Helper()
	if !checkManagerError(t, name, err, ErrLocked) {
		return
	}
	if desc := err.(ManagerError).Description; desc != errLocked {
		t.Fatalf("%s: refused for %q, want the locked check (%q)", name,
			desc, errLocked)
	}
}

// TestDeriveFromRootPathVector pins the derivation to BIP-32 test vectors, so
// a library quirk shared with the reference derivation in the other test can't
// hide a wrong key. Vector 4 exists for the leading-zero bug of
// hdkeychain.DeriveNonStandard: its m/0'/1' key has an intermediate key with a
// leading zero byte, which vector 1 and the Dash Core paths never hit.
//
// This swaps the package-level rootKey, so it must not run in parallel.
func TestDeriveFromRootPathVector(t *testing.T) {
	const h = hdkeychain.HardenedKeyStart
	tests := []struct {
		name string
		seed string
		path []uint32
		xprv string
	}{
		{"vector 1 m/0'", "000102030405060708090a0b0c0d0e0f", []uint32{h},
			"xprv9uHRZZhk6KAJC1avXpDAp4MDc3sQKNxDiPvvkX8Br5ngLNv1TxvUxt4cV1rGL5hj6KCesnDYUhd7oWgT11eZG7XnxHrnYeSvkzY7d2bhkJ7"},
		{"vector 4 m/0'/1'", "3ddd5602285899a946114506157c7997e5444528f3003f6134712147db19b678", []uint32{h, h + 1},
			"xprv9xJocDuwtYCMNAo3Zw76WENQeAS6WGXQ55RCy7tDJ8oALr4FWkuVoHJeHVAcAqiZLE7Je3vZJHxspZdFHfnBEjHqU5hG1Jaj32dVoS6XLT1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vectorSeed, err := hex.DecodeString(tc.seed)
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			master, err := hdkeychain.NewMaster(vectorSeed, &chaincfg.MainNetParams)
			if err != nil {
				t.Fatalf("master: %v", err)
			}
			want, err := hdkeychain.NewKeyFromString(tc.xprv)
			if err != nil {
				t.Fatalf("vector xprv: %v", err)
			}
			wantPriv, err := want.ECPrivKey()
			if err != nil {
				t.Fatalf("vector key: %v", err)
			}

			// setupManager creates the wallet from the package rootKey.
			orig := rootKey
			rootKey = master
			defer func() { rootKey = orig }()

			teardown, db, mgr := setupManager(t)
			defer teardown()

			var got []byte
			err = walletdb.View(db, func(tx walletdb.ReadTx) error {
				ns := tx.ReadBucket(waddrmgrNamespaceKey)
				if err := mgr.Unlock(ns, privPassphrase); err != nil {
					return err
				}
				priv, err := mgr.DeriveFromRootPath(ns, tc.path)
				if err != nil {
					return err
				}
				got = priv.Serialize()
				return nil
			})
			if err != nil {
				t.Fatalf("derive: %v", err)
			}
			if string(got) != string(wantPriv.Serialize()) {
				t.Fatal("derived key does not match the BIP-32 test vector")
			}
		})
	}
}

// TestDeriveFromRootPathNeuteredRoot checks that a manager whose root private
// key was removed refuses to derive. This is distinct from a watch-only
// manager, which is refused before the root is looked up.
func TestDeriveFromRootPathNeuteredRoot(t *testing.T) {
	teardown, db, mgr := setupManager(t)
	defer teardown()

	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		if err := mgr.Unlock(ns, privPassphrase); err != nil {
			return err
		}
		return mgr.NeuterRootKey(ns)
	})
	if err != nil {
		t.Fatalf("neuter root key: %v", err)
	}

	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		_, err := mgr.DeriveFromRootPath(
			tx.ReadBucket(waddrmgrNamespaceKey), []uint32{hdkeychain.HardenedKeyStart},
		)
		return err
	})
	checkManagerError(t, "neutered root", err, ErrWatchingOnly)
}

// TestDeriveFromRootPathConcurrentLock runs derivations while the manager is
// repeatedly locked and unlocked. Lock zeroes the crypto key that the
// derivation decrypts the root with, so a derivation must either fail as
// locked or return the correct key, never a wrong key or another error. Run
// with -race to catch unsynchronized access.
func TestDeriveFromRootPathConcurrentLock(t *testing.T) {
	teardown, db, mgr := setupManager(t)
	defer teardown()

	const h = hdkeychain.HardenedKeyStart
	path := []uint32{9 + h, 5 + h, 3 + h, 2 + h, 0}
	want := expectedRootKey(t, path)

	unlock := func() {
		_ = walletdb.View(db, func(tx walletdb.ReadTx) error {
			return mgr.Unlock(tx.ReadBucket(waddrmgrNamespaceKey), privPassphrase)
		})
	}
	unlock()

	const iterations = 200
	var wg sync.WaitGroup
	errs := make(chan string, 4*iterations)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mgr.Lock()
			unlock()
		}
	}()

	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				err := walletdb.View(db, func(tx walletdb.ReadTx) error {
					ns := tx.ReadBucket(waddrmgrNamespaceKey)
					priv, err := mgr.DeriveFromRootPath(ns, path)
					if err != nil {
						return err
					}
					if string(priv.Serialize()) != string(want) {
						errs <- "derived a wrong key while locking"
					}
					return nil
				})
				if err == nil {
					continue
				}
				// Require the explicit locked check: a failed decrypt
				// also carries ErrLocked, but only the check holding
				// the read lock can be relied on without -race.
				merr, ok := err.(ManagerError)
				if !ok || merr.ErrorCode != ErrLocked || merr.Description != errLocked {
					errs <- "unexpected error: " + err.Error()
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

// TestDeriveFromRootPathDashCoreVector checks DIP-9 masternode paths against
// Dash Core v23.1.4's own BIP-32 implementation. The addresses were produced
// by deriveaddresses on pkh(<tprv of the test seed>/<path>) on regtest, so
// the expected values do not come from this repo's derivation code. Only the
// hash160 is compared since the address version byte depends on the network.
func TestDeriveFromRootPathDashCoreVector(t *testing.T) {
	teardown, db, mgr := setupManager(t)
	defer teardown()

	const h = hdkeychain.HardenedKeyStart
	tests := []struct {
		name string
		path []uint32
		addr string
	}{
		{"owner m/9'/5'/3'/2'/0", []uint32{9 + h, 5 + h, 3 + h, 2 + h, 0},
			"yXfHkawavvqer22RVZbvrtpUCrfqTtfFWs"},
		{"owner m/9'/5'/3'/2'/1", []uint32{9 + h, 5 + h, 3 + h, 2 + h, 1},
			"yQjGQLJG7ru4BTbCexkHKWD8QMadCzbb63"},
		{"voting testnet m/9'/1'/3'/1'/0", []uint32{9 + h, 1 + h, 3 + h, 1 + h, 0},
			"yi98gPF7oLhcqT6zagsWTKBjJ94EbknS84"},
	}

	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		if err := mgr.Unlock(ns, privPassphrase); err != nil {
			return err
		}
		for _, tc := range tests {
			priv, err := mgr.DeriveFromRootPath(ns, tc.path)
			if err != nil {
				return err
			}
			want, _, err := base58.CheckDecode(tc.addr)
			if err != nil {
				t.Fatalf("%s: decode address: %v", tc.name, err)
			}
			got := btcutil.Hash160(priv.PubKey().SerializeCompressed())
			if hex.EncodeToString(got) != hex.EncodeToString(want) {
				t.Errorf("%s: key hash160 %x, Dash Core derived %x",
					tc.name, got, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
