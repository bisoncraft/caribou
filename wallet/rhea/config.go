// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package rhea is the protocol layer for RHEA (Burrow) lending through
// RHEA's cross-chain multichain accounts (MCAs). An MCA is a NEAR account
// controlled by external-chain identities (here, an EVM key) through
// EIP-191 signatures over JSON "businesses" that the MCA executes. Supply
// reaches an MCA through 1Click with a custom recipient message; withdrawal
// is a signed business submitted through RHEA's relayer.
//
// The package builds, validates, and signs the only businesses Caribou
// issues, builds supply recipient messages, decodes contract views and
// transaction receipt trees, and talks to the relayer API. It holds no
// state and has no wallet or Core dependencies.
package rhea

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Market is a lending market Caribou supports.
type Market struct {
	// Symbol is a display symbol.
	Symbol string
	// TokenID is the NEP-141 contract of the market token, which is also
	// Burrow's asset ID.
	TokenID string
	// OneClickAssetID is the 1Click asset that means this token held on
	// NEAR. For tokens the 1Click catalog lists under another chain (ZEC),
	// this is the catalog's NEAR-side entry, not the token's nep141 ID.
	OneClickAssetID string
	// FiatTicker is the normalized ticker (core's ticker groups, as in the
	// wallet's ticker rates) whose USD rate prices one whole market token.
	// Rate sources price by ticker, so the token's chain does not matter.
	FiatTicker string
}

// Deployment names the contracts and services of a RHEA lending deployment.
type Deployment struct {
	// AccountManager registers identities, creates MCAs, and holds the
	// supply templates.
	AccountManager string
	// Burrow is the lending contract.
	Burrow string
	// Relayer is the account that submits signed businesses and collects
	// the relayer fee (the relayer config's COLLECT_FEE_ACCOUNT_ID).
	Relayer string
	// RelayerAPI is the base URL of the relayer HTTP API.
	RelayerAPI string
	// MCASuffix is the account suffix of every MCA the AccountManager
	// creates.
	MCASuffix string
	// WrappedNEAR is the NEP-141 wrapped NEAR token, whose base units are
	// yoctoNEAR.
	WrappedNEAR string
	// Markets is the market allowlist.
	Markets []*Market
}

// Mainnet is the production deployment. Contract versions at the time of
// writing (2026-11-17): account manager 0.3.4, MCA 0.1.6, Burrow 0.16.2.
var Mainnet = &Deployment{
	AccountManager: "multica.near",
	Burrow:         "contract.main.burrow.near",
	Relayer:        "mca-relayer.rhealab.near",
	RelayerAPI:     "https://api.rhea.finance",
	MCASuffix:      ".multica.near",
	WrappedNEAR:    "wrap.near",
	Markets: []*Market{
		{
			Symbol:          "USDC",
			TokenID:         "17208628f84f5d6ad33f0da3bbbeb27ffcb398eac501a31bd6ad2011e36133a1",
			OneClickAssetID: "nep141:17208628f84f5d6ad33f0da3bbbeb27ffcb398eac501a31bd6ad2011e36133a1",
			FiatTicker:      "USDC",
		},
		{
			Symbol:          "USDT",
			TokenID:         "usdt.tether-token.near",
			OneClickAssetID: "nep141:usdt.tether-token.near",
			FiatTicker:      "USDT",
		},
		{
			Symbol:          "NEAR",
			TokenID:         "wrap.near",
			OneClickAssetID: "nep141:wrap.near",
			FiatTicker:      "NEAR",
		},
		{
			// The catalog's nep141:zec.omft.near is ZEC on the Zcash
			// chain; this is the same token delivered on NEAR.
			Symbol:          "ZEC",
			TokenID:         "zec.omft.near",
			OneClickAssetID: "1cs_v1:near:nep141:zec.omft.near",
			FiatTicker:      "ZEC",
		},
	},
}

// Simnet is the deployment the simnet mock (util/testing/one-click)
// simulates. Its contracts and tokens exist only inside the mock, so their
// names cannot collide with the NEAR sandbox or with mainnet. RelayerAPI is
// the mock's default address.
var Simnet = &Deployment{
	AccountManager: "multica.test.near",
	Burrow:         "burrow.test.near",
	Relayer:        "relayer.test.near",
	RelayerAPI:     "http://127.0.0.1:17781/rhea/relayer",
	MCASuffix:      ".multica.test.near",
	WrappedNEAR:    "wrap.near",
	Markets: []*Market{
		{Symbol: "USDC", TokenID: "usdc.test.near", OneClickAssetID: "nep141:usdc.test.near", FiatTicker: "USDC"},
		{Symbol: "USDT", TokenID: "usdt.test.near", OneClickAssetID: "nep141:usdt.test.near", FiatTicker: "USDT"},
	},
}

// Market returns the allowlisted market for a token, or nil.
func (d *Deployment) Market(tokenID string) *Market {
	for _, m := range d.Markets {
		if m.TokenID == tokenID {
			return m
		}
	}
	return nil
}

// IsMCA reports whether accountID is an MCA of this deployment: a single
// label under MCASuffix.
func (d *Deployment) IsMCA(accountID string) bool {
	label, ok := strings.CutSuffix(accountID, d.MCASuffix)
	return ok && label != "" && !strings.Contains(label, ".") && validAccountID(accountID)
}

