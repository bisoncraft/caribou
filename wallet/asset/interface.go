// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package asset

import (
	"context"

	dcrwalletjson "decred.org/dcrwallet/v5/rpc/jsonrpc/types"
	"github.com/bisoncraft/caribou/util"
	pi "github.com/bisoncraft/caribou/util/politeia/pitypes"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

// WalletTrait is a bitset indicating various optional wallet features, such as
// the presence of auxiliary methods like Rescan, Withdraw and Sweep.
type WalletTrait uint64

const (
	WalletTraitRescanner             WalletTrait = 1 << iota // The Wallet is an asset.Rescanner.
	WalletTraitNewAddresser                                  // The Wallet can generate new addresses on demand with NewAddress.
	WalletTraitLogFiler                                      // The Wallet allows for downloading of a log file.
	WalletTraitFeeRater                                      // Wallet can provide a fee rate for non-critical transactions
	WalletTraitAccelerator                                   // This wallet can accelerate transactions using the CPFP technique
	WalletTraitRecoverer                                     // The wallet is an asset.Recoverer.
	WalletTraitWithdrawer                                    // The Wallet can withdraw a specific amount from an exchange wallet.
	WalletTraitSweeper                                       // The Wallet can sweep all the funds, leaving no change.
	WalletTraitRestorer                                      // The wallet is an asset.WalletRestorer
	WalletTraitTxFeeEstimator                                // The wallet can estimate transaction fees.
	WalletTraitPeerManager                                   // The wallet can manage its peers.
	WalletTraitAuthenticator                                 // The wallet require authentication.
	WalletTraitShielded                                      // DEPRECATED. Left for ordering
	WalletTraitDeprTokenApprover                             // DEPRECATED. Left for ordering
	WalletTraitDeprAccountLocker                             // DEPRECATED. Left for ordering
	WalletTraitTicketBuyer                                   // The wallet can participate in decred staking.
	WalletTraitHistorian                                     // This wallet can return its transaction history // DEPRECATED
	WalletTraitFundsMixer                                    // The wallet can mix funds.
	WalletTraitDeprDynamicSwapper                            // DEPRECATED. Left for ordering
	WalletTraitDeprContractDeployer                          // DEPRECATED. Left for ordering
	WalletTraitDeprContractGasTester                         // DEPRECATED. Left for ordering
	WalletTraitPoliteiaVoter                                 // The wallet can vote on Politeia proposals.
	WalletTraitDestinationTagger                             // The wallet's Send accepts a destination tag.
)

// IsRescanner tests if the WalletTrait has the WalletTraitRescanner bit set.
func (wt WalletTrait) IsRescanner() bool {
	return wt&WalletTraitRescanner != 0
}

// IsNewAddresser tests if the WalletTrait has the WalletTraitNewAddresser bit
// set, which indicates the presence of a NewAddress method that will generate a
// new address on each call. If this method does not exist, the Address method
// should be assumed to always return the same receiving address.
func (wt WalletTrait) IsNewAddresser() bool {
	return wt&WalletTraitNewAddresser != 0
}

// IsLogFiler tests if WalletTrait has the WalletTraitLogFiler bit set.
func (wt WalletTrait) IsLogFiler() bool {
	return wt&WalletTraitLogFiler != 0
}

// IsFeeRater tests if the WalletTrait has the WalletTraitFeeRater bit set. Every
// Wallet implements FeeRate, so this is always true; the trait is retained for
// bit-order stability.
func (wt WalletTrait) IsFeeRater() bool {
	return wt&WalletTraitFeeRater != 0
}

// IsAccelerator tests if the WalletTrait has the WalletTraitAccelerator bit set,
// which indicates the presence of an Accelerate method.
func (wt WalletTrait) IsAccelerator() bool {
	return wt&WalletTraitAccelerator != 0
}

// IsRecoverer tests if the WalletTrait has the WalletTraitRecoverer bit set,
// which indicates the wallet implements the Recoverer interface.
func (wt WalletTrait) IsRecoverer() bool {
	return wt&WalletTraitRecoverer != 0
}

// IsWithdrawer tests if the WalletTrait has the WalletTraitSender bit set,
// which indicates the presence of a Withdraw method.
func (wt WalletTrait) IsWithdrawer() bool {
	return wt&WalletTraitWithdrawer != 0
}

// IsSweeper tests if the WalletTrait has the WalletTraitSweeper bit set, which
// indicates the presence of a Sweep method.
func (wt WalletTrait) IsSweeper() bool {
	return wt&WalletTraitSweeper != 0
}

// IsRestorer tests if the WalletTrait has the WalletTraitRestorer bit set, which
// indicates the wallet implements the WalletRestorer interface.
func (wt WalletTrait) IsRestorer() bool {
	return wt&WalletTraitRestorer != 0
}

// IsTxFeeEstimator tests if the WalletTrait has the WalletTraitTxFeeEstimator
// bit set, which indicates the wallet implements the TxFeeEstimator interface.
func (wt WalletTrait) IsTxFeeEstimator() bool {
	return wt&WalletTraitTxFeeEstimator != 0
}

// IsPeerManager tests if the WalletTrait has the WalletTraitPeerManager bit
// set, which indicates the wallet implements the PeerManager interface.
func (wt WalletTrait) IsPeerManager() bool {
	return wt&WalletTraitPeerManager != 0
}

// IsAuthenticator tests if WalletTrait has WalletTraitAuthenticator bit set,
// which indicates authentication is required by wallet.
func (wt WalletTrait) IsAuthenticator() bool {
	return wt&WalletTraitAuthenticator != 0
}

// IsTicketBuyer tests if the WalletTrait has the WalletTraitTicketBuyer bit set,
// which indicates the wallet implements the TicketBuyer interface.
func (wt WalletTrait) IsTicketBuyer() bool {
	return wt&WalletTraitTicketBuyer != 0
}

// IsFundsMixer tests if the WalletTrait has the WalletTraitFundsMixer bit set,
// which indicates the wallet implements the FundsMixer interface.
func (wt WalletTrait) IsFundsMixer() bool {
	return wt&WalletTraitFundsMixer != 0
}

// IsPoliteiaVoter tests if the WalletTrait has the WalletTraitPoliteiaVoter
// bit set, which indicates the wallet implements the PoliteiaVoter interface.
func (wt WalletTrait) IsPoliteiaVoter() bool {
	return wt&WalletTraitPoliteiaVoter != 0
}

// IsDestinationTagger tests if the WalletTrait has the
// WalletTraitDestinationTagger bit set, which indicates the wallet implements
// the DestinationTagger interface.
func (wt WalletTrait) IsDestinationTagger() bool {
	return wt&WalletTraitDestinationTagger != 0
}

// DetermineWalletTraits returns the WalletTrait bitset for the provided Wallet.
func DetermineWalletTraits(w Wallet) (t WalletTrait) {
	if _, is := w.(Rescanner); is {
		t |= WalletTraitRescanner
	}
	if _, is := w.(NewAddresser); is {
		t |= WalletTraitNewAddresser
	}
	if _, is := w.(LogFiler); is {
		t |= WalletTraitLogFiler
	}
	// Every Wallet implements FeeRate, so this trait is always set. It is
	// retained for wallet-trait bit-order stability.
	t |= WalletTraitFeeRater
	if _, is := w.(Accelerator); is {
		t |= WalletTraitAccelerator
	}
	if _, is := w.(Recoverer); is {
		t |= WalletTraitRecoverer
	}
	if _, is := w.(Withdrawer); is {
		t |= WalletTraitWithdrawer
	}
	if _, is := w.(Sweeper); is {
		t |= WalletTraitSweeper
	}
	if _, is := w.(WalletRestorer); is {
		t |= WalletTraitRestorer
	}
	if _, is := w.(TxFeeEstimator); is {
		t |= WalletTraitTxFeeEstimator
	}
	if _, is := w.(PeerManager); is {
		t |= WalletTraitPeerManager
	}
	if _, is := w.(Authenticator); is {
		t |= WalletTraitAuthenticator
	}
	if _, is := w.(TicketBuyer); is {
		t |= WalletTraitTicketBuyer
	}
	if _, is := w.(FundsMixer); is {
		t |= WalletTraitFundsMixer
	}
	if _, is := w.(PoliteiaVoter); is {
		t |= WalletTraitPoliteiaVoter
	}
	if _, is := w.(DestinationTagger); is {
		t |= WalletTraitDestinationTagger
	}
	return t
}

// CoinNotFoundError is returned when a coin cannot be found, either because it
// has been spent or it never existed. This error may be returned from
// AuditContract, Refund or Redeem as those methods expect the provided coin to
// exist and be unspent.
const (
	// ErrSwapNotInitiated most likely means that a swap using a contract has
	// not yet been mined. There is no guarantee that the swap will be mined
	// in the future.
	ErrSwapNotInitiated = util.ErrorKind("swap not yet initiated")

	CoinNotFoundError = util.ErrorKind("coin not found")
	// ErrTxRejected is returned when a transaction was rejected. This
	// generally would indicate either an internal wallet error, or potentially
	// a user using multiple instances of the wallet simultaneously. As such
	// it may or may not be advisable to try the tx again without seeking
	// further investigation.
	ErrTxRejected = util.ErrorKind("transaction was rejected")
	// ErrTxLost is returned when the tx is irreparably lost, as would be the
	// case if it's inputs were spent by another tx first or if it is not the
	// accepted tx for a given nonce. These txs have incurred no fee losses, so
	// the caller should feel free to re-issue the tx.
	ErrTxLost         = util.ErrorKind("tx lost")
	ErrRequestTimeout = util.ErrorKind("request timeout")
	ErrConnectionDown = util.ErrorKind("wallet not connected")
	ErrNotImplemented = util.ErrorKind("not implemented")
	ErrUnsupported    = util.ErrorKind("unsupported")
	// ErrSwapRefunded is returned from ConfirmTransaction when the swap has
	// been refunded before the user could redeem.
	ErrSwapRefunded = util.ErrorKind("swap refunded")
	// ErrSwapRedeemed is returned from ConfirmTransaction when the swap has
	// been redeemed before the user could refund.
	ErrSwapRedeemed = util.ErrorKind("swap redeemed")
	// ErrNotEnoughConfirms is returned when a transaction is confirmed,
	// but does not have enough confirmations to be trusted.
	ErrNotEnoughConfirms = util.ErrorKind("transaction does not have enough confirmations")
	// ErrWalletTypeDisabled indicates that a wallet type is no longer
	// available.
	ErrWalletTypeDisabled = util.ErrorKind("wallet type has been disabled")
	// ErrInsufficientBalance is returned when there is insufficient available
	// balance for an operation, such as reserving funds for future bonds.
	ErrInsufficientBalance = util.ErrorKind("insufficient available balance")
	// ErrUnapprovedToken is returned when trying to fund an order using a token
	// that has not been approved.
	ErrUnapprovedToken = util.ErrorKind("token not approved")
	ErrApprovalPending = util.ErrorKind("approval pending")
	// ErrInsufficientRedeemFunds is returned when there is insufficient funds
	// to redeem a swap, but a gasless relay redemption would fix the issue.
	ErrInsufficientRedeemFunds = util.ErrorKind("insufficient redeem funds")

	// ErrRelayRedemptionLotSizeTooSmall is returned when the lot size is
	// too small to cover the relay fees for a gasless redemption.
	ErrRelayRedemptionLotSizeTooSmall = util.ErrorKind("relay redemption lot size too small")

	// ErrMultisigPartialSend is returned when an error occurs after creating
	// a multisig and sending some amount of funds.
	ErrMultisigPartialSend = util.ErrorKind("multisig partially sent")

	// InternalNodeLoggerName is the name for a logger that is used to fine
	// tune log levels for only loggers using this name.
	InternalNodeLoggerName = "INTL"
)

type WalletDefinition struct {
	// If seeded is true, the Create method will be called with a deterministic
	// seed that should be used to set the wallet key(s). This would be
	// true for built-in wallets.
	Seeded bool `json:"seeded"`
	// Type is a string identifying the wallet type. NOTE: There should be a
	// particular WalletTrait set for any given Type, but wallet construction is
	// presently required to discern traits.
	Type string `json:"type"`
	// Tab is a displayable string for the wallet type. One or two words. First
	// word capitalized. Displayed on a wallet selection tab.
	Tab string `json:"tab"`
	// Description is a short description of the wallet, suitable for a tooltip.
	Description string `json:"description"`
	// DefaultConfigPath is the default file path that the Wallet uses for its
	// configuration file. Probably only useful for unseeded / external wallets.
	DefaultConfigPath string `json:"configpath"`
	// ConfigOpts is a slice of expected Wallet config options, with the display
	// name, config key (for parsing the option from a config file/text) and
	// description for each option. This can be used to request config info from
	// users e.g. via dynamically generated GUI forms.
	ConfigOpts []*ConfigOption `json:"configopts"`
	// MultiFundingOpts are options related to funding multi-trades.
	MultiFundingOpts []*OrderOption `json:"multifundingopts"`
	// NoAuth indicates that the wallet does not implement the Authenticator
	// interface. A better way to check is to use the wallet traits but wallet
	// construction is presently required to discern traits.
	NoAuth bool `json:"noauth"`
	// GuideLink is a link to wallet configuration docs that the user may follow
	// when creating a new wallet or updating its settings.
	GuideLink string `json:"guidelink"`
}

// Network is information about an asset's network.
type Network struct {
	ID            uint32 `json:"id"`
	NativeAssetID uint32 `json:"nativeAssetID"`
	// Priority indicates this is a well-known, high-priority network that
	// should be pre-selected by default in multi-network activation UIs.
	// Obscure or experimental networks can set this to false so they are
	// opt-in rather than opt-out.
	Priority bool `json:"priority"`
}

// Token combines the generic util.Token with a WalletDefinition.
type Token struct {
	*util.Token
	Definition      *WalletDefinition `json:"definition"`
	ContractAddress string            `json:"contractAddress"` // Set in SetNetwork
}

type BlockchainClass string

const (
	BlockchainClassUTXO    BlockchainClass = "UTXO"
	BlockchainClassEVM     BlockchainClass = "EVM"
	BlockchainClassAccount BlockchainClass = "Account"
)

func (c BlockchainClass) IsEVM() bool {
	return c == BlockchainClassEVM
}

// WalletInfo is auxiliary information about an ExchangeWallet.
type WalletInfo struct {
	// Name is the display name for the currency, e.g. "Decred"
	Name string `json:"name"`
	// AvailableWallets is an ordered list of available WalletDefinition. The
	// first WalletDefinition is considered the default, and might, for instance
	// be the initial form offered to the user for configuration, with others
	// available to select.
	AvailableWallets []*WalletDefinition `json:"availablewallets"`
	// UnitInfo is the information about unit names and conversion factors for
	// the asset.
	UnitInfo util.UnitInfo `json:"unitinfo"`
	// BlockchainClass is the type of the wallet's blockchain.
	BlockchainClass BlockchainClass
	// MaxFeeRate is an absolute ceiling on caller-approved fee rates, in the
	// asset's atomic unit per (virtual) byte. It is set far above any
	// plausible fee market; a submitted rate beyond it indicates a client bug
	// (e.g. a units mixup) and is rejected regardless of whether live fee or
	// fiat estimates are available. Zero means no ceiling; only assets whose
	// wallets price transactions at a caller-approved rate (UTXO chains) need
	// one.
	MaxFeeRate uint64 `json:"maxfeerate"`
	// Network describes the chain this wallet operates on. For chains that
	// host tokens, this is the network referenced by token Token.NetworkID.
	Network *Network `json:"network"`
	// SeedAssetID, when non-zero, overrides the asset ID used to derive this
	// wallet's seed from the application seed. It is set when a chain must
	// derive the same keys as another registered asset, e.g. HyperEVM shares
	// HyperCore's address so funds sent over either network stay reachable.
	SeedAssetID uint32 `json:"-"`
}

// ConfigOption is a wallet configuration option.
type ConfigOption struct {
	Key          string `json:"key"`
	DisplayName  string `json:"displayname"`
	Description  string `json:"description"`
	DefaultValue string `json:"default"`
	// If MaxValue/MinValue are set to the string "now" for a date config, the
	// UI will display the current date.
	MaxValue          any `json:"max"`
	MinValue          any `json:"min"`
	Options           map[string]*ConfigOption
	NoEcho            bool `json:"noecho"`
	IsBoolean         bool `json:"isboolean"`
	IsDate            bool `json:"isdate"`
	DisableWhenActive bool `json:"disablewhenactive"`
	// Repeatable signals a text input that can be duplicated and submitted
	// multiple times, with the specified delimiter used to encode the data
	// in the settings map.
	Repeatable string `json:"repeatable"`
	// RepeatN signals how many times text input should be repeated, replicating
	// this option N times.
	RepeatN  int32 `json:"repeatN"`
	Required bool  `json:"required"`
	// DependsOn is the key of another config option that if is set to true,
	// this config option will be shown.
	DependsOn string `json:"dependsOn"`

	// ShowByDefault to show or not options on "hide advanced options".
	ShowByDefault bool `json:"showByDefault,omitempty"`
}

const (
	// SpecialSettingActivelyUsed is a special setting that can be injected by
	// core that lets the wallet know that it is being actively used. A use
	// case is by the bitcoin SPV wallet to decide whether or not it is safe
	// to do a full rescan.
	SpecialSettingActivelyUsed = "special_activelyUsed"
)

// WalletConfig is the configuration settings for the wallet. WalletConfig
// is passed to the wallet constructor.
type WalletConfig struct {
	// Type is the type of wallet, corresponding to the Type field of an
	// available WalletDefinition.
	Type string
	// Settings is the key-value store of wallet connection parameters. The
	// Settings are supplied by the user according the WalletInfo's
	// ConfigOpts.
	Settings map[string]string
	// Emit is a WalletEmitter that manages a channel over which an asset may
	// issue notifications. Every asset is expected to issue a TipChangeNote
	// when a network change occurs that might require balance updates or action
	// on swaps.
	Emit *WalletEmitter
	// PeersChange is a function that will be called when the number of
	// wallet/node peers changes, or the wallet fails to get the count. This
	// should not be called prior to Connect of the constructed wallet.
	PeersChange func(uint32, error)
	// DataDir is a filesystem directory the wallet may use for persistent
	// storage.
	DataDir string
	// TorProxy is the address (host:port) of a SOCKS5 proxy (e.g. Tor) for
	// outgoing wallet connections such as RPC providers and Electrum servers.
	TorProxy string
	// NetworkTxDB is a handle to the master transaction database, scoped to this
	// wallet's network. The wallet uses it to store and retrieve its own
	// transactions.
	NetworkTxDB txdb.NetworkTxDB
}

// ConfirmTxStatus contains the coinID which redeemed or refunded a swap, the
// number of confirmations the transaction has, and the number of confirmations
// required for it to be considered confirmed.
type ConfirmTxStatus struct {
	Confs  uint64
	Req    uint64
	CoinID util.Bytes
	// PendingSubmission is true for ETH relay redemptions before the relay
	// has submitted the transaction on-chain. The redemption message should
	// only be sent to the server once this is false.
	PendingSubmission bool
}

// Wallet is a common interface to be implemented by cryptocurrency wallet
// software. It covers basic wallet operations: balance, send, receive, and
// transaction history.
type Wallet interface {
	// It should be assumed that once disconnected, subsequent Connect calls
	// will fail, requiring a new Wallet instance.
	util.Connector
	// Info returns a set of basic information about the wallet driver.
	Info() *WalletInfo
	// Balance should return the balance of the wallet, categorized by
	// available, immature, and locked. Balance takes a list of minimum
	// confirmations for which to calculate maturity, and returns a list of
	// corresponding *Balance.
	Balance() (*Balance, error)
	// DepositAddress returns an address for depositing funds into the wallet.
	DepositAddress() (string, error)
	// OwnsDepositAddress indicates if the provided address can be used
	// to deposit funds into the wallet.
	OwnsDepositAddress(address string) (bool, error)
	// Send sends the exact value to the specified address. This is different
	// from Withdraw, which subtracts the tx fees from the amount sent.
	Send(address string, value, feeRate uint64) (Coin, error)
	// ValidateAddress checks that the provided address is valid.
	ValidateAddress(address string) bool
	// SyncStatus is information about the blockchain sync status. It should
	// only indicate synced when there are network peers and all blocks on the
	// network have been processed by the wallet.
	SyncStatus() (*SyncStatus, error)
	// StandardSendFee returns the fee for a "standard" send tx.
	StandardSendFee(feeRate uint64) uint64
	// FeeRate returns a non-critical fee-rate estimate for the asset. The rate
	// is for the wallet's own Send and Withdraw, and is not a server-validated
	// swap fee suggestion. Some wallets cannot always provide an estimate (an
	// SPV or freshly-started wallet, for example), so a zero rate may be
	// returned; a zero rate means no estimate is available, and callers must
	// treat fee-dependent operations as blocked rather than substituting a
	// made-up rate.
	FeeRate() uint64
	// TxHistory returns all the transactions a wallet has made. If refID
	// is nil, then transactions starting from the most recent are returned
	// (past is ignored). If past is true, the transactions prior to the
	// refID are returned, otherwise the transactions after the refID are
	// returned. n is the number of transactions to return. If n is <= 0,
	// all the transactions will be returned.
	TxHistory(*txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error)
	// WalletTransaction returns a single transaction that either a wallet
	// has made or in which the wallet has received funds. This function may
	// support more transactions than are returned by TxHistory. For example,
	// ETH/token wallets do not return receiving transactions in TxHistory,
	// but WalletTransaction will return them.
	WalletTransaction(ctx context.Context, txID string) (*txdb.WalletTransaction, error)
	// PendingTransactions loads wallet transactions that are not yet confirmed.
	PendingTransactions(ctx context.Context) []*txdb.WalletTransaction
}

// Authenticator is a wallet implementation that require authentication.
type Authenticator interface {
	// Unlock unlocks the exchange wallet.
	Unlock(pw []byte) error
	// Lock locks the exchange wallet.
	Lock() error
	// Locked will be true if the wallet is currently locked.
	Locked() bool
}

// TxFeeEstimator is a wallet implementation with fee estimation functionality.
type TxFeeEstimator interface {
	// EstimateSendTxFee returns a tx fee rate estimate for sending or withdrawing
	// the provided amount using the provided feeRate. This uses actual utxos to
	// calculate the tx fee where possible and ensures the wallet has enough to
	// cover send value and minimum fees.
	EstimateSendTxFee(address string, value, feeRate uint64, subtract, maxWithdraw bool) (fee uint64, isValidAddress bool, err error)
}

// DestinationTagger is a wallet whose network routes a payment to a
// sub-account of the destination address using a numeric tag (XRPL's
// DestinationTag). Exchange deposit addresses commonly require one.
type DestinationTagger interface {
	// SendTagged sends value to address with destTag included in the
	// transaction. Semantics otherwise match Wallet.Send.
	SendTagged(address string, value, feeRate uint64, destTag uint32) (Coin, error)
}

// Broadcaster is a wallet that can send a raw transaction on the asset network.
type Broadcaster interface {
	// SendTransaction broadcasts a raw transaction, returning its coin ID.
	SendTransaction(rawTx []byte) ([]byte, error)
}

// Opener is used by wallets that need a password to open the wallet for the first time.
type Opener interface {
	// OpenWithPW opens a wallet that needs a password. Should be done before
	// connecting to the wallet.
	OpenWithPW(ctx context.Context, pw []byte) error
	// IsOpen returns whether the wallet is currently open.
	IsOpen() bool
	// Close should be called when done using the wallet.
	Close() error
}

// SyncStatus is the status of wallet syncing.
type SyncStatus struct {
	Synced         bool    `json:"synced"`
	TargetHeight   uint64  `json:"targetHeight"`
	StartingBlocks uint64  `json:"startingBlocks"`
	Blocks         uint64  `json:"blocks"`
	Transactions   *uint64 `json:"txs,omitempty"`
}

func (ss *SyncStatus) BlockProgress() float32 {
	switch {
	case ss.Synced:
		return 1
	case ss.TargetHeight == 0:
		return 0
	case ss.TargetHeight == ss.StartingBlocks:
		return 0.999
	}
	prog := float32(ss.Blocks-ss.StartingBlocks) / float32(ss.TargetHeight-ss.StartingBlocks)
	if ss.Transactions == nil { // If the asset doesn't support tx sync status, max unsynced is 0.999
		return min(prog, 0.999)
	}
	return prog
}

type PaymentMultisig struct {
	NRequired, Locktime int64
	AssetID             uint32
	SignerXpubs         [][]byte
	AddrToVal           map[string]float64
	SpendingTx          *PaymentMultisigTx
}

type PaymentMultisigTx struct {
	TxHex   string `json:"txhex"`
	HasSigs []bool `json:"hassigs"`
}

// Multisigner is a wallet capable of sending payments to multisig addresses.
type Multisigner interface {
	Broadcaster
	// SendFundsToMultisig sends amounts to a multisig address, then creates
	// a transaction that spends those funds. ErrMultisigPartialSend and
	// partial results are returned if an error happens after sending funds.
	SendFundsToMultisig(ctx context.Context, pm *PaymentMultisig) (*PaymentMultisigTx, error)
	// SignMultisig signs the pmTx with the supplied privateKey and inserts
	// the signature at idx among the other signatures.
	SignMultisig(ctx context.Context, pmTx *PaymentMultisigTx, privKey []byte) (*PaymentMultisigTx, error)
	// RefundMultisig refunds a multisig if it is after the locktime and we
	// are the sender.
	RefundMultisig(ctx context.Context, pmTx *PaymentMultisigTx) (txHash string, err error)
	// ViewPaymentMultisig returns a tx hex in human readable json format.
	ViewPaymentMultisig(pmTx *PaymentMultisigTx) (string, error)
}

// IncomingTxRecorder is implemented by wallets that cannot observe
// unsolicited incoming transactions on their own (account-based chains
// with no per-address transaction index over standard RPC) but can fetch
// and record one when its hash is learned from an external source, such
// as a cross-chain swap service reporting its payout.
type IncomingTxRecorder interface {
	// RecordIncomingTx fetches the transaction and stores it in the
	// wallet's transaction history. It is idempotent, and errors if the
	// transaction is unknown, not yet mined, or cannot be attributed to
	// the wallet. amtHint is the payment amount as reported by the
	// external source, in atomic units; it is recorded (and marked as
	// externally reported) only when the amount cannot be attributed
	// on-chain, as with a contract-internal native transfer from an
	// unrecognized contract. Zero disables the fallback.
	RecordIncomingTx(ctx context.Context, txID string, amtHint uint64) error
}

// Rescanner is a wallet implementation with rescan functionality.
type Rescanner interface {
	// Rescan performs a rescan and block until it is done. If no birthday is
	// provided, internal wallets may use a birthday concurrent with the
	// earliest date at which a wallet was possible, which is asset-dependent.
	Rescan(ctx context.Context, bday /* unix time seconds */ uint64) error
}

// Recoverer is a wallet implementation with recover functionality.
type Recoverer interface {
	// GetRecoveryCfg returns information that will help the wallet get back to
	// its previous state after it is recreated.
	GetRecoveryCfg() (map[string]string, error)
	// Move will move all wallet files to a backup directory so the wallet can
	// be recreated.
	Move(backupdir string) error
}

// TxAbandoner is a wallet that can abandon (forget about) unconfirmed
// transactions. This is useful when a transaction gets stuck (e.g., due to
// low fees) and the wallet needs to stop tracking it to allow the funds to
// be spent again.
type TxAbandoner interface {
	// AbandonTransaction marks an unconfirmed transaction and all its
	// descendants as abandoned. This allows the wallet to forget about the
	// transaction and potentially spend its inputs in a different transaction.
	// Returns an error if the transaction is confirmed or does not exist.
	AbandonTransaction(ctx context.Context, txID string) error
}

// Withdrawer is a wallet that can withdraw a certain amount from the
// source wallet/account.
type Withdrawer interface {
	// Withdraw withdraws funds to the specified address. Fees are subtracted
	// from the value.
	Withdraw(address string, value, feeRate uint64) (Coin, error)
}

// Bridger is a wallet that can bridge funds to and receive bridged funds from
// another blockchain.
type Bridger interface {
	// ApproveBridgeContract submits a transaction to authorize the bridge contract
	// to manage the user's tokens. This step is optional for some assets.
	// Use BridgeContractApprovalStatus to determine if approval is necessary.
	// The onConfirm callback is called when the approval transaction confirms.
	ApproveBridgeContract(ctx context.Context, bridgeName string, onConfirm func()) (string, error)

	// UnapproveBridgeContract submits a transaction to revoke the bridge contract's
	// permission to manage the user's tokens.
	// The onConfirm callback is called when the unapproval transaction confirms.
	UnapproveBridgeContract(ctx context.Context, bridgeName string, onConfirm func()) (string, error)

	// BridgeContractApprovalStatus retrieves the current approval state of the bridge contract.
	// Returns Approved for assets that don't require explicit approval.
	BridgeContractApprovalStatus(ctx context.Context, bridgeName string) (ApprovalStatus, error)

	// InitiateBridge starts a fund transfer to the specified destination chain.
	// If a completion transaction is needed, a BridgeReadyToCompleteNote will
	// be emitted. Some assets do not require a completion transaction.
	InitiateBridge(ctx context.Context, amt uint64, dest uint32, bridgeName string) (txID string, err error)

	// CompleteBridge finalizes a bridge by executing a transaction on the
	// destination chain to issue the user's tokens. Some assets require
	// multiple transactions to complete the bridge. Once all the transactions
	// are confirmed, a BridgeCompletedNote will be emitted.
	CompleteBridge(ctx context.Context, bridgeTx *txdb.BridgeCounterpartTx, amount uint64, mintData []byte, bridgeName string) error

	// MarkBridgeComplete is called when the destination chain sends a
	// notification regarding the completion of a bridge. The completionTxIDs
	// are the IDs of the transactions that completed the bridge. The amtReceived
	// is the amount of tokens received on the destination chain. The fees are
	// the network fees paid on the destination chain. The complete boolean is
	// true if the bridge has fully completed and the funds are allocated on
	// the destination chain.
	MarkBridgeComplete(initiationTxID string, completionTxIDs []string, amtReceived, fees uint64, complete bool)

	// PendingBridges lists all uncompleted bridge transactions on the blockchain.
	// For token wallets, this includes pending bridges for other tokens on the same chain.
	PendingBridges() ([]*txdb.WalletTransaction, error)

	// BridgeHistory retrieves a record of bridge transactions on the blockchain.
	// For token wallets, this includes history for other tokens on the same chain.
	BridgeHistory(n int, refID *string, past bool) ([]*txdb.WalletTransaction, error)

	// SupportedDestinations returns the list of asset IDs that are supported as destinations for the origin asset.
	SupportedDestinations() map[uint32][]string

	// BridgeInitiationFeesAndLimits returns the estimated for initiating a bridge, and the
	// min and mix limits for the bridge, if any.
	BridgeInitiationFeesAndLimits(bridgeName string, destAssetID uint32) (estimatedFee uint64, limits [2]uint64, hasLimits bool, err error)

	// BridgeCompletionFees returns the estimated fees for completing a bridge,
	// and whether the wallet has sufficient balance available to pay those fees.
	BridgeCompletionFees(bridgeName string) (fees uint64, hasSufficientBalance bool, err error)
}

// Sweeper is a wallet that can clear the entire balance of the wallet/account
// to an address. Similar to Withdraw, but no input value is required.
type Sweeper interface {
	Sweep(address string, feeRate uint64) (Coin, error)
}

// NewAddresser is a wallet that can generate new deposit addresses.
type NewAddresser interface {
	NewAddress() (string, error)
	AddressUsed(string) (bool, error)
}

// LogFiler is a wallet that allows for downloading of its log file.
type LogFiler interface {
	LogFilePath() string
}

// FundsMixingStats describes the current state of a wallet's funds mixer.
type FundsMixingStats struct {
	// Enabled is true if the wallet is configured for funds mixing. The wallet
	// must be configured before mixing can be started.
	Enabled bool `json:"enabled"`
	// UnmixedBalanceThreshold is the minimum amount of unmixed funds that must
	// be in the wallet for mixing to happen.
	UnmixedBalanceThreshold uint64 `json:"unmixedBalanceThreshold"`
	// MixedFunds is the total amout of funds in the mixed account.
	MixedFunds uint64 `json:"mixedFunds"`
	// TradingFunds is the total amout of funds in the trading account.
	TradingFunds uint64 `json:"tradingFunds"`
}

// FundsMixer defines methods for mixing funds in a wallet.
type FundsMixer interface {
	// FundsMixingStats returns the current state of the wallet's funds mixer.
	FundsMixingStats() (*FundsMixingStats, error)
	// ConfigureFundsMixer configures the wallet for funds mixing.
	ConfigureFundsMixer(enabled bool) error
}

// WalletRestoration contains all the information needed for a user to restore
// their wallet in an external wallet.
type WalletRestoration struct {
	Target string `json:"target"`
	Seed   string `json:"seed"`
	// SeedName is the name of the seed used for this particular wallet, i.e
	// Private Key.
	SeedName     string `json:"seedName"`
	Instructions string `json:"instructions"`
}

// WalletRestorer is a wallet which gives information about how to restore
// itself in external wallet software.
type WalletRestorer interface {
	// RestorationInfo returns information about how to restore the wallet in
	// various external wallets.
	RestorationInfo(seed []byte) ([]*WalletRestoration, error)
}

// EarlyAcceleration is returned from the PreAccelerate function to inform the
// user that either their last acceleration or oldest swap transaction happened
// very recently, and that they should double check that they really want to do
// an acceleration.
type EarlyAcceleration struct {
	// TimePast is the amount of seconds that has past since either the previous
	// acceleration, or the oldest unmined swap transaction was submitted to
	// the blockchain.
	TimePast uint64 `json:"timePast"`
	// WasAccelerated is true if the action that took place TimePast seconds
	// ago was an acceleration. If false, the oldest unmined swap transaction
	// in the order was submitted TimePast seconds ago.
	WasAccelerated bool `json:"wasAccelerated"`
}

// Accelerator is implemented by wallets which support acceleration of the
// mining of swap transactions.
type Accelerator interface {
	// FeesForRemainingSwaps returns the fees for a certain number of
	// chained/grouped swaps at a given feeRate. This should be used with an
	// Accelerator wallet to help compute the required amount for remaining
	// swaps for a given trade with a mix of future and active matches, which is
	// only known to the consumer. This is only accurate if each swap has a
	// single input or chained swaps all pay the same fees. Accurate estimates
	// for new orders without existing funding should use PreSwap or FundOrder.
	FeesForRemainingSwaps(n, feeRate uint64) uint64
	// AccelerateOrder uses the Child-Pays-For-Parent technique to accelerate a
	// chain of swap transactions and previous accelerations. It broadcasts a new
	// transaction with a fee high enough so that the average fee of all the
	// unconfirmed transactions in the chain and the new transaction will have
	// an average fee rate of newFeeRate. The changeCoin argument is the latest
	// change in the order. It must be the input in the acceleration transaction
	// in order for the order to be accelerated. requiredForRemainingSwaps is the
	// amount of funds required to complete the rest of the swaps in the order.
	// The change output of the acceleration transaction will have at least
	// this amount.
	//
	// The returned change coin may be nil, and should be checked before use.
	AccelerateOrder(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes,
		requiredForRemainingSwaps, newFeeRate uint64) (Coin, string, error)
	// AccelerationEstimate takes the same parameters as AccelerateOrder, but
	// instead of broadcasting the acceleration transaction, it just returns
	// the amount of funds that will need to be spent in order to increase the
	// average fee rate to the desired amount.
	AccelerationEstimate(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes,
		requiredForRemainingSwaps, newFeeRate uint64) (uint64, error)
	// PreAccelerate returns the current average fee rate of the unmined swap
	// initiation and acceleration transactions, and also returns a suggested
	// range that the fee rate should be increased to in order to expedite mining.
	// The feeSuggestion argument is the current prevailing network rate. It is
	// used to help determine the suggestedRange, which is a range meant to give
	// the user a good amount of flexibility in determining the post acceleration
	// effective fee rate, but still not allowing them to pick something
	// outrageously high.
	PreAccelerate(swapCoins, accelerationCoins []util.Bytes, changeCoin util.Bytes,
		requiredForRemainingSwaps, feeSuggestion uint64) (uint64, *XYRange, *EarlyAcceleration, error)
}

// TokenConfig is required to OpenTokenWallet.
type TokenConfig struct {
	// AssetID of the token.
	AssetID uint32
	// Settings correspond to Token.Definition.ConfigOpts.
	Settings map[string]string
	// Emit is a WalletEmitter that manages a channel over which an asset may
	// issue notifications.
	Emit *WalletEmitter
	// PeersChange will be called after the parent's PeersChange.
	PeersChange func(uint32, error)
	// NetworkTxDB is a handle to the master transaction database, scoped to the
	// token's network (its parent chain). The wallet uses it to store and
	// retrieve its own transactions.
	NetworkTxDB txdb.NetworkTxDB
}

// TokenMaster is implemented by assets which support degenerate tokens.
type TokenMaster interface {
	// CreateTokenWallet creates a wallet for the specified token asset. The
	// settings correspond to the Token.Definition.ConfigOpts.
	CreateTokenWallet(assetID uint32, settings map[string]string) error
	// OpenTokenWallet opens a wallet for the specified token asset.
	OpenTokenWallet(cfg *TokenConfig) (Wallet, error)
}

// TrustlineState describes whether a token wallet's account has opted in to
// hold its token, and what the opt-in costs in the network's native asset.
type TrustlineState struct {
	// Status is Approved when the trust line exists, Pending while an
	// establishing transaction awaits validation, and NotApproved otherwise.
	Status ApprovalStatus `json:"status"`
	// Reserve is the native-asset amount, in atomic units, the account must
	// hold above its base reserve for as long as the trust line exists. It is
	// held rather than spent, and released when the line is removed.
	Reserve uint64 `json:"reserve"`
	// Shortfall is the additional native-asset balance, in atomic units, the
	// account needs before EstablishTrustline can succeed, including the base
	// reserve when the account does not yet exist on the ledger. Zero when
	// the line can be established now, or already exists.
	Shortfall uint64 `json:"shortfall"`
}

// TrustlineEstablisher is implemented by token wallets whose token requires
// an explicit, reserve-bearing opt-in transaction (an XRPL TrustSet) before
// the account can receive it. Establishing the line is a user-initiated
// action rather than part of wallet creation: it commits native funds on the
// ledger and needs a funded account. A token wallet without a trust line is a
// valid state; it reports a zero balance and cannot receive the token until
// the line exists.
type TrustlineEstablisher interface {
	// TrustlineStatus reports the wallet's cached trust-line state. It does
	// not query the network. The wallet refreshes the cache alongside its
	// balance and announces changes through a "trustline" custom wallet note
	// whose payload is the new TrustlineState.
	TrustlineStatus() *TrustlineState
	// EstablishTrustline submits the opt-in transaction and returns its ID.
	// It is idempotent: an existing line returns an empty ID and no error,
	// and a still-pending establishing transaction returns that
	// transaction's ID without resubmitting. An unaffordable line returns an
	// error wrapping ErrInsufficientBalance.
	EstablishTrustline() (txID string, err error)
}

// MinimumDepositer is implemented by wallets on networks where an account
// must hold a minimum balance to exist on the ledger, such as the XRPL base
// reserve. A first deposit below the minimum is rejected by the network at
// the sender, so the deposit flow should state it before an account is
// funded.
type MinimumDepositer interface {
	// MinimumDeposit is the smallest first deposit, in atomic units, that
	// creates the account on the ledger.
	MinimumDeposit() uint64
}

// LiveReconfigurer is a wallet that can possibly handle a reconfiguration
// without the need for re-initialization.
type LiveReconfigurer interface {
	// Reconfigure attempts to reconfigure the wallet. If reconfiguration
	// requires a restart, the Wallet should still validate as much
	// configuration as possible.
	Reconfigure(ctx context.Context, cfg *WalletConfig, currentAddress string) (restartRequired bool, err error)
}

// PeerSource specifies how a wallet knows about a peer. It may have been
// hardcoded into the wallet code, added manually by the user, or discovered
// by communicating with the default/user added peers.
type PeerSource uint16

const (
	WalletDefault PeerSource = iota
	UserAdded
	Discovered
)

// WalletPeer provides information about a wallet's peer.
type WalletPeer struct {
	Addr      string     `json:"addr"`
	Source    PeerSource `json:"source"`
	Connected bool       `json:"connected"`
}

// PeerManager is a wallet which provides allows the user to see the peers the
// wallet is connected to and add new peers.
type PeerManager interface {
	// Peers returns a list of peers that the wallet is connected to.
	Peers() ([]*WalletPeer, error)
	// AddPeer connects the wallet to a new peer. The peer's address will be
	// persisted and connected to each time the wallet is started up.
	AddPeer(addr string) error
	// RemovePeer will remove a peer that was added by AddPeer. This peer may
	// still be connected to by the wallet if it discovers it on its own.
	RemovePeer(addr string) error
}

type ApprovalStatus uint8

const (
	Approved ApprovalStatus = iota
	Pending
	NotApproved
)

func (a ApprovalStatus) String() string {
	switch a {
	case Approved:
		return "approved"
	case Pending:
		return "pending"
	case NotApproved:
		return "not approved"
	}
	return "unknown"
}

// TicketTransaction represents a ticket transaction.
type TicketTransaction struct {
	Hash        string `json:"hash"`
	TicketPrice uint64 `json:"ticketPrice"`
	Fees        uint64 `json:"fees"`
	Stamp       uint64 `json:"stamp"`
	BlockHeight int64  `json:"blockHeight"`
}

// TicketStatus from dcrwallet.
type TicketStatus uint

// Copy of wallet.TicketStatus
const (
	TicketStatusUnknown TicketStatus = iota
	TicketStatusUnmined
	TicketStatusImmature
	TicketStatusLive
	TicketStatusVoted
	TicketStatusMissed
	TicketStatusExpired
	TicketStatusUnspent
	TicketStatusRevoked
)

// Ticket holds information about a decred ticket.
type Ticket struct {
	Tx      TicketTransaction `json:"tx"`
	Status  TicketStatus      `json:"status"`
	Spender string            `json:"spender"`
}

// TBChoice is a possible agenda choice for a TicketBuyer.
type TBChoice struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// TBAgenda is an agenda that the TicketBuyer can vote on.
type TBAgenda struct {
	ID            string      `json:"id"`
	Description   string      `json:"description"`
	CurrentChoice string      `json:"currentChoice"`
	Choices       []*TBChoice `json:"choices"`
}

// TBTreasurySpend represents a treasury spend that the TicketBuyer can vote on.
type TBTreasurySpend struct {
	Hash          string `json:"hash"`
	Value         uint64 `json:"value"`
	CurrentPolicy string `json:"currentPolicy"`
}

// Stances are current policy preferences for the TicketBuyer.
type Stances struct {
	Agendas        []*TBAgenda                           `json:"agendas"`
	TreasurySpends []*TBTreasurySpend                    `json:"tspends"`
	TreasuryKeys   []*dcrwalletjson.TreasuryPolicyResult `json:"treasuryKeys"`
}

// VotingServiceProvider is information about a voting service provider.
type VotingServiceProvider struct {
	URL           string       `json:"url"`
	Network       util.Network `json:"network"`
	Launched      uint64       `json:"launched"`    // milliseconds
	LastUpdated   uint64       `json:"lastUpdated"` // milliseconds
	APIVersions   []int64      `json:"apiVersions"`
	FeePercentage float64      `json:"feePercentage"`
	Closed        bool         `json:"closed"`
	Voting        int64        `json:"voting"`
	Voted         int64        `json:"voted"`
	Revoked       int64        `json:"revoked"`
	VSPDVersion   string       `json:"vspdVersion"`
	BlockHeight   uint32       `json:"blockHeight"`
	NetShare      float32      `json:"netShare"`
}

// TicketStats sums up some statistics for historical staking data for a
// TicketBuyer.
type TicketStats struct {
	TotalRewards uint64 `json:"totalRewards"`
	TicketCount  uint32 `json:"ticketCount"`
	Votes        uint32 `json:"votes"`
	Revokes      uint32 `json:"revokes"`
	Queued       uint32 `json:"queued"`
	Mempool      uint32 `json:"mempool"`
}

// TicketStakingStatus holds various stake information from the wallet.
type TicketStakingStatus struct {
	// TicketPrice is the current price of one ticket. Also known as the
	// stake difficulty.
	TicketPrice uint64 `json:"ticketPrice"`
	// VotingSubsidy is the current reward for a vote.
	VotingSubsidy uint64 `json:"votingSubsidy"`
	// VSP is the currently set VSP address and fee.
	VSP string `json:"vsp"`
	// IsRPC will be true if this is an RPC wallet, in which case we can't
	// set a new VSP and some other information may not be available.
	IsRPC bool `json:"isRPC"`
	// Tickets returns current active tickets up until they are voted or
	// revoked. Includes unconfirmed tickets.
	Tickets []*Ticket `json:"tickets"`
	// Stances returns current voting preferences.
	Stances Stances `json:"stances"`
	// Stats is statistical info about staking history.
	Stats TicketStats `json:"stats"`
}

// TicketBuyer is a wallet that can participate in decred staking.
// Satisfies pi.VotingWallet.
// TODO: Consider adding (*AutoClient).ProcessUnprocessedTickets/ProcessManagedTickets
// to be used when restoring wallet from seed.
type TicketBuyer interface {
	// StakeStatus returns current staking statuses such as currently owned
	// tickets, ticket price, and current voting preferences.
	StakeStatus() (*TicketStakingStatus, error)
	// SetVSP sets the VSP provider.
	SetVSP(addr string) error
	// PurchaseTickets starts an asynchronous process to purchase n tickets.
	// Look for TicketPurchaseUpdate notifications to track the process.
	PurchaseTickets(n int, feeSuggestion uint64) error
	// SetVotingPreferences sets default voting settings for all active
	// tickets and future tickets. Nil maps can be provided for no change.
	SetVotingPreferences(choices, tSpendPolicy, treasuryPolicy map[string]string) error
	// ListVSPs lists known available voting service providers.
	ListVSPs() ([]*VotingServiceProvider, error)
	// TicketPage fetches a page of tickets within a range of block numbers with
	// a target page size and optional offset. scanStart is the block in which
	// to start the scan. The scan progresses in reverse block number order,
	// starting at scanStart and going to progressively lower blocks. scanStart
	// can be set to -1 to indicate the current chain tip.
	TicketPage(scanStart int32, n, skipN int) ([]*Ticket, error)
	// AddressAccount returns the account number for an address.
	AddressAccount(address string) (bool, uint32, error)
	// SignMessage returns the signature of a signed message using an address'
	// associated private key.
	SignMessage(msg string, addr string) ([]byte, error)
	// CommittedTickets takes a list of tickets and returns a filtered list of
	// tickets that are controlled by this wallet.
	CommittedTickets(tickets []*chainhash.Hash) ([]*chainhash.Hash, []stdaddr.Address, error)
}

// PoliteiaVoter is a wallet that can sync Decred governance proposals from
// Politeia and cast votes.
//
// NOTE: This interface references types from dex/politeia, which couples this
// generic package to DCR-specific types. This is consistent with the existing
// TicketBuyer interface that references dcrd and dcrwallet types. If more
// assets need governance interfaces in the future, consider abstracting the
// proposal types.
type PoliteiaVoter interface {
	// PoliteiaDetails returns the Politeia URL, whether a sync is in progress,
	// and the unix timestamp of the last completed sync.
	PoliteiaDetails() (url string, syncing bool, lastSync int64)
	// ProposalsAll fetches proposals from the local DB with optional filtering.
	ProposalsAll(offset, rowsCount int, searchPhrase string, filterByVoteStatus ...int) ([]*pi.Proposal, int, error)
	// Proposal retrieves a single proposal by token, populating vote details
	// if the proposal is currently voting and the wallet has eligible tickets.
	Proposal(token string) (*pi.Proposal, error)
	// ProposalsInProgress returns mini proposals for proposals currently in
	// progress (unauthorized, authorized, or started).
	ProposalsInProgress() ([]*pi.MiniProposal, error)
	// CastVote casts votes on the proposal identified by token using the
	// provided vote bit. The wallet must already be unlocked.
	CastVote(token, bit string) error
	// PoliteiaEnabled returns whether Politeia sync is currently enabled.
	PoliteiaEnabled() bool
}

// Balance is categorized information about a wallet's balance.
type Balance struct {
	// Available is the balance that is available for trading immediately.
	Available uint64 `json:"available"`
	// Immature is the balance that is not ready, but will be after some
	// confirmations.
	Immature uint64 `json:"immature"`
	// Locked is the total amount locked in the wallet which includes but
	// is not limited to funds locked for swap but not actually swapped yet.
	Locked uint64 `json:"locked"`
	// ReservesDeficit is the difference between the available balance and the
	// amount reserved for specific purposes.
	ReservesDeficit uint64 `json:"reservesDeficit"`
	// Other is a place to list custom balance categories. It is recommended for
	// custom balance added here to have a translation and tooltip info in
	// wallet/appserver/site/src/js/wallet.js#customWalletBalanceCategory
	Other map[BalanceCategory]CustomBalance `json:"other"`
}

// CustomBalance is a balance category used to track funds for a particular
// purpose or for a special kind of balance (e.g Zcash Shielded wallet
// balances).
type CustomBalance struct {
	// Amount is the balance in the wallet for this custom category. It is
	// subtracted from Balance.Available above.
	Amount uint64 `json:"amt"`
	// Locked is a flag to indicate that this Amount is not included in
	// Balance.Available and is included in Balance.Locked.
	Locked bool `json:"locked"`
}

// BalanceCategory is a string identifier for a custom balance category.
type BalanceCategory string

// Balance categories for custom balances These values are used as a map key for
// custom balances and may be recognized in the frontend to support translation.
// It is recommended for custom balance categories listed here to have a
// translation and tooltip info in
// wallet/appserver/site/src/js/wallet.js#customWalletBalanceCategory. If any of
// these balance categories should change, the customWalletBalanceCategory
// function in the wallet.js file above should be updated with the new value.
const (
	BalanceCategoryShielded = "Shielded"
	BalanceCategoryUnmixed  = "Unmixed"
	BalanceCategoryStaked   = "Staked"
)

// Coin is some amount of spendable asset. Coin provides the information needed
// to locate the unspent value on the blockchain.
type Coin interface {
	// ID is a unique identifier for this coin.
	ID() util.Bytes
	// String is a string representation of the coin.
	String() string
	// Value is the available quantity, in atoms/satoshi.
	Value() uint64
	// TxID is the ID of the transaction that created the coin.
	TxID() string
}

// TokenCoin extends the Coin interface to include the amount locked
// of the parent asset to be used for fees.
type TokenCoin interface {
	Coin
	Fees() uint64
}

type RecoveryCoin interface {
	// RecoveryID is an ID that can be used to re-establish funding state during
	// startup. If a Coin implements RecoveryCoin, the RecoveryID will be used
	// in the database record, and ultimately passed to the FundingCoins method.
	RecoveryID() util.Bytes
}

// Coins a collection of coins as returned by Fund.
type Coins []Coin

// A GeocodeRedeemer redeems funds from a geocode game.
type GeocodeRedeemer interface {
	RedeemGeocode(code []byte, msg string) (util.Bytes, uint64, error)
}

// WalletNotification can be any asynchronous information the wallet needs
// to convey.
type WalletNotification any

type baseWalletNotification struct {
	AssetID uint32 `json:"assetID"`
	Route   string `json:"route"`
}

// TipChangeNote is the only required wallet notification. All wallets should
// emit a TipChangeNote when a state change occurs that might necessitate swap
// progression or new balance checks.
type TipChangeNote struct {
	baseWalletNotification
	Tip  uint64 `json:"tip"`
	Data any    `json:"data"`
}

// BridgeReadyToCompleteNote is emitted by the wallet that initiated a bridge
// to indicate that the destination wallet can complete the bridge.
type BridgeReadyToCompleteNote struct {
	baseWalletNotification
	DestAssetID        uint32 `json:"destAssetID"`
	InitiateBridgeTxID string `json:"bridgeTxID"`
	Amount             uint64 `json:"amount"`
	Data               []byte `json:"data"`
	BridgeName         string `json:"bridgeName"`
}

// BridgeCompletedNote is emitted by the wallet that completed a bridge to
// notify that the bridge has been completed.
type BridgeCompletedNote struct {
	baseWalletNotification
	SourceAssetID   uint32   `json:"sourceAssetID"`
	InitiationTxID  string   `json:"initiationTxID"`
	CompletionTxIDs []string `json:"completionTxIDs"`
	AmtReceived     uint64   `json:"amtReceived"`
	Fees            uint64   `json:"fees"`
	Complete        bool     `json:"complete"`
}

// BalanceChangeNote can be sent when the wallet detects a balance change
// between tip changes.
type BalanceChangeNote struct {
	baseWalletNotification
	Balance *Balance
}

// TransactionNote is sent when a transaction is made, seen, or updated.
type TransactionNote struct {
	baseWalletNotification
	Transaction *txdb.WalletTransaction `json:"transaction"`
	New         bool                    `json:"new"`
}

// CustomWalletNote is any other information the wallet wishes to convey to
// the user.
type CustomWalletNote struct {
	baseWalletNotification
	Payload any `json:"payload"`
}

type ActionTaker interface {
	// TakeAction processes a response to an ActionRequired wallet notification.
	TakeAction(actionID string, payload []byte) error
}

// WalletEmitter handles a channel for wallet notifications and provides methods
// that generates notifications.
type WalletEmitter struct {
	c       chan<- WalletNotification
	assetID uint32
	log     util.Logger
}

type ActionRequiredNote struct {
	baseWalletNotification
	Payload  any    `json:"payload"`
	UniqueID string `json:"uniqueID"`
	ActionID string `json:"actionID"`
}

type ActionResolvedNote struct {
	baseWalletNotification
	UniqueID string `json:"uniqueID"`
}

// NewWalletEmitter constructs a WalletEmitter for an asset.
func NewWalletEmitter(c chan<- WalletNotification, assetID uint32, log util.Logger) *WalletEmitter {
	return &WalletEmitter{
		c:       c,
		assetID: assetID,
		log:     log,
	}
}

func (e *WalletEmitter) emit(note WalletNotification) {
	select {
	case e.c <- note:
	default:
		e.log.Warn("blocking WalletNotification channel")
	}
}

// Data sends a CustomWalletNote with the specified data payload.
func (e *WalletEmitter) Data(route string, payload any) {
	e.emit(&CustomWalletNote{
		baseWalletNotification: baseWalletNotification{
			AssetID: e.assetID,
			Route:   route,
		}, Payload: payload,
	})
}

// TipChange sends a TipChangeNote with optional extra data.
func (e *WalletEmitter) TipChange(tip uint64, datas ...any) {
	var data any
	if len(datas) > 0 {
		data = datas[0]
	}
	e.emit(&TipChangeNote{
		baseWalletNotification: baseWalletNotification{
			AssetID: e.assetID,
			Route:   "tipChange",
		},
		Tip: tip, Data: data,
	})
}

// BalanceChange sends a BalanceChangeNote.
func (e *WalletEmitter) BalanceChange(bal *Balance) {
	e.emit(&BalanceChangeNote{
		baseWalletNotification: baseWalletNotification{
			AssetID: e.assetID,
			Route:   "balanceChange",
		},
		Balance: bal,
	})
}

// TransactionNote sends a TransactionNote.
func (e *WalletEmitter) TransactionNote(tx *txdb.WalletTransaction, new bool) {
	e.emit(&TransactionNote{
		baseWalletNotification: baseWalletNotification{
			AssetID: e.assetID,
			Route:   "transaction",
		},
		Transaction: tx,
		New:         new,
	})
}

// TransactionHistorySyncedNote sends a TransactionHistorySyncedNote.
func (e *WalletEmitter) TransactionHistorySyncedNote() {
	e.emit(&baseWalletNotification{
		AssetID: e.assetID,
		Route:   "transactionHistorySynced",
	})
}

// ActionRequired is a route that will end up as a special dialogue seeking
// user input via (ActionTaker).TakeAction.
func (e *WalletEmitter) ActionRequired(uniqueID, actionID string, payload any) {
	e.emit(&ActionRequiredNote{
		baseWalletNotification: baseWalletNotification{
			AssetID: e.assetID,
			Route:   "actionRequired",
		},
		UniqueID: uniqueID,
		ActionID: actionID,
		Payload:  payload,
	})
}

// BridgeReadyToComplete is emitted by the wallet which initiated a bridge to
// notify that the destination chain is ready to complete the bridge.
func (e *WalletEmitter) BridgeReadyToComplete(destAssetID uint32, bridgeTxID string, amount uint64, data []byte, bridgeName string) {
	e.emit(&BridgeReadyToCompleteNote{
		baseWalletNotification: baseWalletNotification{
			AssetID: e.assetID,
			Route:   "bridgeReadyToComplete",
		},
		DestAssetID:        destAssetID,
		InitiateBridgeTxID: bridgeTxID,
		Amount:             amount,
		Data:               data,
		BridgeName:         bridgeName,
	})
}

// BridgeCompleted is emitted by the wallet which completed a bridge to
// notify that the bridge has been completed.
func (e *WalletEmitter) BridgeCompleted(sourceAssetID uint32, initiationTxID string, completionTxIDs []string, amtReceived, fees uint64, complete bool) {
	e.emit(&BridgeCompletedNote{
		baseWalletNotification: baseWalletNotification{
			AssetID: e.assetID,
			Route:   "bridgeCompleted",
		},
		SourceAssetID:   sourceAssetID,
		InitiationTxID:  initiationTxID,
		CompletionTxIDs: completionTxIDs,
		AmtReceived:     amtReceived,
		Fees:            fees,
		Complete:        complete,
	})
}

func (e *WalletEmitter) ActionResolved(uniqueID string) {
	e.emit(&ActionResolvedNote{
		baseWalletNotification: baseWalletNotification{
			AssetID: e.assetID,
			Route:   "actionResolved",
		},
		UniqueID: uniqueID,
	})
}

// Sender sends with more granular control parameters
type Sender interface {
	// SendAndLock sends the exact value to the specified address with an unlock time.
	// This can be n blocks or n milliseconds depending on interpretation.
	SendAndLock(address string, value uint64, unlock uint64, feeRate uint64) (Coin, error)
}
