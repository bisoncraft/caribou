// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package rhea

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// FinalExecutionFinal is the final_execution_status of a transaction whose
// receipts are all executed and in final blocks.
const FinalExecutionFinal = "FINAL"

// StatusKind classifies an execution outcome.
type StatusKind int

const (
	// StatusUnknown is an outcome not yet known (not executed, or not
	// reported).
	StatusUnknown StatusKind = iota
	// StatusSuccessValue is success with a return value.
	StatusSuccessValue
	// StatusSuccessReceiptID is success whose result is another receipt's.
	StatusSuccessReceiptID
	// StatusFailure is a failed execution. A failed receipt's state changes
	// are reverted; its logs are not evidence of anything.
	StatusFailure
)

// Status is an execution outcome.
type Status struct {
	Kind StatusKind
	// Value is the decoded return value of StatusSuccessValue.
	Value []byte
	// ReceiptID is the receipt of StatusSuccessReceiptID.
	ReceiptID string
	// Failure is the raw error of StatusFailure.
	Failure json.RawMessage
}

// OK reports whether the outcome is a success.
func (s *Status) OK() bool {
	return s.Kind == StatusSuccessValue || s.Kind == StatusSuccessReceiptID
}

func (s *Status) UnmarshalJSON(b []byte) error {
	*s = Status{}
	if len(b) > 0 && b[0] == '"' {
		return nil // "Unknown" and any other bare-string status
	}
	var v struct {
		SuccessValue     *string         `json:"SuccessValue"`
		SuccessReceiptID *string         `json:"SuccessReceiptId"`
		Failure          json.RawMessage `json:"Failure"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("error decoding outcome status: %w", err)
	}
	switch {
	case v.Failure != nil:
		s.Kind, s.Failure = StatusFailure, v.Failure
	case v.SuccessValue != nil:
		val, err := base64.StdEncoding.DecodeString(*v.SuccessValue)
		if err != nil {
			return fmt.Errorf("bad success value: %w", err)
		}
		s.Kind, s.Value = StatusSuccessValue, val
	case v.SuccessReceiptID != nil:
		s.Kind, s.ReceiptID = StatusSuccessReceiptID, *v.SuccessReceiptID
	}
	return nil
}

// Action is a transaction or receipt action. Kind is the action variant
// ("FunctionCall", "Transfer", ...); the remaining fields are set for the
// variants that carry them.
type Action struct {
	Kind       string
	MethodName string
	// Args are the decoded call arguments.
	Args []byte
	Gas  uint64
	// Deposit is the attached deposit of a FunctionCall or the amount of a
	// Transfer, in yoctoNEAR.
	Deposit *big.Int
}

func (a *Action) UnmarshalJSON(b []byte) error {
	*a = Action{}
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &a.Kind) // unit variants like "CreateAccount"
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("error decoding action: %w", err)
	}
	if len(m) != 1 {
		return fmt.Errorf("action has %d variants", len(m))
	}
	for k, v := range m {
		a.Kind = k
		switch k {
		case "FunctionCall":
			var fc struct {
				MethodName string `json:"method_name"`
				Args       string `json:"args"`
				Gas        uint64 `json:"gas"`
				Deposit    string `json:"deposit"`
			}
			if err := json.Unmarshal(v, &fc); err != nil {
				return fmt.Errorf("error decoding function call: %w", err)
			}
			args, err := base64.StdEncoding.DecodeString(fc.Args)
			if err != nil {
				return fmt.Errorf("bad function call args: %w", err)
			}
			if a.Deposit, err = parseAmount(fc.Deposit); err != nil {
				return err
			}
			a.MethodName, a.Args, a.Gas = fc.MethodName, args, fc.Gas
		case "Transfer":
			var t struct {
				Deposit string `json:"deposit"`
			}
			if err := json.Unmarshal(v, &t); err != nil {
				return fmt.Errorf("error decoding transfer: %w", err)
			}
			var err error
			if a.Deposit, err = parseAmount(t.Deposit); err != nil {
				return err
			}
		}
	}
	return nil
}

// Outcome is the execution outcome of a transaction or receipt.
type Outcome struct {
	ID         string
	BlockHash  string
	ExecutorID string
	Logs       []string
	// ReceiptIDs are the receipts this execution created.
	ReceiptIDs []string
	Status     Status
}

type rpcOutcome struct {
	ID        string `json:"id"`
	BlockHash string `json:"block_hash"`
	Outcome   struct {
		ExecutorID string   `json:"executor_id"`
		Logs       []string `json:"logs"`
		ReceiptIDs []string `json:"receipt_ids"`
		Status     Status   `json:"status"`
	} `json:"outcome"`
}

func (o *rpcOutcome) outcome() *Outcome {
	return &Outcome{
		ID:         o.ID,
		BlockHash:  o.BlockHash,
		ExecutorID: o.Outcome.ExecutorID,
		Logs:       o.Outcome.Logs,
		ReceiptIDs: o.Outcome.ReceiptIDs,
		Status:     o.Outcome.Status,
	}
}

// Receipt kinds.
const (
	ReceiptKindAction = "Action"
	ReceiptKindData   = "Data"
)

// Receipt is a receipt and its outcome.
type Receipt struct {
	ID            string
	PredecessorID string
	ReceiverID    string
	// Kind is the receipt variant: ReceiptKindAction, ReceiptKindData (a
	// promise result, with no actions), or another variant name. Empty when
	// the endpoint reported the receipt's outcome but not the receipt.
	Kind string
	// Actions are an action receipt's actions.
	Actions []*Action
	// Outcome is nil when the endpoint reported no outcome for the
	// receipt.
	Outcome *Outcome
}

// TxTree is a transaction with every receipt and outcome as reported by
// EXPERIMENTAL_tx_status.
//
// Two separate properties decide what a tree can prove. Complete reports
// whether execution is over: the transaction is FINAL and every reachable
// receipt has a known outcome, so logs and events are final.
// EvidenceAvailable reports whether the endpoint also reported every
// executed receipt with its actions, which the call-based extractors
// (ResolvedTransfers, ExecCalls) need: without it they cannot tell a call
// that did not happen from one that was not reported.
type TxTree struct {
	Hash                 string
	SignerID             string
	ReceiverID           string
	FinalExecutionStatus string
	Actions              []*Action
	Outcome              *Outcome

	receipts map[string]*Receipt
	order    []string
}

func noNilActions(actions []*Action, where string) error {
	for i, a := range actions {
		if a == nil {
			return fmt.Errorf("%s action %d is null", where, i)
		}
	}
	return nil
}

// DecodeTxTree decodes an EXPERIMENTAL_tx_status result. Null array
// elements are errors, never absent entries.
//
// A transaction whose signer is also its receiver executes its first
// receipt locally, and endpoints omit that local receipt from the receipt
// list. Its actions are the transaction's actions, so it is reconstructed
// from the transaction.
func DecodeTxTree(raw []byte) (*TxTree, error) {
	var v struct {
		FinalExecutionStatus string `json:"final_execution_status"`
		Transaction          struct {
			Hash       string    `json:"hash"`
			SignerID   string    `json:"signer_id"`
			ReceiverID string    `json:"receiver_id"`
			Actions    []*Action `json:"actions"`
		} `json:"transaction"`
		TransactionOutcome *rpcOutcome   `json:"transaction_outcome"`
		ReceiptsOutcome    []*rpcOutcome `json:"receipts_outcome"`
		Receipts           []*struct {
			ReceiptID     string                     `json:"receipt_id"`
			PredecessorID string                     `json:"predecessor_id"`
			ReceiverID    string                     `json:"receiver_id"`
			Receipt       map[string]json.RawMessage `json:"receipt"`
		} `json:"receipts"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("error decoding transaction status: %w", err)
	}
	tx := v.Transaction
	if tx.Hash == "" {
		return nil, errors.New("transaction status lacks a transaction")
	}
	if err := noNilActions(tx.Actions, "transaction"); err != nil {
		return nil, err
	}
	t := &TxTree{
		Hash:                 tx.Hash,
		SignerID:             tx.SignerID,
		ReceiverID:           tx.ReceiverID,
		FinalExecutionStatus: v.FinalExecutionStatus,
		Actions:              tx.Actions,
		receipts:             make(map[string]*Receipt),
	}
	if v.TransactionOutcome != nil {
		t.Outcome = v.TransactionOutcome.outcome()
	}
	for i, r := range v.Receipts {
		if r == nil || r.ReceiptID == "" {
			return nil, fmt.Errorf("receipt %d is null or lacks an ID", i)
		}
		if t.receipts[r.ReceiptID] != nil {
			return nil, fmt.Errorf("duplicate receipt %s", r.ReceiptID)
		}
		if len(r.Receipt) != 1 {
			return nil, fmt.Errorf("receipt %s has %d variants", r.ReceiptID, len(r.Receipt))
		}
		rc := &Receipt{ID: r.ReceiptID, PredecessorID: r.PredecessorID, ReceiverID: r.ReceiverID}
		for kind, body := range r.Receipt {
			rc.Kind = kind
			if kind != ReceiptKindAction {
				continue
			}
			var action *struct {
				Actions []*Action `json:"actions"`
			}
			if err := json.Unmarshal(body, &action); err != nil {
				return nil, fmt.Errorf("error decoding receipt %s: %w", r.ReceiptID, err)
			}
			if action == nil || action.Actions == nil {
				return nil, fmt.Errorf("action receipt %s lacks actions", r.ReceiptID)
			}
			if err := noNilActions(action.Actions, "receipt "+r.ReceiptID); err != nil {
				return nil, err
			}
			rc.Actions = action.Actions
		}
		t.receipts[r.ReceiptID] = rc
	}
	if t.Outcome != nil && tx.SignerID != "" && tx.SignerID == tx.ReceiverID {
		for _, id := range t.Outcome.ReceiptIDs {
			if t.receipts[id] == nil {
				t.receipts[id] = &Receipt{
					ID: id, PredecessorID: tx.SignerID, ReceiverID: tx.ReceiverID,
					Kind: ReceiptKindAction, Actions: tx.Actions,
				}
			}
		}
	}
	for i, o := range v.ReceiptsOutcome {
		if o == nil || o.ID == "" {
			return nil, fmt.Errorf("receipt outcome %d is null or lacks an ID", i)
		}
		rc := t.receipts[o.ID]
		if rc == nil {
			rc = &Receipt{ID: o.ID, ReceiverID: o.Outcome.ExecutorID}
			t.receipts[o.ID] = rc
		}
		if rc.Outcome != nil {
			return nil, fmt.Errorf("duplicate outcome for receipt %s", o.ID)
		}
		rc.Outcome = o.outcome()
		t.order = append(t.order, o.ID)
	}
	return t, nil
}

