// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

const (
	// createTimeout bounds Create within the context it is given. The
	// birthday search and the account's tree state each try the servers in
	// turn, at up to 30 seconds a call, and a Tor proxy adds to that.
	createTimeout = 5 * time.Minute
	// birthdayMargin is how far before the seed's creation time the
	// birthday search looks. A day covers block times, a wrong clock, and a
	// seed paid before core recorded its time.
	birthdayMargin = 24 * time.Hour
)

// Create creates the wallet: the zeclib database with ZIP 32 account 0 of
// the seed, then the encrypted seed, whose file marks a complete wallet. A
// Create interrupted before the seed is saved leaves a database without it,
// which the next Create deletes.
//
// The account is always created as a restore, from a birthday found a day
// before the seed's creation time: a seed's age does not prove it was never
// paid, and a birthday cannot be lowered later. A seed made now scans the
// last day of blocks. A zero creation time begins at the activation of
// NU6.3, before which no account can hold Ironwood funds.
func (d *Driver) Create(ctx context.Context, p *asset.CreateWalletParams) (err error) {
	defer func() {
		log := p.Logger
		if log == nil {
			log = util.Disabled
		}
		err = explain(log, err)
	}()
	if p.Type != walletTypeNative {
		return fmt.Errorf("unknown wallet type %q", p.Type)
	}
	if len(p.Seed) != entropyLen {
		return fmt.Errorf("the seed is %d bytes, want %d", len(p.Seed), entropyLen)
	}
	netCfg, err := configFor(p.Net, p.Settings, p.TorProxy)
	if err != nil {
		return err
	}
	seed, err := zip32Seed(p.Seed)
	if err != nil {
		return err
	}
	defer encode.ClearBytes(seed)

	store := newSeedStore(p.DataDir)
	if err := os.MkdirAll(p.DataDir, 0700); err != nil {
		return fmt.Errorf("creating the data directory: %w", err)
	}
	if exists, err := store.exists(); err != nil || exists {
		if err == nil {
			err = fmt.Errorf("%w in %s", errWalletExists, p.DataDir)
		}
		return err
	}
	dbPath := filepath.Join(p.DataDir, dbFileName)
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing what an earlier attempt left: %w", err)
		}
	}

	log := p.Logger
	if log == nil {
		log = util.Disabled
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	be, err := openBackend(netCfg.zeclibConfig(dbPath, log), nil)
	if err != nil {
		return err
	}
	account, err := createAccount(ctx, be, p, seed)
	if closeErr := be.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("closing the wallet database: %w", closeErr)
	}
	if err != nil {
		return err
	}
	return store.save(p.Seed, account.SeedFingerprint, p.Pass)
}

// createAccount finds the birthday and creates ZIP 32 account 0 of the seed.
func createAccount(ctx context.Context, be backend, p *asset.CreateWalletParams, seed []byte) (*zeclib.Account, error) {
	var birthday uint32
	switch {
	case p.Net == util.Simnet:
		birthday = simnetBirthday
	case p.Birthday == 0:
		// Raised to the activation of NU6.3. Zero would select a height
		// near the tip.
		birthday = 1
	default:
		h, err := be.HeightForTime(ctx, time.Unix(int64(p.Birthday), 0), birthdayMargin)
		if err != nil {
			return nil, fmt.Errorf("finding the birthday height: %w", err)
		}
		birthday = h
	}
	account, err := be.CreateAccount(ctx, &zeclib.AccountParams{
		Name:     "account 0",
		Seed:     seed,
		Birthday: birthday,
		Restore:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("creating the account: %w", err)
	}
	if !account.Derived || account.HDIndex != 0 {
		return nil, fmt.Errorf("the new account is not ZIP 32 account 0 of the seed")
	}
	return account, nil
}
