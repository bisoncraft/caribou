// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// The ZIP 32 seed is the BIP 39 seed of the entropy's 24-word phrase with an
// empty passphrase. The expected seeds were computed with PBKDF2-HMAC-SHA512
// outside Go.
func TestSeedDerivation(t *testing.T) {
	for _, tc := range []struct {
		entropy byte
		phrase  string
		seedHex string
	}{
		{0x00, "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon " +
			"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art",
			"408b285c123836004f4b8842c89324c1f01382450c0d439af345ba7fc49acf705489c6fc77dbd4e3dc1dd8cc6bc9f043db8ada1e243c4a0eafb290d399480840"},
		{0x7f, "legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful " +
			"legal winner thank year wave sausage worth title",
			"761914478ebf6fe16185749372e91549361af22b386de46322cf8b1ba7e92e80c4af05196f742be1e63aab603899842ddadf4e7248d8e43870a4b6ff9bf16324"},
	} {
		entropy := bytes.Repeat([]byte{tc.entropy}, 32)
		phrase, err := mnemonicFor(entropy)
		if err != nil || phrase != tc.phrase {
			t.Errorf("phrase of %02x: %q, %v", tc.entropy, phrase, err)
		}
		seed, err := zip32Seed(entropy)
		if err != nil || hex.EncodeToString(seed) != tc.seedHex {
			t.Errorf("seed of %02x: %x, %v", tc.entropy, seed, err)
		}
	}
}

func createParams(dir string, net util.Network, birthday uint64) *asset.CreateWalletParams {
	return &asset.CreateWalletParams{
		Type: walletTypeNative, Seed: testEntropy, Pass: testPW, DataDir: dir, Net: net, Birthday: birthday,
	}
}

// The account is created as a restore, from the height HeightForTime finds
// a day before the seed's time, whatever the seed's age. A zero time begins
// at NU6.3, and simnet at its fixed birthday.
func TestCreateBirthday(t *testing.T) {
	seedTime := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name      string
		net       util.Network
		birthday  uint64
		asked     bool
		wantStart uint32
	}{
		{"a seed made now", util.Mainnet, uint64(seedTime.Unix()), true, 3_000_000},
		{"an old seed", util.Testnet, uint64(seedTime.Add(-365 * 24 * time.Hour).Unix()), true, 3_000_000},
		{"no time", util.Mainnet, 0, false, 1},
		{"simnet", util.Simnet, uint64(seedTime.Unix()), false, simnetBirthday},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := installOpener(t)
			o.mtx.Lock()
			o.backends[filepath.Join(t.TempDir(), "unused")] = nil
			o.mtx.Unlock()
			dir := t.TempDir()
			// The fake answers HeightForTime with 3,000,000.
			prev := openBackend
			openBackend = func(cfg *zeclib.Config, seed []byte) (backend, error) {
				be, err := prev(cfg, seed)
				if err == nil {
					be.(*fakeBackend).heightForTime = 3_000_000
				}
				return be, err
			}
			if err := (&Driver{}).Create(createParams(dir, tc.net, tc.birthday)); err != nil {
				t.Fatalf("Create: %v", err)
			}
			b := o.latest()
			if len(b.createParams) != 1 {
				t.Fatalf("%d accounts created", len(b.createParams))
			}
			p := b.createParams[0]
			if !p.Restore || p.Birthday != tc.wantStart || !bytes.Equal(p.Seed, testZip32) {
				t.Errorf("account params: restore %t, birthday %d, seed %x", p.Restore, p.Birthday, p.Seed)
			}
			if tc.asked != (len(b.birthdayAsked) == 1) {
				t.Fatalf("HeightForTime asked %d times", len(b.birthdayAsked))
			}
			if tc.asked && (b.birthdayAsked[0].Unix() != int64(tc.birthday) || b.marginAsked[0] != 24*time.Hour) {
				t.Errorf("HeightForTime(%v, %v)", b.birthdayAsked[0], b.marginAsked[0])
			}
			if !b.isClosed() {
				t.Error("the database was left open")
			}
			fp, err := newSeedStore(dir).fingerprint()
			if err != nil || !bytes.Equal(fp, []byte{0xfe}) {
				t.Errorf("the stored fingerprint is not the account's: %x, %v", fp, err)
			}
			entropy, err := newSeedStore(dir).load(testPW)
			if err != nil || !bytes.Equal(entropy, testEntropy) {
				t.Errorf("the stored seed: %x, %v", entropy, err)
			}
		})
	}
}

// The seed file is written last, so an interrupted Create leaves no wallet,
// and the next Create starts over from what it left.
func TestCreateFileOrder(t *testing.T) {
	o := installOpener(t)
	dir := t.TempDir()
	d := &Driver{}
	prev := openBackend
	failing := errors.New("the server is down")
	openBackend = func(cfg *zeclib.Config, seed []byte) (backend, error) {
		be, err := prev(cfg, seed)
		if err == nil {
			be.(*fakeBackend).createErr = failing
		}
		return be, err
	}
	if err := d.Create(createParams(dir, util.Simnet, 0)); !errors.Is(err, failing) {
		t.Fatalf("Create with a failing account: %v", err)
	}
	if exists, err := d.Exists(walletTypeNative, dir, nil, util.Simnet); err != nil || exists {
		t.Fatalf("Exists after a failed Create: %t, %v", exists, err)
	}
	if !o.latest().isClosed() {
		t.Error("the database was left open")
	}
	// What a database leaves beside it is removed.
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := os.WriteFile(filepath.Join(dir, dbFileName+suffix), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	openBackend = prev
	if err := d.Create(createParams(dir, util.Simnet, 0)); err != nil {
		t.Fatalf("Create over the leftovers: %v", err)
	}
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, dbFileName+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was not removed: %v", dbFileName+suffix, err)
		}
	}
	if exists, err := d.Exists(walletTypeNative, dir, nil, util.Simnet); err != nil || !exists {
		t.Fatalf("Exists: %t, %v", exists, err)
	}
	if err := d.Create(createParams(dir, util.Simnet, 0)); err == nil {
		t.Error("Create over an existing wallet succeeded")
	}
}

func TestCreateArguments(t *testing.T) {
	installOpener(t)
	d := &Driver{}
	p := createParams(t.TempDir(), util.Simnet, 0)
	p.Type = "rpc"
	if err := d.Create(p); err == nil {
		t.Error("an unknown type")
	}
	p = createParams(t.TempDir(), util.Simnet, 0)
	p.Seed = make([]byte, 64)
	if err := d.Create(p); err == nil {
		t.Error("a 64-byte seed")
	}
	p = createParams(t.TempDir(), util.Simnet, 0)
	p.Pass = []byte("short")
	if err := d.Create(p); err == nil {
		t.Error("a short password")
	}
	if _, err := d.Exists("rpc", t.TempDir(), nil, util.Simnet); err == nil {
		t.Error("Exists of an unknown type")
	}
}

func TestDriver(t *testing.T) {
	d := &Driver{}
	wire := make([]byte, 32)
	wire[0] = 0xab
	id, err := d.DecodeCoinID(wire)
	if err != nil || id != "00000000000000000000000000000000000000000000000000000000000000ab" {
		t.Errorf("DecodeCoinID: %s, %v", id, err)
	}
	if _, err := d.DecodeCoinID([]byte{1}); err == nil {
		t.Error("a short coin ID")
	}
	if info := d.Info(); info.Name != "Zcash" || info.UnitInfo.Conventional.ConversionFactor != 1e8 ||
		info.MaxFeeRate != feeRate || info.Network.ID != BipID {
		t.Errorf("Info: %+v", info)
	}
}
