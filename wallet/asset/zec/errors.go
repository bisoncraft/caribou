// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib"
)

var (
	// errSeedDecrypt is the failure to decrypt the stored seed of a seed
	// file that parses: a wrong password, or a secret that is damaged,
	// absent or too short. The keystore does not tell these apart.
	errSeedDecrypt = errors.New("the seed could not be decrypted")
	// errSeedFile is a seed file that is missing, cannot be read, or does not
	// parse. The error of the file system or the parser stays in the chain.
	errSeedFile = errors.New("the seed file is missing or damaged")
	// errBadAddress is an address that does not decode for the network.
	errBadAddress = errors.New("the address does not decode")
	// errWalletExists is a Create over a complete wallet.
	errWalletExists = errors.New("a Zcash wallet already exists")
)

// userError is an error whose message is the wallet's own, written for the
// user. Core and the appserver show the innermost error of a chain
// (core.UnwrapErr), so userError has no Unwrap: the chain stops at it and the
// user reads its message, not a library's. Is and As consult the cause, so
// errors.Is and errors.As against zeclib's and this package's errors keep
// working for Go callers.
type userError struct {
	msg   string
	cause error
}

func (e *userError) Error() string { return e.msg }

// Is reports whether the cause matches target.
func (e *userError) Is(target error) bool { return errors.Is(e.cause, target) }

// As finds the first error in the cause's chain that matches target.
func (e *userError) As(target any) bool { return errors.As(e.cause, target) }

// explain gives an error a user can reach the wallet's message. Every
// exported method a user reaches through the API returns its error through
// it, in one deferred call. nil stays nil, and an error explained already
// stays as it is. When the message differs from the error's own text, the
// error is logged at Debug, once, when it is explained: core logs only the
// message.
func explain(log util.Logger, err error) error {
	if err == nil {
		return nil
	}
	if ue, is := err.(*userError); is {
		return ue
	}
	ue := &userError{msg: describe(err), cause: err}
	if ue.msg != err.Error() {
		log.Debugf("%s; the error: %v", ue.msg, err)
	}
	return ue
}

// explain is explain with the wallet's logger.
func (w *zecWallet) explain(err error) error {
	return explain(w.log, err)
}

// shortAddress abbreviates an address for a log line: its first ten
// characters and an ellipsis.
func shortAddress(address string) string {
	if len(address) <= 10 {
		return address
	}
	return address[:10] + "…"
}

// zecAmount formats zatoshis as ZEC, without trailing zeros, in integer
// arithmetic: a float64 does not hold every amount to eight decimals near the
// supply limit.
func zecAmount(zats uint64) string {
	const zatsPerZEC = 100_000_000
	s := strconv.FormatUint(zats/zatsPerZEC, 10)
	if frac := zats % zatsPerZEC; frac != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%08d", frac), "0")
	}
	return s + " ZEC"
}

// plain is an error's text without zeclib's prefixes.
func plain(err error) string {
	return strings.ReplaceAll(err.Error(), "zeclib: ", "")
}

// withReason appends what an error says beyond its sentinel's own words to
// msg, if it says anything more.
func withReason(msg string, err error, sentinel string) string {
	text := plain(err)
	if !strings.HasPrefix(text, sentinel) {
		return msg + ": " + text
	}
	if r := strings.TrimPrefix(strings.TrimPrefix(text, sentinel), ": "); r != "" {
		return msg + ": " + r
	}
	return msg
}

// describe writes the user's message for an error.
func describe(err error) string {
	var funds *zeclib.InsufficientFundsError
	var minimum *zeclib.AmountBelowMinimumError
	switch {
	case errors.As(err, &funds):
		return fmt.Sprintf("insufficient funds: %s available, %s required with the fee",
			zecAmount(funds.Available), zecAmount(funds.Required))
	case errors.As(err, &minimum):
		return fmt.Sprintf("the amount is below the smallest payment to a transparent address, %d zats", minimum.Minimum)
	case errors.Is(err, errNotSynced), errors.Is(err, zeclib.ErrNotSynced):
		return "the Zcash wallet is still syncing; try again once it has caught up with the chain"
	case errors.Is(err, errLocked):
		return "the Zcash wallet is locked"
	case errors.Is(err, errClosed), errors.Is(err, zeclib.ErrClosed):
		return "the Zcash wallet is not open"
	case errors.Is(err, errSeedDecrypt):
		return "the Zcash wallet's seed could not be decrypted: the password is wrong or the seed file is damaged"
	case errors.Is(err, errSeedFile):
		return "the Zcash wallet's seed file is missing or damaged"
	case errors.Is(err, errWalletExists):
		return "a Zcash wallet already exists"
	case errors.Is(err, zeclib.ErrSeedRequired):
		return "the Zcash wallet's database needs its seed for an upgrade; open it with the wallet password"
	case errors.Is(err, zeclib.ErrFeeTooHigh), errors.Is(err, zeclib.ErrAmountChanged):
		return "the wallet's spendable funds changed since the fee was estimated; try again"
	case errors.Is(err, zeclib.ErrTooLarge):
		return "the payment needs more inputs than one transaction can hold; send a smaller amount first"
	case errors.Is(err, zeclib.ErrAddressExpired):
		// zeclib reports an address whose expiry has passed and one that
		// expires before the transaction would in the same error, told apart
		// only in its free text.
		return "the address has expired, or expires too soon for this payment; ask the recipient for a current one"
	case errors.Is(err, zeclib.ErrUnsupportedRecipient):
		return withReason("this wallet cannot pay that address", err, "unsupported recipient")
	case errors.Is(err, errNotPayable), errors.Is(err, errBalanceChanged), errors.Is(err, errLeftBehind):
		return plain(err)
	case errors.Is(err, zeclib.ErrSeedMismatch):
		return "internal error: the seed does not belong to the wallet's account"
	case errors.Is(err, errBadAddress):
		return withReason("not a valid Zcash address for this network", err, errBadAddress.Error()+": invalid argument")
	case errors.Is(err, zeclib.ErrInvalidArgument):
		return withReason("invalid input", err, "invalid argument")
	case errors.Is(err, zeclib.ErrServerUnreachable):
		return "no Zcash server could be reached; check the network connection and the lightwalletd server setting"
	case errors.Is(err, zeclib.ErrWrongNetwork):
		return "the Zcash server is on another network; check the lightwalletd server setting"
	case errors.Is(err, zeclib.ErrConsensusBranch):
		return "the Zcash server follows other consensus rules; check the lightwalletd server setting"
	case errors.Is(err, zeclib.ErrServerStale):
		return "the Zcash server is behind the chain; check the lightwalletd server setting"
	case errors.Is(err, zeclib.ErrServerFailure):
		return withReason("the Zcash server failed", err, "the server failed")
	default:
		return plain(err)
	}
}
