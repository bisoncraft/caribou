// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// RHEA lending storage. Operation records (supply, withdraw, sweep) live in
// their own table keyed by operation ID, with a creation-stamp index for
// newest-first listing and an active index holding the records that still
// need polling (RheaOp.Active), including terminal records whose state was
// locally inferred. Account records hold what this installation has learned
// about each identity's multichain account (MCA): when it was first seen,
// in chain time, and how far its nonce history has been verified. Discovery
// never depends on account records; they only narrow later work.

package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/bisoncraft/caribou/wallet/intents"
)

const (
	rheaOpsTable      = "rheaOps"
	rheaAccountsTable = "rheaAccounts"

	rheaStampIndexName  = "rheaStamp"
	rheaActiveIndexName = "rheaActive"
)

// ErrRheaOpNotFound is returned by RheaOp for an unknown operation ID.
var ErrRheaOpNotFound = util.ErrorKind("rhea op not found")

// RHEA operation kinds.
const (
	RheaOpSupply   = "supply"
	RheaOpWithdraw = "withdraw"
	RheaOpSweep    = "sweep"
)

// RHEA operation states. A supply's 1Click leg uses the intents states
// (QUOTED, DEPOSIT_SENT, the service statuses, EXPIRED, DEPOSIT_FAILED) until
// delivery; a delivered supply is then attributed from receipts. Withdraw
// and sweep start with a signed business submitted through the relayer.
const (
	// RheaStateDelivered: 1Click reports the supply delivered; receipt
	// attribution is pending. Active.
	RheaStateDelivered = "DELIVERED"
	// RheaStateSupplied: receipts show the delivery supplied to Burrow.
	// Terminal.
	RheaStateSupplied = "SUPPLIED"
	// RheaStatePartial: receipts show part of the delivery supplied and
	// the rest refunded into the MCA's plain balance (located, sweepable).
	// Terminal.
	RheaStatePartial = "PARTIAL"
	// RheaStateStranded: tokens attributed to the operation sit in the
	// MCA's plain balance, located and sweepable. Terminal for the
	// operation.
	RheaStateStranded = "STRANDED"
	// RheaStateDeliveryUnresolved: receipts are unavailable, incomplete,
	// or show a refund outside the MCA. Active.
	RheaStateDeliveryUnresolved = "DELIVERY_UNRESOLVED"

	// RheaStateSigned: a business is signed and persisted, not yet
	// submitted. Active.
	RheaStateSigned = "SIGNED"
	// RheaStateSubmitted: the relayer accepted the business. Active.
	RheaStateSubmitted = "SUBMITTED"
	// RheaStateSubmitUnknown: the submission's outcome is unknown. Active.
	RheaStateSubmitUnknown = "SUBMIT_UNKNOWN"
	// RheaStateExecuted: an exec carrying the signed business is
	// attributed, and its evidence has not yet settled the outcome (a
	// sweep whose receipts are missing or contradictory). Active and
	// retried.
	RheaStateExecuted = "EXECUTED"
	// RheaStateDelivering: the withdrawal's 1Click leg is in progress.
	// Active.
	RheaStateDelivering = "DELIVERING"
	// RheaStateWithdrawn: the withdrawal's 1Click leg delivered to the
	// wallet. Terminal.
	RheaStateWithdrawn = "WITHDRAWN"
	// RheaStateNotExecuted: finalized chain state past the deadline shows
	// the signed nonce unconsumed. Terminal.
	RheaStateNotExecuted = "NOT_EXECUTED"
	// RheaStateExecFailed: the attributed exec's child receipts failed in
	// part; moved tokens are recorded. Terminal.
	RheaStateExecFailed = "EXEC_FAILED"
	// RheaStateNonceConsumedUnattributed: the nonce was consumed by an exec
	// that does not carry this operation's signature, or whose exec could
	// not be found. Active.
	RheaStateNonceConsumedUnattributed = "NONCE_CONSUMED_UNATTRIBUTED"
)

// rheaTerminalStates are the states that need no further polling. The
// 1Click terminal statuses and the local EXPIRED and DEPOSIT_FAILED end a
// supply that never delivered. A withdrawal or sweep leg that 1Click
// refunds is not terminal: the refund returns to the MCA and is attributed
// as STRANDED funds (see Terminal).
var rheaTerminalStates = map[string]bool{
	RheaStateSupplied:         true,
	RheaStatePartial:          true,
	RheaStateStranded:         true,
	RheaStateWithdrawn:        true,
	RheaStateNotExecuted:      true,
	RheaStateExecFailed:       true,
	IntentsStateExpired:       true,
	IntentsStateDepositFailed: true,
	intents.StatusRefunded:    true,
	intents.StatusFailed:      true,
}