// Receipts returns the receipts that have outcomes, in reported order.
func (t *TxTree) Receipts() []*Receipt {
	rs := make([]*Receipt, 0, len(t.order))
	for _, id := range t.order {
		rs = append(rs, t.receipts[id])
	}
	return rs
}

// Receipt returns a receipt by ID, or nil.
func (t *TxTree) Receipt(id string) *Receipt {
	return t.receipts[id]
}

// Complete returns nil when the endpoint reports the transaction FINAL and
// every receipt reachable from the transaction outcome has a known
// outcome. Otherwise it describes what is missing; the tree may still
// change. Complete says nothing about whether receipts' actions were
// reported; see EvidenceAvailable.
func (t *TxTree) Complete() error {
	if t.FinalExecutionStatus != FinalExecutionFinal {
		return fmt.Errorf("execution status %q, not %s", t.FinalExecutionStatus, FinalExecutionFinal)
	}
	if t.Outcome == nil || t.Outcome.Status.Kind == StatusUnknown {
		return errors.New("transaction outcome unknown")
	}
	seen := make(map[string]bool)
	queue := append([]string(nil), t.Outcome.ReceiptIDs...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		r := t.receipts[id]
		if r == nil || r.Outcome == nil {
			return fmt.Errorf("receipt %s has no outcome", id)
		}
		if r.Outcome.Status.Kind == StatusUnknown {
			return fmt.Errorf("receipt %s outcome unknown", id)
		}
		queue = append(queue, r.Outcome.ReceiptIDs...)
	}
	return nil
}