// Business constants. Gas values match businesses the deployed RHEA
// frontend has executed successfully (for example exec 4SDDqzRj…, a
// supply-only withdrawal), which use 120 Tgas for Burrow execute where the
// published SDK still uses 150 Tgas. The ft_transfer_call gas matches the
// account manager's own BurrowSupply template.
const (
	// StorageDeposit is the NEP-145 storage deposit Caribou attaches when
	// registering an account with a token, and the relayer's
	// attach_deposit, in yoctoNEAR (0.00125 NEAR).
	StorageDeposit = "1250000000000000000000"

	GasSimpleWithdraw = "100000000000000"
	GasExecute        = "120000000000000"
	GasStorageDeposit = "10000000000000"
	GasFTTransfer     = "10000000000000"
	GasFTTransferCall = "50000000000000"

	// OneYocto is the deposit NEP-141 and Burrow require on calls that
	// move funds.
	OneYocto = "1"

	// TransferIntervalBlock delays the transfer to a 1Click deposit
	// address by this many blocks after the preceding request, as the
	// RHEA frontend does, so the withdrawal settles first.
	TransferIntervalBlock = 2
)

// Signing windows. A business deadline is computed from the timestamp of
// the latest final block, never from the local clock.
const (
	// DeadlineWindow is how far past the reference block time Caribou sets
	// a business deadline.
	DeadlineWindow = 5 * time.Minute
	// MaxDeadlineWindow is the longest deadline, past the reference block
	// time, the validator accepts.
	MaxDeadlineWindow = 10 * time.Minute
	// MaxClockSkew is the largest difference between the local clock and
	// the reference block time at which signing is allowed.
	MaxClockSkew = 2 * time.Minute
)

// Supply template names. Supply runs an account-manager template chosen by
// the recipient message, without a signature from the identity.
const (
	TemplateBurrowSupply            = "BurrowSupply"
	TemplateBurrowRegisterAndSupply = "BurrowRegisterAndSupply"
)

// pinnedTemplates are the mainnet supply templates as returned by
// get_reliable_business_template_paged on 2026-11-17 (block 225016693).
// Both supply with msg "" (plain supply, never collateral). The account
// manager's governance can change templates without notice, so they are
// compared byte for byte before any supply is quoted.
var pinnedTemplates = map[string]string{
	TemplateBurrowSupply: `[{"receiver_id":"$TOKEN_ID_FLAG$","function_calls":[{"method_name":"ft_transfer_call",` +
		`"args":"{\"amount\":\"$AMOUNT_FLAG$\",\"msg\":\"\",\"receiver_id\":\"contract.main.burrow.near\"}",` +
		`"gas":"50000000000000","deposit":"1"}]}]`,
	TemplateBurrowRegisterAndSupply: `[{"receiver_id":"contract.main.burrow.near","function_calls":[{"method_name":"storage_deposit",` +
		`"args":"{\"registration_only\":true}","gas":"10000000000000","deposit":"100000000000000000000000"}]},` +
		`{"receiver_id":"$TOKEN_ID_FLAG$","function_calls":[{"method_name":"ft_transfer_call",` +
		`"args":"{\"amount\":\"$AMOUNT_FLAG$\",\"msg\":\"\",\"receiver_id\":\"contract.main.burrow.near\"}",` +
		`"gas":"50000000000000","deposit":"1"}]}]`,
}

// PinnedTemplate returns the expected supply template for this deployment:
// the pinned mainnet template with this deployment's Burrow account.
func (d *Deployment) PinnedTemplate(name string) (string, bool) {
	t, ok := pinnedTemplates[name]
	if !ok {
		return "", false
	}
	return strings.ReplaceAll(t, Mainnet.Burrow, d.Burrow), true
}

// CheckTemplates verifies the supply templates in a
// get_reliable_business_template_paged result against the pinned
// templates. A missing or altered supply template fails.
func (d *Deployment) CheckTemplates(result []byte) error {
	var templates map[string]string
	if err := json.Unmarshal(result, &templates); err != nil {
		return fmt.Errorf("error decoding templates: %w", err)
	}
	for _, name := range []string{TemplateBurrowSupply, TemplateBurrowRegisterAndSupply} {
		want, _ := d.PinnedTemplate(name)
		got, ok := templates[name]
		if !ok {
			return fmt.Errorf("template %s missing", name)
		}
		if got != want {
			return fmt.Errorf("template %s differs from the pinned template", name)
		}
	}
	return nil
}

// validAccountID reports whether s is a valid NEAR account ID: 2 to 64
// characters of lowercase alphanumerics separated by single '.', '-', or
// '_' characters. Implicit (64 hex) accounts satisfy it.
func validAccountID(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	prevSep := true // no leading separator
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			prevSep = false
		case c == '.' || c == '-' || c == '_':
			if prevSep {
				return false
			}
			prevSep = true
		default:
			return false
		}
	}
	return !prevSep
}

// isImplicitAccount reports whether s is a NEAR implicit account: 64
// lowercase hex characters. 1Click NEAR deposit addresses are implicit
// accounts.
func isImplicitAccount(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
