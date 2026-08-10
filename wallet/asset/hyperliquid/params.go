// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/bisoncraft/caribou/util"
)

const (
	BipID            = 10001
	DefaultFee       = 0
	MaxBlockInterval = 45 // seconds before considered unsynced; must exceed wsPingInterval+wsWriteDeadline
	keyFileName      = "hl-keyfile.json"

	// atomsPerHYPE is UnitInfo's ConversionFactor as an integer constant, for
	// the overflow bounds in parseHYPE.
	atomsPerHYPE = 100_000_000

	// pendingTTL is how long a pending send may wait for confirmation before
	// it is considered permanently unconfirmed and removed from the DB.
	// Hyperliquid finalizes in ~1 s; 120 s allows for WS reconnect lag.
	pendingTTL = 120

	// echoMatchWindowMS bounds how far a pending send's nonce may sit from a
	// ledger echo's timestamp for confirmPendingSend to pair the two. Both are
	// absolute times — the nonce is minted from our clock, u.Time comes from
	// the ledger — so the gap is however long the server took to process the
	// send, plus clock skew. It does not grow with delivery lag: an echo the
	// REST catch-up replays hours later still carries its original ledger
	// timestamp.
	//
	// A send cannot take longer than httpTimeout to be accepted, and a record
	// that has waited pendingTTL is expired anyway, so this only has to exclude
	// the wildly implausible — an echo from months back claiming an in-flight
	// send, which Rescan replays whole history through.
	echoMatchWindowMS = pendingTTL * 1000

	// echoSkewAllowanceMS is how far past a record's OutcomeUnknownMS a ledger
	// echo may still be attributed to it. That timestamp is an upper bound on
	// when the send could have been processed, so in principle the allowance is
	// zero; it exists only to absorb skew between our clock and ledger time.
	//
	// It has to stay below the gap between a failed send and the user's retry,
	// which is what it is being used to distinguish, so it is kept small. Skew
	// worse than this is already self-limiting: Hyperliquid rejects a nonce
	// more than a day out, so a clock wrong enough to matter cannot send at
	// all. Past that the pairing just degrades to plain FIFO.
	echoSkewAllowanceMS = 1_000
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "atoms",
		Conventional: util.Denomination{
			Unit:             "HYPE",
			ConversionFactor: 1e8,
		},
	}

	// DefaultAPIEndpoints maps network to the Hyperliquid REST API base URL.
	DefaultAPIEndpoints = map[util.Network]string{
		util.Mainnet: "https://api.hyperliquid.xyz",
		util.Testnet: "https://api.hyperliquid-testnet.xyz",
	}

	// DefaultWSEndpoints maps network to the Hyperliquid WebSocket URL.
	DefaultWSEndpoints = map[util.Network]string{
		util.Mainnet: "wss://api.hyperliquid.xyz/ws",
		util.Testnet: "wss://api.hyperliquid-testnet.xyz/ws",
	}

	// eip712ChainNames maps network to the hyperliquidChain string used in
	// EIP-712 spot send messages.
	eip712ChainNames = map[util.Network]string{
		util.Mainnet: "Mainnet",
		util.Testnet: "Testnet",
	}

	// eip712ChainIDs maps network to the chain ID used in the EIP-712 domain
	// separator for Hyperliquid user-signed actions. Testnet uses Arbitrum
	// Sepolia (0x66eee = 421614); mainnet uses Arbitrum One (0xa4b1 = 42161).
	eip712ChainIDs = map[util.Network]uint64{
		util.Mainnet: 0xa4b1,
		util.Testnet: 0x66eee,
	}
)

// atomsToHYPE converts microHYPE atoms to a decimal HYPE string for the
// Hyperliquid API. Trailing zeros after the decimal point are stripped.
func atomsToHYPE(atoms uint64) string {
	whole := atoms / 1e8
	frac := atoms % 1e8
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	fracStr := fmt.Sprintf("%08d", frac)
	fracStr = strings.TrimRight(fracStr, "0")
	return fmt.Sprintf("%d.%s", whole, fracStr)
}

// parseHYPE parses a decimal HYPE string from the Hyperliquid API into
// microHYPE atoms. Sub-atom precision is truncated.
func parseHYPE(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid HYPE amount %q: %w", s, err)
	}
	// The API is untrusted input, so reject values that do not fit rather
	// than wrapping: scaling by 1e8 overflows uint64 far below ParseUint's
	// own limit, and a wrapped value reads as a plausible small amount in
	// Balance().Available and in ledger amounts.
	if whole > math.MaxUint64/atomsPerHYPE {
		return 0, fmt.Errorf("HYPE amount %q overflows uint64 atoms", s)
	}
	atoms := whole * 1e8
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 8 {
			frac = frac[:8]
		}
		for len(frac) < 8 {
			frac += "0"
		}
		fracVal, err := strconv.ParseUint(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid HYPE decimal in %q: %w", s, err)
		}
		if fracVal > math.MaxUint64-atoms {
			return 0, fmt.Errorf("HYPE amount %q overflows uint64 atoms", s)
		}
		atoms += fracVal
	}
	return atoms, nil
}