// RheaPendingBusinessStates are the states in which a signed business may
// still be admitted by its MCA.
var RheaPendingBusinessStates = map[string]bool{
	RheaStateSigned:        true,
	RheaStateSubmitted:     true,
	RheaStateSubmitUnknown: true,
}

// RheaLeg is an operation's 1Click leg. Amounts are the quote's base-unit
// strings for the leg's assets.
type RheaLeg struct {
	DepositAddress string `json:"depositAddress"`
	DepositMemo    string `json:"depositMemo,omitempty"`
	OriginAsset    string `json:"originAsset"`
	DestAsset      string `json:"destAsset"`
	AmountIn       string `json:"amountIn"`
	AmountOut      string `json:"amountOut"`
	MinAmountOut   string `json:"minAmountOut,omitempty"`
	// Recipient and CustomRecipientMsg are where and how the leg pays out;
	// RefundTo is where 1Click refunds the deposit.
	Recipient          string          `json:"recipient,omitempty"`
	CustomRecipientMsg string          `json:"customRecipientMsg,omitempty"`
	RefundTo           string          `json:"refundTo,omitempty"`
	Deadline           time.Time       `json:"deadline"`
	QuoteEnvelope      json.RawMessage `json:"quoteEnvelope,omitempty"`
	DepositTxID        string          `json:"depositTxID,omitempty"`
	// DepositTxFee is the origin network's fee for the deposit, in the
	// origin chain's parent-asset atoms; USDDepositFee its value at send.
	DepositTxFee  uint64   `json:"depositTxFee,omitempty"`
	USDDepositFee float64  `json:"usdDepositFee,omitempty"`
	Status        string   `json:"status,omitempty"`
	RefundReason  string   `json:"refundReason,omitempty"`
	NearTxHashes  []string `json:"nearTxHashes,omitempty"`
	// Received marks a delivery whose receipts show the recipient kept some
	// of the tokens. A later 1Click refund report then contradicts the
	// chain and is not adopted.
	Received bool `json:"received,omitempty"`
	// VerifierRefunded marks a delivery whose receipts, in a unique, final,
	// fully reported transaction, show the verifier → recipient transfer
	// returned in full: the evidence required before a later 1Click
	// refund report is adopted. It holds only for the transaction set it
	// was established on: every attribution recomputes it, and a changed
	// transaction list clears it.
	VerifierRefunded bool `json:"verifierRefunded,omitempty"`
	// Inferred and InferredFrom carry the intents engine's
	// locally-inferred terminal state semantics (see IntentsSwap).
	Inferred     bool   `json:"inferred,omitempty"`
	InferredFrom string `json:"inferredFrom,omitempty"`
}

// RheaRelayerLeg is an operation's signed business and its submission.
type RheaRelayerLeg struct {
	// SignedRequest is the exact request JSON submitted to the relayer,
	// persisted before any submission.
	SignedRequest json.RawMessage `json:"signedRequest"`
	Nonce         uint64          `json:"nonce"`
	// Deadline is the business deadline (chain time).
	Deadline time.Time `json:"deadline"`
	BatchID  string    `json:"batchID,omitempty"`
	TxHash   string    `json:"txHash,omitempty"`
	// RelayerError is the relayer's error text, if any. It is not evidence
	// that nothing was submitted.
	RelayerError string `json:"relayerError,omitempty"`
	// Unconfirmed marks a NOT_EXECUTED conclusion that rests on the MCA's
	// nonce and deadline semantics (a stale or expired business cannot
	// execute), which a funded test has yet to confirm.
	Unconfirmed bool `json:"unconfirmed,omitempty"`
}

// RheaRefund is an amount attributed to a refund and the account it went
// to.
type RheaRefund struct {
	Account string `json:"account"`
	Amount  string `json:"amount"`
}

