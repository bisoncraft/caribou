// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package rhea

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"
)

// Business is the payload an MCA executes on its identity's signature. The
// signature is EIP-191 over the business's canonical JSON (see Canonical);
// the MCA re-serializes the business it parses and verifies against that,
// so field order and encoding must match exactly. The business carries no
// MCA or chain binding beyond the MCA's nonce.
type Business struct {
	// Nonce is the MCA's get_nonce value the business consumes.
	Nonce string
	// Deadline is a Unix-millisecond time after which the MCA refuses the
	// business.
	Deadline   string
	TxRequests []*TxRequest
}

// TxRequest is one request of a business. Exactly one variant is set.
type TxRequest struct {
	FunctionCall *FunctionCallRequest
	GasPayment   *GasPayment
	Transfer     *Transfer
	// Other holds any other variant (AddWallet, RemoveWallet,
	// UpgradeToLatest, or one unknown to this package) as compact JSON.
	// Caribou never builds one; decoded businesses keep it so their
	// canonical bytes are reproduced.
	Other json.RawMessage
}

// FunctionCallRequest makes the MCA call a contract, one receipt carrying
// every call in FunctionCalls. IntervalBlock delays the receipt by that many
// blocks after the preceding request.
type FunctionCallRequest struct {
	ReceiverID    string
	FunctionCalls []*FunctionCall
	IntervalBlock *uint64
}

// FunctionCall is one contract call. Args is the call's JSON arguments as a
// string; Gas and Deposit are decimal strings (gas units, yoctoNEAR).
type FunctionCall struct {
	MethodName string
	Args       string
	Gas        string
	Deposit    string
}

// GasPayment pays the relayer from the MCA's plain balance of TokenID
// ("near" for native NEAR), in that token's base units.
type GasPayment struct {
	TokenID string
	Amount  string
}

// GasTokenNEAR is the GasPayment token ID for native NEAR.
const GasTokenNEAR = "near"

// Transfer sends native NEAR (yoctoNEAR) from the MCA.
type Transfer struct {
	ReceiverID string
	Amount     string
}

// writeJSONString writes s as a JSON string without HTML escaping, matching
// JavaScript's JSON.stringify and serde_json for the ASCII content Caribou
// builds.
func writeJSONString(b *bytes.Buffer, s string) error {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	b.Truncate(b.Len() - 1) // Encode appends a newline
	return nil
}

// marshalNoEscape marshals v (a struct with fields in the intended order)
// as compact JSON without HTML escaping.
func marshalNoEscape(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), nil
}

// objWriter writes a JSON object with members in call order.
type objWriter struct {
	b   *bytes.Buffer
	n   int
	err error
}

func newObj(b *bytes.Buffer) *objWriter {
	b.WriteByte('{')
	return &objWriter{b: b}
}

func (o *objWriter) key(k string) {
	if o.n > 0 {
		o.b.WriteByte(',')
	}
	o.n++
	if err := writeJSONString(o.b, k); err != nil && o.err == nil {
		o.err = err
	}
	o.b.WriteByte(':')
}

func (o *objWriter) str(k, v string) {
	o.key(k)
	if err := writeJSONString(o.b, v); err != nil && o.err == nil {
		o.err = err
	}
}

func (o *objWriter) raw(k string, f func() error) {
	o.key(k)
	if err := f(); err != nil && o.err == nil {
		o.err = err
	}
}

func (o *objWriter) close() error {
	o.b.WriteByte('}')
	return o.err
}

