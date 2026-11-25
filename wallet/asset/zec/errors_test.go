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
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

// shown is the text the user sees for an error: core wraps it, and the
// appserver shows core.UnwrapErr(err), the innermost error of the chain by
// errors.Unwrap. The function is core's, repeated here.
func shown(err error) string {
	err = fmt.Errorf("core: %w", err)
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err.Error()
		}
		err = inner
	}
}

// removeSeed deletes the seed file of the wallet in dir.
func removeSeed(t *testing.T, dir string) {
	if err := os.Remove(filepath.Join(dir, seedFileName)); err != nil {
		t.Fatal(err)
	}
}

// garbleSeed replaces the seed file of the wallet in dir with text that does
// not parse.
func garbleSeed(t *testing.T, dir string) {
	if err := os.WriteFile(filepath.Join(dir, seedFileName), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
}

// writeSeed replaces the seed file with content.
func writeSeed(content string) func(*testing.T, string) {
	return func(t *testing.T, dir string) {
		if err := os.WriteFile(filepath.Join(dir, seedFileName), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// seedCase reads the seed through Unlock, or through an OpenWithPW whose
// database needs the seed (migration), after damage changes the seed file,
// with a wrong password or the right one.
func seedCase(migration bool, damage func(*testing.T, string), wrongPW bool) func(*testing.T) error {
	return func(t *testing.T) error {
		e := newEnv(t)
		if damage != nil {
			damage(t, e.dir)
		}
		pw := testPW
		if wrongPW {
			pw = []byte(strings.Repeat("x", 32))
		}
		if migration {
			e.o.needSeed = true
			return e.w.OpenWithPW(context.Background(), pw)
		}
		return e.w.Unlock(pw)
	}
}

// Every failure a user can reach through the API shows the wallet's message,
// without zeclib's words, and still matches its sentinels for Go callers.
func TestUserFacingErrors(t *testing.T) {
	const other = "ufake-other"
	sendWith := func(cause error) func(*testing.T) error {
		return func(t *testing.T) error {
			e := sendEnv(t)
			e.c.sendErr = cause
			_, err := e.w.Send(other, 1_000, feeRate)
			return err
		}
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T) error
		want string
		is   error
		// as, if set, is a pointer to a typed error errors.As must find.
		as any
	}{{
		name: "Send: insufficient funds",
		run:  sendWith(&zeclib.InsufficientFundsError{Available: 150_000_000, Required: 250_010_000}),
		want: "insufficient funds: 1.5 ZEC available, 2.5001 ZEC required with the fee",
		is:   zeclib.ErrInsufficientFunds,
		as:   new(*zeclib.InsufficientFundsError),
	}, {
		name: "Send: below the transparent minimum",
		run:  sendWith(&zeclib.AmountBelowMinimumError{Minimum: 54}),
		want: "the amount is below the smallest payment to a transparent address, 54 zats",
		is:   zeclib.ErrAmountBelowMinimum,
		as:   new(*zeclib.AmountBelowMinimumError),
	}, {
		name: "Send: the inputs changed",
		run:  sendWith(&zeclib.FeeTooHighError{Fee: 20_000, MaxFee: 10_000}),
		want: "the wallet's spendable funds changed since the fee was estimated; try again",
		is:   zeclib.ErrFeeTooHigh,
		as:   new(*zeclib.FeeTooHighError),
	}, {
		name: "Send: the amount changed, wrapped by zeclib",
		run:  sendWith(fmt.Errorf("%w: the plan pays 900 zats, 1000 asked", zeclib.ErrAmountChanged)),
		want: "the wallet's spendable funds changed since the fee was estimated; try again",
		is:   zeclib.ErrAmountChanged,
	}, {
		name: "Send: too large",
		run:  sendWith(fmt.Errorf("%w: 2000 inputs", zeclib.ErrTooLarge)),
		want: "the payment needs more inputs than one transaction can hold; send a smaller amount first",
		is:   zeclib.ErrTooLarge,
	}, {
		name: "Send: expired address",
		run:  sendWith(fmt.Errorf("%w: the address expired at height 400, and the chain is at 401", zeclib.ErrAddressExpired)),
		want: "the address has expired, or expires too soon for this payment; ask the recipient for a current one",
		is:   zeclib.ErrAddressExpired,
	}, {
		name: "Send: an address expiring before the transaction would",
		run: sendWith(fmt.Errorf("%w: the address expires at height 420, before the transaction would at 440",
			zeclib.ErrAddressExpired)),
		want: "the address has expired, or expires too soon for this payment; ask the recipient for a current one",
		is:   zeclib.ErrAddressExpired,
	}, {
		name: "Send: not an address",
		run: func(t *testing.T) error {
			e := sendEnv(t)
			_, err := e.w.Send("garbage", 1_000, feeRate)
			return err
		},
		want: "not a valid Zcash address for this network",
		is:   errBadAddress,
	}, {
		name: "Send: an invalid amount",
		run:  sendWith(fmt.Errorf("%w: the amount is zero", zeclib.ErrInvalidArgument)),
		want: "invalid input: the amount is zero",
		is:   zeclib.ErrInvalidArgument,
	}, {
		name: "Send: unsupported recipient",
		run:  sendWith(fmt.Errorf("%w: a Sapling address", zeclib.ErrUnsupportedRecipient)),
		want: "this wallet cannot pay that address: a Sapling address",
		is:   zeclib.ErrUnsupportedRecipient,
	}, {
		name: "Send: not synced",
		run: func(t *testing.T) error {
			e := sendEnv(t)
			e.setStatus(zeclib.SyncSyncing, 300)
			_, err := e.w.Send(other, 1_000, feeRate)
			return err
		},
		want: "the Zcash wallet is still syncing; try again once it has caught up with the chain",
		is:   errNotSynced,
	}, {
		name: "Send: zeclib not synced, wrapped by zeclib",
		run:  sendWith(fmt.Errorf("%w: the scan is 12 blocks behind the tip", zeclib.ErrNotSynced)),
		want: "the Zcash wallet is still syncing; try again once it has caught up with the chain",
		is:   zeclib.ErrNotSynced,
	}, {
		name: "Send: locked",
		run: func(t *testing.T) error {
			e := openTxEnv(t, 300)
			_, err := e.w.Send(other, 1_000, feeRate)
			return err
		},
		want: "the Zcash wallet is locked",
		is:   errLocked,
	}, {
		name: "Withdraw: the balance changed",
		run: func(t *testing.T) error {
			e := sendEnv(t)
			e.setAvailable(100_000)
			e.c.sendMax = &zeclib.SendMaxEstimate{Amount: 80_000, Fee: 15_000}
			_, err := e.w.Withdraw(other, 100_000, feeRate)
			return err
		},
		want: "the balance changed; refresh and try again",
		is:   errBalanceChanged,
	}, {
		name: "Withdraw: more than one transaction",
		run: func(t *testing.T) error {
			e := sendEnv(t)
			e.setAvailable(100_000)
			e.c.sendMax = &zeclib.SendMaxEstimate{Amount: 80_000, Fee: 15_000,
				LeftBehind: zeclib.LeftBehind{TransparentCount: 1, TransparentValue: 5_000}}
			_, err := e.w.Withdraw(other, 100_000, feeRate)
			return err
		},
		want: "the balance needs more than one transaction to send; withdraw a smaller amount first",
		is:   errLeftBehind,
	}, {
		name: "EstimateSendTxFee: insufficient funds",
		run: func(t *testing.T) error {
			e := sendEnv(t)
			e.c.estimate = func(uint64) (*zeclib.TransferEstimate, error) {
				return nil, &zeclib.InsufficientFundsError{Available: 5_000, Required: 15_000}
			}
			_, _, err := e.w.EstimateSendTxFee(other, 5_000, feeRate, false, false)
			return err
		},
		want: "insufficient funds: 0.00005 ZEC available, 0.00015 ZEC required with the fee",
		is:   zeclib.ErrInsufficientFunds,
		as:   new(*zeclib.InsufficientFundsError),
	}, {
		name: "NewAddress: closed",
		run: func(t *testing.T) error {
			e := newEnv(t)
			_, err := e.w.NewAddress()
			return err
		},
		want: "the Zcash wallet is not open",
		is:   errClosed,
	}, {
		name: "AddressReceivers: a Sapling address",
		run: func(t *testing.T) error {
			e := newEnv(t)
			_, err := e.w.AddressReceivers("zfake-sapling")
			return err
		},
		want: "not an address this wallet pays: Sapling",
		is:   errNotPayable,
	}, {
		name: "AddressReceivers: not an address",
		run: func(t *testing.T) error {
			e := newEnv(t)
			_, err := e.w.AddressReceivers("garbage")
			return err
		},
		want: "not a valid Zcash address for this network",
		is:   zeclib.ErrInvalidArgument,
	}, {
		name: "OwnsDepositAddress: closed",
		run: func(t *testing.T) error {
			e := newEnv(t)
			_, err := e.w.OwnsDepositAddress(testAddress)
			return err
		},
		want: "the Zcash wallet is not open",
		is:   errClosed,
	}, {
		name: "AddressUsed: closed",
		run: func(t *testing.T) error {
			e := newEnv(t)
			_, err := e.w.AddressUsed(testAddress)
			return err
		},
		want: "the Zcash wallet is not open",
		is:   errClosed,
	}, {
		name: "AddressUsed: not an address",
		run: func(t *testing.T) error {
			e := newEnv(t)
			_, err := e.w.AddressUsed("garbage")
			return err
		},
		want: "not a valid Zcash address for this network",
		is:   errBadAddress,
	}, {
		name: "Unlock: wrong password",
		run:  seedCase(false, nil, true),
		want: "the Zcash wallet's seed could not be decrypted: the password is wrong or the seed file is damaged",
		is:   errSeedDecrypt,
	}, {
		name: "Unlock: no seed file",
		run:  seedCase(false, removeSeed, false),
		want: "the Zcash wallet's seed file is missing or damaged",
		is:   fs.ErrNotExist,
	}, {
		name: "Unlock: a malformed seed file",
		run:  seedCase(false, garbleSeed, false),
		want: "the Zcash wallet's seed file is missing or damaged",
		is:   errSeedFile,
	}, {
		name: "Unlock: a seed file without a secret",
		run:  seedCase(false, writeSeed(`{"pubKey":"0102","secret":""}`), false),
		want: "the Zcash wallet's seed could not be decrypted: the password is wrong or the seed file is damaged",
		is:   errSeedDecrypt,
	}, {
		name: "Unlock: a seed file with a short secret",
		run:  seedCase(false, writeSeed(`{"pubKey":"0102","secret":"00112233"}`), false),
		want: "the Zcash wallet's seed could not be decrypted: the password is wrong or the seed file is damaged",
		is:   errSeedDecrypt,
	}, {
		name: "OpenWithPW for a migration: wrong password",
		run:  seedCase(true, nil, true),
		want: "the Zcash wallet's seed could not be decrypted: the password is wrong or the seed file is damaged",
		is:   errSeedDecrypt,
	}, {
		name: "OpenWithPW for a migration: no seed file",
		run:  seedCase(true, removeSeed, false),
		want: "the Zcash wallet's seed file is missing or damaged",
		is:   fs.ErrNotExist,
	}, {
		name: "OpenWithPW for a migration: a malformed seed file",
		run:  seedCase(true, garbleSeed, false),
		want: "the Zcash wallet's seed file is missing or damaged",
		is:   errSeedFile,
	}, {
		name: "Connect: the database needs the seed",
		run: func(t *testing.T) error {
			e := newEnv(t)
			e.o.needSeed = true
			_, err := e.w.Connect(context.Background())
			return err
		},
		want: "the Zcash wallet's database needs its seed for an upgrade; open it with the wallet password",
		is:   zeclib.ErrSeedRequired,
	}, {
		name: "Open (reconfigure): a malformed server setting",
		run: func(t *testing.T) error {
			_, err := (&Driver{}).Open(&asset.WalletConfig{Type: walletTypeNative, DataDir: t.TempDir(),
				Settings: map[string]string{settingServer: "no-port"}}, nil, util.Mainnet)
			return err
		},
		want: `the lightwalletd server "no-port" is not host:port`,
	}, {
		name: "Create: no server reachable",
		run: func(t *testing.T) error {
			installOpener(t)
			prev := openBackend
			openBackend = func(cfg *zeclib.Config, seed []byte) (backend, error) {
				be, err := prev(cfg, seed)
				if err == nil {
					be.(*fakeBackend).heightForTimeErr = fmt.Errorf("%w: connection refused", zeclib.ErrServerUnreachable)
				}
				return be, err
			}
			return (&Driver{}).Create(t.Context(), createParams(t.TempDir(), util.Mainnet, 1_800_000_000))
		},
		want: "no Zcash server could be reached; check the network connection and the lightwalletd server setting",
		is:   zeclib.ErrServerUnreachable,
	}, {
		name: "Create: a wallet exists",
		run: func(t *testing.T) error {
			e := newEnv(t)
			return (&Driver{}).Create(t.Context(), createParams(e.dir, util.Simnet, 0))
		},
		want: "a Zcash wallet already exists",
		is:   errWalletExists,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatal("no error")
			}
			got := shown(err)
			if got != tc.want {
				t.Errorf("shown %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "zeclib") {
				t.Errorf("shown %q names zeclib", got)
			}
			// Go callers match the wallet's error, and the error as core
			// wraps it.
			for _, e := range []error{err, fmt.Errorf("core: %w", err)} {
				if tc.is != nil && !errors.Is(e, tc.is) {
					t.Errorf("errors.Is(%q, %v) is false", e, tc.is)
				}
				if tc.as != nil && !errors.As(e, tc.as) {
					t.Errorf("errors.As(%q, %T) is false", e, tc.as)
				}
			}
		})
	}
	// errors.As still finds zeclib's typed errors.
	e := sendEnv(t)
	e.c.sendErr = &zeclib.InsufficientFundsError{Available: 1, Required: 2}
	_, err := e.w.Send(other, 1_000, feeRate)
	var funds *zeclib.InsufficientFundsError
	if !errors.As(err, &funds) || funds.Required != 2 {
		t.Errorf("errors.As: %v", err)
	}
}

// A missing seed file is errSeedFile and fs.ErrNotExist at once.
func TestMissingSeedFile(t *testing.T) {
	err := seedCase(false, removeSeed, false)(t)
	if !errors.Is(err, errSeedFile) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing seed file: %v", err)
	}
}

// explain logs the error at Debug once, when the message the user sees
// differs from the error's own text, and not at all when they are the same.
func TestExplainLogsTheCause(t *testing.T) {
	e := newEnv(t)
	err := e.w.Unlock([]byte(strings.Repeat("x", 32)))
	// A second explain of the same error logs nothing more.
	_ = e.w.explain(err)
	if n := e.log.count("the error: the seed could not be decrypted"); n != 1 {
		t.Errorf("the cause was logged %d times, want once", n)
	}
	if n := e.log.count("[DBG]"); n != 1 {
		t.Errorf("%d debug lines, want 1: %q", n, e.log.lines)
	}
	if _, err := e.w.AddressReceivers("zfake-sapling"); err == nil {
		t.Fatal("no error")
	}
	if n := e.log.count("[DBG]"); n != 1 {
		t.Errorf("an error shown as its own text was logged: %q", e.log.lines)
	}
}

// zecAmount is exact to the zatoshi up to the supply limit.
func TestZecAmount(t *testing.T) {
	for zats, want := range map[uint64]string{
		0:                     "0 ZEC",
		1:                     "0.00000001 ZEC",
		100_000_000:           "1 ZEC",
		150_000_000:           "1.5 ZEC",
		2_099_999_999_999_999: "20999999.99999999 ZEC",
	} {
		if got := zecAmount(zats); got != want {
			t.Errorf("zecAmount(%d) = %q, want %q", zats, got, want)
		}
	}
}