// EvidenceAvailable returns nil when every receipt with an outcome was
// also reported as a receipt (with its actions, for action receipts). The
// call-based extractors require it.
func (t *TxTree) EvidenceAvailable() error {
	for _, id := range t.order {
		if t.receipts[id].Kind == "" {
			return fmt.Errorf("receipt %s was not reported, only its outcome", id)
		}
	}
	return nil
}

// Subtree returns the IDs of the receipt root and every receipt it created,
// directly or through descendants, as reported by their outcomes. Evidence
// for one call is read from its subtree so sibling work in the same
// transaction is never attributed to it.
func (t *TxTree) Subtree(root string) map[string]bool {
	sub := make(map[string]bool)
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if sub[id] {
			continue
		}
		sub[id] = true
		if r := t.receipts[id]; r != nil && r.Outcome != nil {
			queue = append(queue, r.Outcome.ReceiptIDs...)
		}
	}
	return sub
}

// Event is a NEP-297 event logged by a successful receipt.
type Event struct {
	ReceiptID string
	// Contract is the account that logged the event.
	Contract string
	Standard string
	Version  string
	Event    string
	Data     json.RawMessage
}

const eventPrefix = "EVENT_JSON:"

// Events returns the events logged by successful receipts, in reported
// order. Logs of failed receipts are ignored: their state changes were
// reverted. Events come from outcomes, so they are final once Complete
// returns nil; the event extractors (FTTransfers, BurrowEvents,
// VerifierWithdrawals, AccountManagerFees) inherit that.
func (t *TxTree) Events() []*Event {
	var evs []*Event
	for _, r := range t.Receipts() {
		if !r.Outcome.Status.OK() {
			continue
		}
		for _, l := range r.Outcome.Logs {
			s, ok := strings.CutPrefix(l, eventPrefix)
			if !ok {
				continue
			}
			var e struct {
				Standard string          `json:"standard"`
				Version  string          `json:"version"`
				Event    string          `json:"event"`
				Data     json.RawMessage `json:"data"`
			}
			if json.Unmarshal([]byte(s), &e) != nil || e.Standard == "" || e.Event == "" {
				continue
			}
			evs = append(evs, &Event{
				ReceiptID: r.ID,
				Contract:  r.Outcome.ExecutorID,
				Standard:  e.Standard,
				Version:   e.Version,
				Event:     e.Event,
				Data:      e.Data,
			})
		}
	}
	return evs
}