// Canonical returns the bytes an identity signs: compact JSON with members
// in the order the MCA serializes them (nonce, deadline, tx_requests;
// receiver_id, function_calls, interval_block; method_name, args, gas,
// deposit).
func (bz *Business) Canonical() ([]byte, error) {
	var b bytes.Buffer
	o := newObj(&b)
	o.str("nonce", bz.Nonce)
	o.str("deadline", bz.Deadline)
	o.raw("tx_requests", func() error {
		b.WriteByte('[')
		for i, r := range bz.TxRequests {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := r.writeCanonical(&b); err != nil {
				return fmt.Errorf("request %d: %w", i, err)
			}
		}
		b.WriteByte(']')
		return nil
	})
	if err := o.close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// MarshalJSON encodes the business canonically.
func (bz *Business) MarshalJSON() ([]byte, error) {
	return bz.Canonical()
}

func (r *TxRequest) writeCanonical(b *bytes.Buffer) error {
	if r == nil {
		return errors.New("empty request")
	}
	set := 0
	for _, isSet := range []bool{r.FunctionCall != nil, r.GasPayment != nil, r.Transfer != nil, r.Other != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%d variants set, want 1", set)
	}
	switch {
	case r.Other != nil:
		return json.Compact(b, r.Other)
	case r.GasPayment != nil:
		o := newObj(b)
		o.raw("GasPayment", func() error {
			g := newObj(b)
			g.str("token_id", r.GasPayment.TokenID)
			g.str("amount", r.GasPayment.Amount)
			return g.close()
		})
		return o.close()
	case r.Transfer != nil:
		o := newObj(b)
		o.raw("Transfer", func() error {
			t := newObj(b)
			t.str("receiver_id", r.Transfer.ReceiverID)
			t.str("amount", r.Transfer.Amount)
			return t.close()
		})
		return o.close()
	}
	fc := r.FunctionCall
	o := newObj(b)
	o.raw("FunctionCall", func() error {
		f := newObj(b)
		f.str("receiver_id", fc.ReceiverID)
		f.raw("function_calls", func() error {
			b.WriteByte('[')
			for i, c := range fc.FunctionCalls {
				if i > 0 {
					b.WriteByte(',')
				}
				if c == nil {
					return errors.New("empty function call")
				}
				co := newObj(b)
				co.str("method_name", c.MethodName)
				co.str("args", c.Args)
				co.str("gas", c.Gas)
				co.str("deposit", c.Deposit)
				if err := co.close(); err != nil {
					return err
				}
			}
			b.WriteByte(']')
			return nil
		})
		if fc.IntervalBlock != nil {
			f.raw("interval_block", func() error {
				b.WriteString(strconv.FormatUint(*fc.IntervalBlock, 10))
				return nil
			})
		}
		return f.close()
	})
	return o.close()
}

// decodeStrict decodes b into v, rejecting unknown members and trailing
// data.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// UnmarshalJSON decodes a business.
func (bz *Business) UnmarshalJSON(b []byte) error {
	var v struct {
		Nonce      *string      `json:"nonce"`
		Deadline   *string      `json:"deadline"`
		TxRequests []*TxRequest `json:"tx_requests"`
	}
	if err := decodeStrict(b, &v); err != nil {
		return fmt.Errorf("error decoding business: %w", err)
	}
	if v.Nonce == nil || v.Deadline == nil {
		return errors.New("business lacks a nonce or deadline")
	}
	if v.TxRequests == nil {
		return errors.New("business lacks requests")
	}
	for i, r := range v.TxRequests {
		if r == nil {
			return fmt.Errorf("business request %d is null", i)
		}
	}
	bz.Nonce, bz.Deadline, bz.TxRequests = *v.Nonce, *v.Deadline, v.TxRequests
	return nil
}

// UnmarshalJSON decodes a request. Variants this package does not model,
// and modeled variants with unexpected members, are kept in Other.
func (r *TxRequest) UnmarshalJSON(b []byte) error {
	*r = TxRequest{}
	keep := func() error {
		var c bytes.Buffer
		if err := json.Compact(&c, b); err != nil {
			return err
		}
		r.Other = c.Bytes()
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || len(m) != 1 {
		// A unit variant (a bare string) or a malformed request.
		if !json.Valid(b) {
			return errors.New("invalid request JSON")
		}
		return keep()
	}
	for k, v := range m {
		switch k {
		case "FunctionCall":
			var fc struct {
				ReceiverID    string `json:"receiver_id"`
				FunctionCalls []*struct {
					MethodName string `json:"method_name"`
					Args       string `json:"args"`
					Gas        string `json:"gas"`
					Deposit    string `json:"deposit"`
				} `json:"function_calls"`
				IntervalBlock *uint64 `json:"interval_block"`
			}
			if decodeStrict(v, &fc) != nil {
				return keep()
			}
			req := &FunctionCallRequest{ReceiverID: fc.ReceiverID, IntervalBlock: fc.IntervalBlock}
			for i, c := range fc.FunctionCalls {
				if c == nil {
					return fmt.Errorf("function call %d is null", i)
				}
				req.FunctionCalls = append(req.FunctionCalls, &FunctionCall{
					MethodName: c.MethodName, Args: c.Args, Gas: c.Gas, Deposit: c.Deposit,
				})
			}
			r.FunctionCall = req
		case "GasPayment":
			var g struct {
				TokenID string `json:"token_id"`
				Amount  string `json:"amount"`
			}
			if decodeStrict(v, &g) != nil {
				return keep()
			}
			r.GasPayment = &GasPayment{TokenID: g.TokenID, Amount: g.Amount}
		case "Transfer":
			var t struct {
				ReceiverID string `json:"receiver_id"`
				Amount     string `json:"amount"`
			}
			if decodeStrict(v, &t) != nil {
				return keep()
			}
			r.Transfer = &Transfer{ReceiverID: t.ReceiverID, Amount: t.Amount}
		default:
			return keep()
		}
	}
	return nil
}

// NonceValue parses the business nonce.
func (bz *Business) NonceValue() (uint64, error) {
	n, err := strconv.ParseUint(bz.Nonce, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad nonce %q: %w", bz.Nonce, err)
	}
	return n, nil
}

// DeadlineTime parses the business deadline.
func (bz *Business) DeadlineTime() (time.Time, error) {
	ms, err := strconv.ParseInt(bz.Deadline, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad deadline %q: %w", bz.Deadline, err)
	}
	return time.UnixMilli(ms), nil
}

// Signer recovers the identity that signed the business.
func (bz *Business) Signer(sig string) (Identity, error) {
	msg, err := bz.Canonical()
	if err != nil {
		return "", err
	}
	return RecoverPersonal(msg, sig)
}

// DeadlineFrom returns the deadline Caribou signs for a business prepared
// against a final block with timestamp blockTime: DeadlineWindow later, at
// millisecond precision.
func DeadlineFrom(blockTime time.Time) time.Time {
	return blockTime.Add(DeadlineWindow).Truncate(time.Millisecond)
}

// Fee caps. A relayer fee above either cap is refused: at most 1/50 (2 %)
// of the net amount, and at most the caller's absolute cap (derived from a
// USD limit). The caps guard against a poisoned relayer config; they are
// not tuned from observed relayer acceptance.
const feeRatioDenominator = 50

// RelayerFee is the relayer fee in token units for a business that makes
// storageDeposits NEP-145 deposits, mirroring the RHEA frontend: the
// relayer config's GAS_FEE for the token plus the token value of
// 0.00125 NEAR per storage deposit, from the account manager's
// get_near_value (token base units per NEAR). The NEAR component rounds up.
func RelayerFee(gasFee, nearValue *big.Int, storageDeposits int) (*big.Int, error) {
	if gasFee == nil || gasFee.Sign() < 0 {
		return nil, errors.New("missing or negative GAS_FEE")
	}
	if nearValue == nil || nearValue.Sign() <= 0 {
		return nil, errors.New("missing or non-positive near value")
	}
	if storageDeposits < 0 {
		return nil, errors.New("negative storage deposit count")
	}
	deposit, _ := new(big.Int).SetString(StorageDeposit, 10)
	num := new(big.Int).Mul(deposit, big.NewInt(int64(storageDeposits)))
	num.Mul(num, nearValue)
	near := ceilDiv(num, yoctoPerNEAR)
	return near.Add(near, gasFee), nil
}

var yoctoPerNEAR = new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)

func ceilDiv(a, b *big.Int) *big.Int {
	q, m := new(big.Int).DivMod(a, b, new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// ToInternal converts a token amount to Burrow's internal units:
// amount × 10^extraDecimals.
func ToInternal(amount *big.Int, extraDecimals uint8) *big.Int {
	return new(big.Int).Mul(amount, pow10(extraDecimals))
}

// FromInternal converts a Burrow internal amount to token units, rounding
// down.
func FromInternal(internal *big.Int, extraDecimals uint8) *big.Int {
	return new(big.Int).Quo(internal, pow10(extraDecimals))
}

func pow10(n uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// maxExtraDecimals bounds Burrow's extra_decimals (18 for 6-decimal
// stablecoins plus 12 is the largest observed total).
const maxExtraDecimals = 30

// signingWindow holds the parameters every Caribou business shares.
type signingWindow struct {
	mca       string
	tokenID   string
	blockTime time.Time
	deadline  time.Time
}

func (w *signingWindow) validate(dep *Deployment) error {
	if !dep.IsMCA(w.mca) {
		return fmt.Errorf("%q is not an MCA of this deployment", w.mca)
	}
	if dep.Market(w.tokenID) == nil {
		return fmt.Errorf("token %q is not an allowlisted market", w.tokenID)
	}
	if w.blockTime.IsZero() {
		return errors.New("no reference block time")
	}
	if !w.deadline.Equal(time.UnixMilli(w.deadline.UnixMilli())) {
		return errors.New("deadline is not at millisecond precision")
	}
	if !w.deadline.After(w.blockTime) {
		return errors.New("deadline is not after the reference block time")
	}
	if w.deadline.Sub(w.blockTime) > MaxDeadlineWindow {
		return fmt.Errorf("deadline %s after the reference block time exceeds %s", w.deadline.Sub(w.blockTime), MaxDeadlineWindow)
	}
	return nil
}

// checkClock refuses signing when the local clock and the reference block
// time disagree by more than MaxClockSkew.
func checkClock(blockTime, now time.Time) error {
	skew := now.Sub(blockTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxClockSkew {
		return fmt.Errorf("local clock differs from the reference block time by %s (limit %s)", skew, MaxClockSkew)
	}
	return nil
}

// checkFee enforces both fee caps. fee and feeCap are in the fee's own
// units; feeInNet is the fee valued in the units of net, the amount moved,
// so the relative cap always compares like with like.
func checkFee(fee, feeCap, feeInNet, net *big.Int) error {
	if fee == nil || fee.Sign() <= 0 {
		return errors.New("missing or non-positive relayer fee")
	}
	if feeCap == nil || feeCap.Sign() <= 0 {
		return errors.New("missing fee cap")
	}
	if fee.Cmp(feeCap) > 0 {
		return fmt.Errorf("relayer fee %s exceeds the cap %s", fee, feeCap)
	}
	if feeInNet == nil || feeInNet.Sign() <= 0 {
		return errors.New("relayer fee has no value in the moved token")
	}
	if new(big.Int).Mul(feeInNet, big.NewInt(feeRatioDenominator)).Cmp(net) > 0 {
		return fmt.Errorf("relayer fee worth %s exceeds 1/%d of the amount %s", feeInNet, feeRatioDenominator, net)
	}
	return nil
}

func positive(name string, v *big.Int) error {
	if v == nil || v.Sign() <= 0 {
		return fmt.Errorf("missing or non-positive %s", name)
	}
	return nil
}

// WithdrawParams are the approved parameters of a withdrawal from the
// plain supplied (non-collateral) balance of a market to a 1Click deposit
// address. They are what a preview committed to; BuildWithdraw derives the
// business from them and ValidateWithdraw rejects any other business.
type WithdrawParams struct {
	MCA     string
	TokenID string
	// ExtraDecimals is the market's Burrow extra_decimals: internal amounts
	// are token amounts × 10^ExtraDecimals.
	ExtraDecimals uint8
	Nonce         uint64
	// BlockTime is the timestamp of the final block the business was
	// prepared against. The deadline is measured from it, never from the
	// local clock.
	BlockTime time.Time
	Deadline  time.Time
	// Fee is the relayer fee in token units, paid from the supplied
	// balance before the withdrawal.
	Fee *big.Int
	// FeeCap is the most Fee may be, in token units.
	FeeCap *big.Int
	// Amount is the net amount in token units withdrawn and transferred to
	// DepositAddress. The withdrawal takes exactly Amount × 10^ExtraDecimals
	// internal units, so nothing is left behind in the MCA.
	Amount *big.Int
	// RegisterMCA adds a storage registration of the MCA with the token,
	// needed when the MCA had none at the read block.
	RegisterMCA bool
	// DepositAddress is the 1Click deposit address (a NEAR implicit
	// account).
	DepositAddress string
}

// StorageDeposits is the number of NEP-145 storage deposits the business
// makes, for RelayerFee.
func (p *WithdrawParams) StorageDeposits() int {
	if p.RegisterMCA {
		return 2
	}
	return 1
}

func (p *WithdrawParams) window() *signingWindow {
	return &signingWindow{mca: p.MCA, tokenID: p.TokenID, blockTime: p.BlockTime, deadline: p.Deadline}
}

func (p *WithdrawParams) validate(dep *Deployment) error {
	if err := p.window().validate(dep); err != nil {
		return err
	}
	if p.ExtraDecimals > maxExtraDecimals {
		return fmt.Errorf("extra decimals %d out of range", p.ExtraDecimals)
	}
	if err := positive("amount", p.Amount); err != nil {
		return err
	}
	if err := checkFee(p.Fee, p.FeeCap, p.Fee, p.Amount); err != nil {
		return err
	}
	if !isImplicitAccount(p.DepositAddress) {
		return fmt.Errorf("deposit address %q is not a NEAR implicit account", p.DepositAddress)
	}
	return nil
}

// Call argument encodings, members in the order the RHEA frontend emits
// them.
type (
	simpleWithdrawArgs struct {
		TokenID     string `json:"token_id"`
		Amount      string `json:"amount_with_inner_decimal"`
		RecipientID string `json:"recipient_id"`
	}
	withdrawAction struct {
		TokenID   string `json:"token_id"`
		MaxAmount string `json:"max_amount"`
	}
	executeArgs struct {
		Actions []struct {
			Withdraw withdrawAction `json:"Withdraw"`
		} `json:"actions"`
	}
	registerArgs struct {
		RegistrationOnly bool   `json:"registration_only"`
		AccountID        string `json:"account_id"`
	}
	registerOnlyArgs struct {
		AccountID        string `json:"account_id"`
		RegistrationOnly bool   `json:"registration_only"`
	}
	ftTransferArgs struct {
		ReceiverID string  `json:"receiver_id"`
		Amount     string  `json:"amount"`
		Memo       *string `json:"memo"`
	}
	ftTransferCallArgs struct {
		ReceiverID string `json:"receiver_id"`
		Amount     string `json:"amount"`
		Msg        string `json:"msg"`
	}
)

func call(method string, args any, gas, deposit string) (*FunctionCall, error) {
	a, err := marshalNoEscape(args)
	if err != nil {
		return nil, err
	}
	return &FunctionCall{MethodName: method, Args: a, Gas: gas, Deposit: deposit}, nil
}

func fcRequest(receiver string, interval *uint64, calls ...*FunctionCall) *TxRequest {
	return &TxRequest{FunctionCall: &FunctionCallRequest{ReceiverID: receiver, FunctionCalls: calls, IntervalBlock: interval}}
}

// depositTransferRequest registers a 1Click deposit address with the token
// and transfers amount to it, TransferIntervalBlock blocks after the
// preceding request.
func depositTransferRequest(tokenID, depositAddress, amount string) (*TxRequest, error) {
	reg, err := call("storage_deposit", registerOnlyArgs{AccountID: depositAddress, RegistrationOnly: true}, GasStorageDeposit, StorageDeposit)
	if err != nil {
		return nil, err
	}
	xfer, err := call("ft_transfer", ftTransferArgs{ReceiverID: depositAddress, Amount: amount}, GasFTTransfer, OneYocto)
	if err != nil {
		return nil, err
	}
	interval := uint64(TransferIntervalBlock)
	return fcRequest(tokenID, &interval, reg, xfer), nil
}

// BuildWithdraw builds the withdrawal business for approved parameters:
//
//  1. Burrow simple_withdraw of the fee (internal units) to the relayer;
//  2. if RegisterMCA, storage_deposit registering the MCA with the token;
//  3. Burrow execute with a single Withdraw of exactly Amount (internal
//     units) from the plain supplied balance;
//  4. after TransferIntervalBlock blocks, storage_deposit registering the
//     deposit address and ft_transfer of Amount (token units) to it.
func BuildWithdraw(dep *Deployment, p *WithdrawParams) (*Business, error) {
	if err := p.validate(dep); err != nil {
		return nil, err
	}
	return buildWithdraw(dep, p)
}

func buildWithdraw(dep *Deployment, p *WithdrawParams) (*Business, error) {
	fee, err := call("simple_withdraw", simpleWithdrawArgs{
		TokenID:     p.TokenID,
		Amount:      ToInternal(p.Fee, p.ExtraDecimals).String(),
		RecipientID: dep.Relayer,
	}, GasSimpleWithdraw, OneYocto)
	if err != nil {
		return nil, err
	}
	reqs := []*TxRequest{fcRequest(dep.Burrow, nil, fee)}
	if p.RegisterMCA {
		reg, err := call("storage_deposit", registerArgs{RegistrationOnly: false, AccountID: p.MCA}, GasStorageDeposit, StorageDeposit)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, fcRequest(p.TokenID, nil, reg))
	}
	var ex executeArgs
	ex.Actions = make([]struct {
		Withdraw withdrawAction `json:"Withdraw"`
	}, 1)
	ex.Actions[0].Withdraw = withdrawAction{TokenID: p.TokenID, MaxAmount: ToInternal(p.Amount, p.ExtraDecimals).String()}
	withdraw, err := call("execute", ex, GasExecute, OneYocto)
	if err != nil {
		return nil, err
	}
	reqs = append(reqs, fcRequest(dep.Burrow, nil, withdraw))
	xfer, err := depositTransferRequest(p.TokenID, p.DepositAddress, p.Amount.String())
	if err != nil {
		return nil, err
	}
	reqs = append(reqs, xfer)
	return &Business{
		Nonce:      strconv.FormatUint(p.Nonce, 10),
		Deadline:   strconv.FormatInt(p.Deadline.UnixMilli(), 10),
		TxRequests: reqs,
	}, nil
}

// ValidateWithdraw checks a business against approved withdrawal parameters
// and the local clock: the parameters must be within every limit (deadline
// window, clock skew, fee caps, allowlist, address forms), and the business
// must be byte-identical to the one BuildWithdraw derives from them. Any
// extra, missing, reordered, or altered request fails, whatever its kind.
func ValidateWithdraw(dep *Deployment, b *Business, p *WithdrawParams, now time.Time) error {
	if err := p.validate(dep); err != nil {
		return err
	}
	if err := checkClock(p.BlockTime, now); err != nil {
		return err
	}
	want, err := buildWithdraw(dep, p)
	if err != nil {
		return err
	}
	return sameBusiness(b, want)
}

// SweepParams are the approved parameters of a business that moves an
// MCA's plain token balance: back into supply, or to a 1Click deposit
// address when DepositAddress is set. The relayer fee is a GasPayment from
// the MCA's plain balance of GasToken.
type SweepParams struct {
	MCA       string
	TokenID   string
	Nonce     uint64
	BlockTime time.Time
	Deadline  time.Time
	// GasToken is the token the fee is paid in: GasTokenNEAR or TokenID.
	GasToken string
	// Fee is the relayer fee in GasToken base units.
	Fee *big.Int
	// FeeCap is the most Fee may be, in GasToken base units.
	FeeCap *big.Int
	// NearValue is the account manager's get_near_value for TokenID (token
	// base units per NEAR) at the read block. It values a native NEAR fee in
	// the swept token for the relative fee cap, and is required exactly
	// when GasToken is GasTokenNEAR and TokenID is not the deployment's
	// wrapped NEAR (whose base units are yoctoNEAR).
	NearValue *big.Int
	// Amount is the token amount moved.
	Amount *big.Int
	// DepositAddress, when set, sends Amount to this 1Click deposit address
	// (a NEAR implicit account). Otherwise Amount is supplied to Burrow
	// with msg "" (plain supply).
	DepositAddress string
}

func (p *SweepParams) validate(dep *Deployment) error {
	w := &signingWindow{mca: p.MCA, tokenID: p.TokenID, blockTime: p.BlockTime, deadline: p.Deadline}
	if err := w.validate(dep); err != nil {
		return err
	}
	if p.GasToken != GasTokenNEAR && p.GasToken != p.TokenID {
		return fmt.Errorf("gas token %q is neither NEAR nor the swept token", p.GasToken)
	}
	if err := positive("amount", p.Amount); err != nil {
		return err
	}
	feeInToken, err := p.feeInToken(dep)
	if err != nil {
		return err
	}
	if err := checkFee(p.Fee, p.FeeCap, feeInToken, p.Amount); err != nil {
		return err
	}
	if p.DepositAddress != "" && !isImplicitAccount(p.DepositAddress) {
		return fmt.Errorf("deposit address %q is not a NEAR implicit account", p.DepositAddress)
	}
	return nil
}

// feeInToken values the fee in the swept token's base units: unchanged when
// the fee is paid in the swept token or in native NEAR for wrapped NEAR
// (both yoctoNEAR), otherwise converted at NearValue and rounded up.
func (p *SweepParams) feeInToken(dep *Deployment) (*big.Int, error) {
	if p.Fee == nil {
		return nil, errors.New("missing relayer fee")
	}
	if p.GasToken == p.TokenID || (p.GasToken == GasTokenNEAR && p.TokenID == dep.WrappedNEAR) {
		return p.Fee, nil
	}
	if p.NearValue == nil || p.NearValue.Sign() <= 0 {
		return nil, fmt.Errorf("a NEAR fee for %s needs the token's near value", p.TokenID)
	}
	return ceilDiv(new(big.Int).Mul(p.Fee, p.NearValue), yoctoPerNEAR), nil
}

// BuildSweep builds the sweep business for approved parameters: a
// GasPayment of the fee, then either ft_transfer_call of Amount to Burrow
// with msg "" or, after TransferIntervalBlock blocks, registration of and
// ft_transfer to the deposit address.
func BuildSweep(dep *Deployment, p *SweepParams) (*Business, error) {
	if err := p.validate(dep); err != nil {
		return nil, err
	}
	return buildSweep(dep, p)
}

func buildSweep(dep *Deployment, p *SweepParams) (*Business, error) {
	reqs := []*TxRequest{{GasPayment: &GasPayment{TokenID: p.GasToken, Amount: p.Fee.String()}}}
	if p.DepositAddress != "" {
		xfer, err := depositTransferRequest(p.TokenID, p.DepositAddress, p.Amount.String())
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, xfer)
	} else {
		supply, err := call("ft_transfer_call", ftTransferCallArgs{ReceiverID: dep.Burrow, Amount: p.Amount.String()}, GasFTTransferCall, OneYocto)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, fcRequest(p.TokenID, nil, supply))
	}
	return &Business{
		Nonce:      strconv.FormatUint(p.Nonce, 10),
		Deadline:   strconv.FormatInt(p.Deadline.UnixMilli(), 10),
		TxRequests: reqs,
	}, nil
}

// ValidateSweep is ValidateWithdraw for sweep businesses.
func ValidateSweep(dep *Deployment, b *Business, p *SweepParams, now time.Time) error {
	if err := p.validate(dep); err != nil {
		return err
	}
	if err := checkClock(p.BlockTime, now); err != nil {
		return err
	}
	want, err := buildSweep(dep, p)
	if err != nil {
		return err
	}
	return sameBusiness(b, want)
}

// sameBusiness compares canonical encodings. On a mismatch it names the
// first difference.
func sameBusiness(got, want *Business) error {
	if got == nil {
		return errors.New("no business")
	}
	gb, err := got.Canonical()
	if err != nil {
		return fmt.Errorf("unencodable business: %w", err)
	}
	wb, err := want.Canonical()
	if err != nil {
		return err
	}
	if bytes.Equal(gb, wb) {
		return nil
	}
	switch {
	case got.Nonce != want.Nonce:
		return fmt.Errorf("nonce %q, want %q", got.Nonce, want.Nonce)
	case got.Deadline != want.Deadline:
		return fmt.Errorf("deadline %q, want %q", got.Deadline, want.Deadline)
	case len(got.TxRequests) != len(want.TxRequests):
		return fmt.Errorf("%d requests, want %d", len(got.TxRequests), len(want.TxRequests))
	}
	for i := range want.TxRequests {
		var g, w bytes.Buffer
		_ = got.TxRequests[i].writeCanonical(&g)
		_ = want.TxRequests[i].writeCanonical(&w)
		if !bytes.Equal(g.Bytes(), w.Bytes()) {
			return fmt.Errorf("request %d differs from the approved request", i)
		}
	}
	return errors.New("business differs from the approved business")
}

// SignedRequest is a signed business as the relayer accepts it.
// AttachDeposit is the NEAR deposit the relayer attaches to exec; it is
// outside the signature.
type SignedRequest struct {
	SignerWallet  Wallet
	Business      *Business
	Signature     string
	AttachDeposit string
}

// MarshalJSON encodes the request with members in the RHEA frontend's
// order: signer_wallet, business, signature, attach_deposit.
func (r *SignedRequest) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	o := newObj(&b)
	o.raw("signer_wallet", func() error {
		w, err := r.SignerWallet.MarshalJSON()
		b.Write(w)
		return err
	})
	o.raw("business", func() error {
		bz, err := r.Business.Canonical()
		b.Write(bz)
		return err
	})
	o.str("signature", r.Signature)
	o.str("attach_deposit", r.AttachDeposit)
	if err := o.close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ParseSignedRequest decodes a signed request as MarshalJSON encodes it.
// Re-encoding the result reproduces the same bytes, so a persisted request
// can be resubmitted identically.
func ParseSignedRequest(b []byte) (*SignedRequest, error) {
	var v struct {
		SignerWallet  *Wallet   `json:"signer_wallet"`
		Business      *Business `json:"business"`
		Signature     string    `json:"signature"`
		AttachDeposit string    `json:"attach_deposit"`
	}
	if err := decodeStrict(b, &v); err != nil {
		return nil, fmt.Errorf("error decoding signed request: %w", err)
	}
	if v.SignerWallet == nil || v.Business == nil || v.Signature == "" || v.AttachDeposit == "" {
		return nil, errors.New("signed request lacks a wallet, business, signature, or deposit")
	}
	return &SignedRequest{SignerWallet: *v.SignerWallet, Business: v.Business, Signature: v.Signature, AttachDeposit: v.AttachDeposit}, nil
}

func (k *Key) signBusiness(b *Business) (*SignedRequest, error) {
	msg, err := b.Canonical()
	if err != nil {
		return nil, err
	}
	sig, err := k.signPersonal(msg)
	if err != nil {
		return nil, err
	}
	return &SignedRequest{
		SignerWallet:  k.id.Wallet(),
		Business:      b,
		Signature:     sig,
		AttachDeposit: StorageDeposit,
	}, nil
}

// SignWithdraw validates a withdrawal business against its approved
// parameters (see ValidateWithdraw) and signs it. There is no other way to
// sign a business with a Key.
func (k *Key) SignWithdraw(dep *Deployment, b *Business, p *WithdrawParams, now time.Time) (*SignedRequest, error) {
	if err := ValidateWithdraw(dep, b, p, now); err != nil {
		return nil, fmt.Errorf("refusing to sign: %w", err)
	}
	return k.signBusiness(b)
}

// SignSweep validates a sweep business against its approved parameters
// (see ValidateSweep) and signs it.
func (k *Key) SignSweep(dep *Deployment, b *Business, p *SweepParams, now time.Time) (*SignedRequest, error) {
	if err := ValidateSweep(dep, b, p, now); err != nil {
		return nil, fmt.Errorf("refusing to sign: %w", err)
	}
	return k.signBusiness(b)
}

// ExecArgs are the arguments of an MCA exec call: a signed business.
type ExecArgs struct {
	SignerWallet Wallet    `json:"signer_wallet"`
	Business     *Business `json:"business"`
	Signature    string    `json:"signature"`
}

// ParseExecArgs decodes exec call arguments.
func ParseExecArgs(b []byte) (*ExecArgs, error) {
	var a ExecArgs
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("error decoding exec arguments: %w", err)
	}
	if a.Business == nil || a.Signature == "" {
		return nil, errors.New("exec arguments lack a business or signature")
	}
	return &a, nil
}
