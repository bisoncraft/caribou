// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bisoncraft/meshwallet/wallet/asset"
	"github.com/bisoncraft/meshwallet/wallet/db"
	"github.com/bisoncraft/meshwallet/util"
	"github.com/decred/dcrd/hdkeychain/v3"
)

const (
	// hdKeyPurposeMulti is the BIP-43 purpose field for multisig keys.
	hdKeyPurposeMulti uint32 = hdkeychain.HardenedKeyStart + 0x6D756C74 // ASCII "mult"
)

// errorSet is a slice of orders with a prefix prepended to the Error output.
type errorSet struct {
	prefix string
	errs   []error
}

// newErrorSet constructs an error set with a prefix.
func newErrorSet(s string, a ...any) *errorSet {
	return &errorSet{prefix: fmt.Sprintf(s, a...)}
}

// add adds the message to the slice as an error and returns the errorSet.
func (set *errorSet) add(s string, a ...any) *errorSet {
	set.errs = append(set.errs, fmt.Errorf(s, a...))
	return set
}

// addErr adds the error to the set.
func (set *errorSet) addErr(err error) *errorSet {
	set.errs = append(set.errs, err)
	return set
}

// ifAny returns the error set if there are any errors, else nil.
func (set *errorSet) ifAny() error {
	if len(set.errs) > 0 {
		return set
	}
	return nil
}

// Error satisfies the error interface. Error strings are concatenated using a
// ", " and prepended with the prefix.
func (set *errorSet) Error() string {
	errStrings := make([]string, 0, len(set.errs))
	for i := range set.errs {
		errStrings = append(errStrings, set.errs[i].Error())
	}
	return set.prefix + "{" + strings.Join(errStrings, ", ") + "}"
}

// WalletForm is information necessary to create a new exchange wallet.
// The ConfigText, if provided, will be parsed for wallet connection settings.
type WalletForm struct {
	AssetID uint32
	Config  map[string]string
	Type    string
}

// WalletBalance is an exchange wallet's balance which includes various locked
// amounts in addition to other balance details stored in db. ContractLocked is
// not included in the Locked field of the embedded asset.Balance since it
// corresponds to outputs that are foreign to the wallet, i.e. only spendable
// by externally-crafted transactions.
type WalletBalance struct {
	*db.Balance
	// ContractLocked is the total amount of funds locked in unspent (i.e.
	// unredeemed / unrefunded) swap contracts. This amount is NOT included in
	// the db.Balance.
	ContractLocked uint64 `json:"contractlocked"`
}

// WalletState is the current status of an exchange wallet.
type WalletState struct {
	Symbol         string                              `json:"symbol"`
	AssetID        uint32                              `json:"assetID"`
	Version        uint32                              `json:"version"`
	WalletType     string                              `json:"type"`
	Class          asset.BlockchainClass               `json:"class"`
	Traits         asset.WalletTrait                   `json:"traits"`
	Open           bool                                `json:"open"`
	Running        bool                                `json:"running"`
	Balance        *WalletBalance                      `json:"balance"`
	Address        string                              `json:"address"`
	Encrypted      bool                                `json:"encrypted"`
	PeerCount      uint32                              `json:"peerCount"`
	Synced         bool                                `json:"synced"`
	SyncProgress   float32                             `json:"syncProgress"`
	SyncStatus     *asset.SyncStatus                   `json:"syncStatus"`
	Disabled       bool                                `json:"disabled"`
	Approved       map[uint32]asset.ApprovalStatus     `json:"approved"`
	BridgeApproved map[string]asset.ApprovalStatus     `json:"bridgeApproved"`
	FeeState       *FeeState                           `json:"feeState"`
	PendingTxs     map[string]*asset.WalletTransaction `json:"pendingTxs"`
}

// FeeState is information about the current network transaction fees and
// estimates of standard operations.
type FeeState struct {
	Rate    uint64 `json:"rate"`
	Send    uint64 `json:"send"`
	StampMS int64  `json:"stampMS"`
}

// BridgeFeesAndLimits contains the fees and limits for a bridge operation.
type BridgeFeesAndLimits struct {
	Fees      map[uint32]uint64 `json:"fees"`
	MinLimit  uint64            `json:"minLimit"`
	MaxLimit  uint64            `json:"maxLimit"`
	HasLimits bool              `json:"hasLimits"`
}