// eventItems decodes each data item of matching events into a new T,
// passing it to f.
func eventItems[T any](t *TxTree, contract, standard, event string, f func(*Event, *T) error) error {
	for _, e := range t.Events() {
		if e.Standard != standard || (event != "" && e.Event != event) || (contract != "" && e.Contract != contract) {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(e.Data, &items); err != nil {
			return fmt.Errorf("%s %s event in receipt %s: %w", standard, e.Event, e.ReceiptID, err)
		}
		if items == nil {
			return fmt.Errorf("%s %s event in receipt %s has no data", standard, e.Event, e.ReceiptID)
		}
		for _, it := range items {
			if bytes.Equal(bytes.TrimSpace(it), []byte("null")) {
				return fmt.Errorf("%s %s event in receipt %s has a null item", standard, e.Event, e.ReceiptID)
			}
			v := new(T)
			if err := json.Unmarshal(it, v); err != nil {
				return fmt.Errorf("%s %s event in receipt %s: %w", standard, e.Event, e.ReceiptID, err)
			}
			if err := f(e, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// FTTransfer is a NEP-141 ft_transfer event: Amount of Token moved From →
// To.
type FTTransfer struct {
	ReceiptID string
	Token     string
	From      string
	To        string
	Amount    *big.Int
	Memo      string
}

// FTTransfers returns the NEP-141 transfers of successful receipts.
func (t *TxTree) FTTransfers() ([]*FTTransfer, error) {
	type item struct {
		OldOwnerID string `json:"old_owner_id"`
		NewOwnerID string `json:"new_owner_id"`
		Amount     string `json:"amount"`
		Memo       string `json:"memo"`
	}
	var out []*FTTransfer
	err := eventItems(t, "", "nep141", "ft_transfer", func(e *Event, it *item) error {
		amt, err := parseAmount(it.Amount)
		if err != nil {
			return err
		}
		out = append(out, &FTTransfer{
			ReceiptID: e.ReceiptID, Token: e.Contract, From: it.OldOwnerID, To: it.NewOwnerID, Amount: amt, Memo: it.Memo,
		})
		return nil
	})
	return out, err
}

// BurrowEvent is a Burrow event with an account, token, and internal-unit
// amount: deposit, withdraw_started, withdraw_succeeded,
// increase_collateral, and similar.
type BurrowEvent struct {
	ReceiptID string
	Event     string
	AccountID string
	TokenID   string
	Amount    *big.Int
}

// Burrow event names.
const (
	BurrowEventDeposit           = "deposit"
	BurrowEventWithdrawStarted   = "withdraw_started"
	BurrowEventWithdrawSucceeded = "withdraw_succeeded"
	BurrowEventWithdrawFailed    = "withdraw_failed"
)

// BurrowEvents returns the account events Burrow logged in successful
// receipts. Events without an account, token, and amount (fee_detail) are
// skipped.
func (t *TxTree) BurrowEvents(burrow string) ([]*BurrowEvent, error) {
	type item struct {
		AccountID string `json:"account_id"`
		TokenID   string `json:"token_id"`
		Amount    string `json:"amount"`
	}
	var out []*BurrowEvent
	err := eventItems(t, burrow, "burrow", "", func(e *Event, it *item) error {
		if it.AccountID == "" || it.TokenID == "" || it.Amount == "" {
			return nil
		}
		amt, err := parseAmount(it.Amount)
		if err != nil {
			return err
		}
		out = append(out, &BurrowEvent{ReceiptID: e.ReceiptID, Event: e.Event, AccountID: it.AccountID, TokenID: it.TokenID, Amount: amt})
		return nil
	})
	return out, err
}

// VerifierWithdrawal is an intents verifier ft_withdraw event: the
// verifier paid Amount of Token out of AccountID's balance to ReceiverID,
// with Msg when the payout was an ft_transfer_call.
type VerifierWithdrawal struct {
	ReceiptID  string
	AccountID  string
	Token      string
	ReceiverID string
	Amount     *big.Int
	Msg        string
}

// VerifierWithdrawals returns the verifier's ft_withdraw events.
func (t *TxTree) VerifierWithdrawals(verifier string) ([]*VerifierWithdrawal, error) {
	type item struct {
		AccountID  string `json:"account_id"`
		Token      string `json:"token"`
		ReceiverID string `json:"receiver_id"`
		Amount     string `json:"amount"`
		Msg        string `json:"msg"`
	}
	var out []*VerifierWithdrawal
	err := eventItems(t, verifier, "dip4", "ft_withdraw", func(e *Event, it *item) error {
		amt, err := parseAmount(it.Amount)
		if err != nil {
			return err
		}
		out = append(out, &VerifierWithdrawal{
			ReceiptID: e.ReceiptID, AccountID: it.AccountID, Token: it.Token, ReceiverID: it.ReceiverID, Amount: amt, Msg: it.Msg,
		})
		return nil
	})
	return out, err
}

// AccountManagerFee is a fee the account manager collected from a supply
// (collect_create_mca_fee, collect_dapp_registration_fee), in token units.
type AccountManagerFee struct {
	ReceiptID string
	Event     string
	TokenID   string
	Amount    *big.Int
}

// AccountManagerFees returns the account manager's fee events.
func (t *TxTree) AccountManagerFees(am string) ([]*AccountManagerFee, error) {
	type item struct {
		TokenID string `json:"token_id"`
		Amount  string `json:"amount"`
	}
	var out []*AccountManagerFee
	err := eventItems(t, am, "account_manager", "", func(e *Event, it *item) error {
		if !strings.HasPrefix(e.Event, "collect_") {
			return nil
		}
		amt, err := parseAmount(it.Amount)
		if err != nil {
			return err
		}
		out = append(out, &AccountManagerFee{ReceiptID: e.ReceiptID, Event: e.Event, TokenID: it.TokenID, Amount: amt})
		return nil
	})
	return out, err
}

// ResolvedTransfer is the resolution of an ft_transfer_call: of Amount sent
// SenderID → ReceiverID, the receiver kept Used; the rest was refunded to
// SenderID.
type ResolvedTransfer struct {
	ReceiptID  string
	Token      string
	SenderID   string
	ReceiverID string
	Amount     *big.Int
	Used       *big.Int
}

// Refunded is the amount returned to the sender.
func (r *ResolvedTransfer) Refunded() *big.Int {
	return new(big.Int).Sub(r.Amount, r.Used)
}

// ResolvedTransfers returns every ft_resolve_transfer with its used
// amount. It fails when the tree's evidence is not fully available (see
// EvidenceAvailable) and when any resolution did not complete: the
// transfer's outcome is then not known. An empty result means no
// ft_transfer_call was resolved in the reported tree; attribution that
// expects a particular hop must find that hop's resolution and treat its
// absence as missing evidence, not as full use.
func (t *TxTree) ResolvedTransfers() ([]*ResolvedTransfer, error) {
	if err := t.EvidenceAvailable(); err != nil {
		return nil, err
	}
	var out []*ResolvedTransfer
	for _, r := range t.Receipts() {
		for _, a := range r.Actions {
			if a.Kind != "FunctionCall" || a.MethodName != "ft_resolve_transfer" {
				continue
			}
			if r.Outcome.Status.Kind != StatusSuccessValue {
				return nil, fmt.Errorf("ft_resolve_transfer receipt %s did not complete", r.ID)
			}
			var args struct {
				SenderID   string `json:"sender_id"`
				ReceiverID string `json:"receiver_id"`
				Amount     string `json:"amount"`
			}
			if err := json.Unmarshal(a.Args, &args); err != nil {
				return nil, fmt.Errorf("bad ft_resolve_transfer args in %s: %w", r.ID, err)
			}
			var usedStr string
			if err := json.Unmarshal(r.Outcome.Status.Value, &usedStr); err != nil {
				return nil, fmt.Errorf("bad ft_resolve_transfer result in %s: %w", r.ID, err)
			}
			amt, err := parseAmount(args.Amount)
			if err != nil {
				return nil, err
			}
			used, err := parseAmount(usedStr)
			if err != nil {
				return nil, err
			}
			if used.Cmp(amt) > 0 {
				return nil, fmt.Errorf("ft_resolve_transfer in %s used %s of %s", r.ID, used, amt)
			}
			out = append(out, &ResolvedTransfer{
				ReceiptID: r.ID, Token: r.ReceiverID, SenderID: args.SenderID, ReceiverID: args.ReceiverID, Amount: amt, Used: used,
			})
		}
	}
	return out, nil
}

// ExecCall is an exec call on an MCA: a signed business and the outcome of
// the receipt that ran it.
type ExecCall struct {
	ReceiptID     string
	MCA           string
	PredecessorID string
	Args          *ExecArgs
	Deposit       *big.Int
	Outcome       *Outcome
}

// ExecCalls returns the exec calls on MCAs of the deployment. It fails when
// the tree's evidence is not fully available (see EvidenceAvailable).
func (t *TxTree) ExecCalls(dep *Deployment) ([]*ExecCall, error) {
	if err := t.EvidenceAvailable(); err != nil {
		return nil, err
	}
	var out []*ExecCall
	for _, r := range t.Receipts() {
		if !dep.IsMCA(r.ReceiverID) {
			continue
		}
		for _, a := range r.Actions {
			if a.Kind != "FunctionCall" || a.MethodName != "exec" {
				continue
			}
			args, err := ParseExecArgs(a.Args)
			if err != nil {
				return nil, fmt.Errorf("exec in receipt %s: %w", r.ID, err)
			}
			out = append(out, &ExecCall{
				ReceiptID: r.ID, MCA: r.ReceiverID, PredecessorID: r.PredecessorID, Args: args, Deposit: a.Deposit, Outcome: r.Outcome,
			})
		}
	}
	return out, nil
}