// RheaOp is a persisted lending operation. Token amounts are decimal
// strings in token base units unless the field says internal (Burrow
// internal units).
type RheaOp struct {
	ID                string `json:"id"`
	Kind              string `json:"kind"`
	State             string `json:"state"`
	AccountManager    string `json:"accountManager"`
	MCA               string `json:"mca,omitempty"`
	Identity          string `json:"identity"`
	DerivationVersion uint32 `json:"derivationVersion"`
	TokenID           string `json:"tokenID"`
	// WalletAssetID is the supply's source or the withdrawal's
	// destination wallet asset.
	WalletAssetID uint32 `json:"walletAssetID"`

	Amount        string `json:"amount,omitempty"`
	Fee           string `json:"fee,omitempty"`
	GrossInternal string `json:"grossInternal,omitempty"`
	// GasToken is the token a sweep's GasPayment fee is paid in ("near"
	// for native NEAR); Fee is then in that token's base units.
	GasToken string `json:"gasToken,omitempty"`
	// Covers lists the operations whose STRANDED funds a sweep moves: those
	// recorded in the same MCA and token when the sweep was signed, all of
	// them within the swept amount. Once the sweep's exec moved its tokens,
	// the covered records no longer count as funds in the MCA.
	Covers []string `json:"covers,omitempty"`

	// Creation marks a supply that creates the MCA; CreationFee is the
	// fee previewed for it and MinDelivered the delivery floor the user
	// accepted, both in token base units.
	Creation     bool   `json:"creation,omitempty"`
	CreationFee  string `json:"creationFee,omitempty"`
	MinDelivered string `json:"minDelivered,omitempty"`

	Leg     *RheaLeg        `json:"leg,omitempty"`
	Relayer *RheaRelayerLeg `json:"relayer,omitempty"`

	Delivered string       `json:"delivered,omitempty"`
	Supplied  string       `json:"supplied,omitempty"`
	Fees      string       `json:"fees,omitempty"`
	Stranded  string       `json:"stranded,omitempty"`
	Refunds   []RheaRefund `json:"refunds,omitempty"`

	USDIn  float64 `json:"usdIn,omitempty"`
	USDOut float64 `json:"usdOut,omitempty"`

	Notes     string    `json:"notes,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Terminal reports whether the operation's state is final. A withdrawal
// or sweep leg that 1Click refunded is not: the refund still has to be
// attributed. A terminal state the engine inferred locally (Leg.Inferred)
// can still be superseded; see Active.
func (op *RheaOp) Terminal() bool {
	if op.Kind != RheaOpSupply && op.State == intents.StatusRefunded {
		return false
	}
	return rheaTerminalStates[op.State]
}

// Active reports whether the operation needs polling: every non-terminal
// operation, plus a terminal one whose 1Click leg state was inferred
// locally (a deadline timeout) rather than reported by the service. The
// service can still settle or refund such a leg, so the operation stays
// in the active set until a service-reported status supersedes the
// inference or the engine retires it (RetireInference).
func (op *RheaOp) Active() bool {
	return !op.Terminal() || (op.Leg != nil && op.Leg.Inferred)
}

// RetireInference makes a locally inferred terminal leg state final,
// dropping the operation from the active set on its next store. The engine
// calls it once the service-correction window has closed with no
// superseding status.
func (op *RheaOp) RetireInference() {
	if op.Leg != nil {
		op.Leg.Inferred = false
		op.Leg.InferredFrom = ""
	}
}

const rheaRecordVersion = 0

// MarshalBinary encodes the stored record: version(1) || JSON.
func (op *RheaOp) MarshalBinary() ([]byte, error) {
	b, err := json.Marshal(op)
	if err != nil {
		return nil, fmt.Errorf("error encoding rhea op: %w", err)
	}
	return append([]byte{rheaRecordVersion}, b...), nil
}

func decodeRheaRecord(b []byte, v any) error {
	if len(b) < 1 {
		return errors.New("empty rhea record")
	}
	if ver := b[0]; ver != rheaRecordVersion {
		return fmt.Errorf("unknown rhea record version %d", ver)
	}
	return json.Unmarshal(b[1:], v)
}

func rheaOpV(vI kvstore.KV) (*RheaOp, error) {
	op, ok := vI.(*RheaOp)
	if !ok {
		return nil, fmt.Errorf("expected *RheaOp value, got %T", vI)
	}
	return op, nil
}

func rheaStampIndexFunc(_, vI kvstore.KV) ([]byte, error) {
	op, err := rheaOpV(vI)
	if err != nil {
		return nil, err
	}
	return uint64Bytes(uint64(op.CreatedAt.UnixMilli())), nil
}

func rheaActiveIndexFunc(_, vI kvstore.KV) ([]byte, error) {
	op, err := rheaOpV(vI)
	if err != nil {
		return nil, err
	}
	if !op.Active() {
		return nil, kvstore.ErrNotIndexed
	}
	return uint64Bytes(uint64(op.CreatedAt.UnixMilli())), nil
}

// StoreRheaOp stores the operation under its ID, stamping UpdatedAt and
// replacing any existing record. CreatedAt orders the listing and must not
// change across re-stores.
func (d *kvDB) StoreRheaOp(op *RheaOp) error {
	if op.ID == "" {
		return errors.New("no operation ID")
	}
	op.UpdatedAt = time.Now()
	return d.rheaOps.Set([]byte(op.ID), op, kvstore.WithReplace())
}

// RheaOp fetches an operation by ID. It is ErrRheaOpNotFound if no record
// exists.
func (d *kvDB) RheaOp(id string) (*RheaOp, error) {
	b, err := d.rheaOps.Get([]byte(id))
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, ErrRheaOpNotFound
		}
		return nil, err
	}
	op := new(RheaOp)
	if err := decodeRheaRecord(b, op); err != nil {
		return nil, fmt.Errorf("error decoding rhea op: %w", err)
	}
	return op, nil
}

func readRheaIndex(idx kvstore.StoreIndex, reverse bool) ([]*RheaOp, error) {
	var opts []kvstore.IterationOption
	if reverse {
		opts = append(opts, kvstore.WithReverse())
	}
	var ops []*RheaOp
	err := idx.Iterate(nil, func(it kvstore.StoreIter) error {
		return it.V(func(v []byte) error {
			op := new(RheaOp)
			if err := decodeRheaRecord(v, op); err != nil {
				return fmt.Errorf("error decoding rhea op: %w", err)
			}
			ops = append(ops, op)
			return nil
		})
	}, opts...)
	return ops, err
}

// RheaOps returns every operation, newest first.
func (d *kvDB) RheaOps() ([]*RheaOp, error) {
	return readRheaIndex(d.rheaStampIdx, true)
}

// ActiveRheaOps returns the operations that need polling (see
// RheaOp.Active), oldest first.
func (d *kvDB) ActiveRheaOps() ([]*RheaOp, error) {
	return readRheaIndex(d.rheaActiveIdx, false)
}

// RheaAccount is what this installation knows about an identity's MCA on
// one account manager.
type RheaAccount struct {
	AccountManager    string `json:"accountManager"`
	Identity          string `json:"identity"`
	DerivationVersion uint32 `json:"derivationVersion"`
	MCA               string `json:"mca"`
	// FirstSeenHeight, FirstSeenHash, and FirstSeenTime name the final
	// block at which this installation first saw the MCA bound to the
	// identity, whether it discovered or created it. The time is the block
	// timestamp, never the local clock.
	FirstSeenHeight uint64    `json:"firstSeenHeight"`
	FirstSeenHash   string    `json:"firstSeenHash"`
	FirstSeenTime   time.Time `json:"firstSeenTime"`
	// VerifiedBelow is the nonce checkpoint: every business the MCA admitted
	// with a nonce below it has been found as an exec on the MCA whose
	// receipt tree was final and fully reported. Zero means nothing is
	// verified, which is also the state of an MCA that has never admitted a
	// business. A lost database restarts the walk from zero.
	VerifiedBelow uint64 `json:"verifiedBelow"`
	// LastLookup is the local time of the last discovery that confirmed the
	// binding.
	LastLookup time.Time `json:"lastLookup"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// MarshalBinary encodes the stored record: version(1) || JSON.
func (a *RheaAccount) MarshalBinary() ([]byte, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("error encoding rhea account: %w", err)
	}
	return append([]byte{rheaRecordVersion}, b...), nil
}

