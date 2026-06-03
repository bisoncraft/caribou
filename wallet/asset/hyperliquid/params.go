// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bisoncraft/bosswallet/util"
)

const (
	BipID            = 10001
	DefaultFee       = 0
	MaxBlockInterval = 45 // seconds before considered unsynced; must exceed wsPingInterval+wsWriteDeadline
	keyFileName      = "hl-keyfile.json"

	// pendingTTL is how long a pending send may wait for confirmation before
	// it is considered permanently unconfirmed and removed from the DB.
	// Hyperliquid finalizes in ~1 s; 30 s is very conservative.
	pendingTTL = 30
)

var (
	UnitInfo = util.UnitInfo{
		AtomicUnit: "microHYPE",
		Conventional: util.Denomination{
			Unit:             "HYPE",
			ConversionFactor: 1e8,
		},
	}

	// DefaultAPIEndpoints maps network to the Hyperliquid REST API base URL.
	DefaultAPIEndpoints = map[util.Network]string{
		util.Mainnet: "https://api.hyperliquid.xyz",
		util.Testnet: "https://api.hyperliquid-testnet.xyz",
		util.Simnet:  "https://api.hyperliquid-testnet.xyz",
	}

	// DefaultWSEndpoints maps network to the Hyperliquid WebSocket URL.
	DefaultWSEndpoints = map[util.Network]string{
		util.Mainnet: "wss://api.hyperliquid.xyz/ws",
		util.Testnet: "wss://api.hyperliquid-testnet.xyz/ws",
		util.Simnet:  "wss://api.hyperliquid-testnet.xyz/ws",
	}

	// eip712ChainNames maps network to the hyperliquidChain string used in
	// EIP-712 spot send messages.
	eip712ChainNames = map[util.Network]string{
		util.Mainnet: "Mainnet",
		util.Testnet: "Testnet",
		util.Simnet:  "Testnet",
	}

	// eip712ChainID is the chain ID in the EIP-712 domain separator.
	// Hyperliquid uses 998 for both mainnet and testnet.
	eip712ChainID = uint64(998)
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
		atoms += fracVal
	}
	return atoms, nil
}