// ContractGasTestResult is the per-asset result of a contract gas test.
type ContractGasTestResult = asset.GasTestResult

// DeployContractResult is the per-chain result of a contract deployment.
type DeployContractResult struct {
	AssetID      uint32 `json:"assetID"`
	Symbol       string `json:"symbol"`
	ContractAddr string `json:"contractAddr,omitempty"`
	TxID         string `json:"txID,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ExtensionModeConfig is configuration for running core in extension mode,
// primarily for restricting certain wallet reconfiguration options.
type ExtensionModeConfig struct {
	// Name of embedding application. Used for messaging with disable features.
	Name string `json:"name"`
	// UseDEXBranding will tell the front end to use legacy DCRDEX branding
	// instead of Bison Wallet branding where possible.
	UseDEXBranding bool `json:"useDEXBranding"`
	// RestrictedWallets are wallets that need restrictions on reconfiguration
	// options.
	RestrictedWallets map[string] /*symbol*/ struct {
		// HiddenFields are configuration fields (asset.ConfigOption.Key) that
		// should not be displayed to the user.
		HiddenFields []string `json:"hiddenFields"`
		// DisableWalletType indicates that we should not offer the user an
		// an option to change the wallet type.
		DisableWalletType bool `json:"disableWalletType"`
		// DisablePassword indicates that we should not offer the user an option
		// to change the wallet password.
		DisablePassword bool `json:"disablePassword"`
		// DisableStaking disables vsp configuration and ticket purchasing.
		DisableStaking bool `json:"disableStaking"`
		// DisablePrivacy disables mixing configuration and control.
		DisablePrivacy bool `json:"disablePrivacy"`
	} `json:"restrictedWallets"`
}

// User is information about the user's wallets.
type User struct {
	Initialized        bool                        `json:"inited"`
	SeedGenerationTime uint64                      `json:"seedgentime"`
	Assets             map[uint32]*SupportedAsset  `json:"assets"`
	FiatRates          map[uint32]float64          `json:"fiatRates"`
	Net                util.Network                 `json:"net"`
	ExtensionConfig    *ExtensionModeConfig        `json:"extensionModeConfig,omitempty"`
	Actions            []*asset.ActionRequiredNote `json:"actions,omitempty"`
}

// SupportedAsset is data about an asset and possibly the wallet associated
// with it.
type SupportedAsset struct {
	ID     uint32       `json:"id"`
	Symbol string       `json:"symbol"`
	Name   string       `json:"name"`
	Wallet *WalletState `json:"wallet"`
	// Info is only populated for base chain assets. One of either Info or
	// Token will be populated.
	Info *asset.WalletInfo `json:"info"`
	// Token is only populated for token assets.
	Token    *asset.Token `json:"token"`
	UnitInfo util.UnitInfo `json:"unitInfo"`
}

// MiniOrder is minimal information about an order in a market's order book.
// Replaced MiniOrder, which had a float Qty in conventional units.
type MiniOrder struct {
	Qty       float64 `json:"qty"`
	QtyAtomic uint64  `json:"qtyAtomic"`
	Rate      float64 `json:"rate"`
	MsgRate   uint64  `json:"msgRate"`
	Epoch     uint64  `json:"epoch,omitempty"`
	Sell      bool    `json:"sell"`
	Token     string  `json:"token"`
}

// coinIDString converts a coin ID to a human-readable string. If an error is
// encountered the value starting with "<invalid coin>:" prefix is returned.
func coinIDString(assetID uint32, coinID []byte) string {
	coinStr, err := asset.DecodeCoinID(assetID, coinID)
	if err != nil {
		// Logging error here with fmt.Printf is better than dropping it. It's not
		// worth polluting this func signature passing in logger because it is used
		// a lot in many places.
		fmt.Printf("invalid coin ID %x for asset %d -> %s: %v\n", coinID, assetID, unbip(assetID), err)

		return "<invalid coin>:" + hex.EncodeToString(coinID)
	}
	return coinStr
}

// assetMap tracks a series of assets and provides methods for registering an
// asset and merging with another assetMap.
type assetMap map[uint32]struct{}

// count registers a new asset.
func (c assetMap) count(assetID uint32) {
	c[assetID] = struct{}{}
}

// merge merges the entries of another assetMap.
func (c assetMap) merge(other assetMap) {
	for assetID := range other {
		c[assetID] = struct{}{}
	}
}