// rheaAccountKey keys an account record by account manager, derivation
// version, and identity.
func rheaAccountKey(am string, version uint32, identity string) []byte {
	return []byte(am + "/" + strconv.FormatUint(uint64(version), 10) + "/" + identity)
}

// StoreRheaAccount stores the account record, stamping UpdatedAt and
// replacing any existing record for the same account manager, derivation
// version, and identity.
func (d *kvDB) StoreRheaAccount(a *RheaAccount) error {
	if a.AccountManager == "" || a.Identity == "" || a.MCA == "" {
		return errors.New("account record lacks an account manager, identity, or MCA")
	}
	a.UpdatedAt = time.Now()
	return d.rheaAccounts.Set(rheaAccountKey(a.AccountManager, a.DerivationVersion, a.Identity), a, kvstore.WithReplace())
}

// RheaAccount fetches the account record for an account manager,
// derivation version, and identity. It is ErrRheaAccountNotFound if none
// is stored.
func (d *kvDB) RheaAccount(am string, version uint32, identity string) (*RheaAccount, error) {
	b, err := d.rheaAccounts.Get(rheaAccountKey(am, version, identity))
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, ErrRheaAccountNotFound
		}
		return nil, err
	}
	a := new(RheaAccount)
	if err := decodeRheaRecord(b, a); err != nil {
		return nil, fmt.Errorf("error decoding rhea account: %w", err)
	}
	return a, nil
}

// ErrRheaAccountNotFound is returned by RheaAccount when no record is
// stored.
var ErrRheaAccountNotFound = util.ErrorKind("rhea account not found")
