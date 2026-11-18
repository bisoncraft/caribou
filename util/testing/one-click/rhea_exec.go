// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package oneclickmock

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// Business encoding as the MCA verifies it: the relayer's request carries
// the business in any key order; the MCA re-encodes the decoded business
// in the SDK's member order (nonce, deadline, tx_requests; receiver_id,
// function_calls, interval_block; method_name, args, gas, deposit;
// token_id, amount), without HTML escaping, and verifies the EIP-191
// signature over those bytes.

type mBusiness struct {
	Nonce      string     `json:"nonce"`
	Deadline   string     `json:"deadline"`
	TxRequests []mRequest `json:"tx_requests"`
}

type mRequest struct {
	FunctionCall *mFunctionCall `json:"FunctionCall,omitempty"`
	GasPayment   *mGasPayment   `json:"GasPayment,omitempty"`
}

type mFunctionCall struct {
	ReceiverID    string  `json:"receiver_id"`
	FunctionCalls []mCall `json:"function_calls"`
	IntervalBlock *uint64 `json:"interval_block,omitempty"`
}

type mCall struct {
	MethodName string `json:"method_name"`
	Args       string `json:"args"`
	Gas        string `json:"gas"`
	Deposit    string `json:"deposit"`
}

type mGasPayment struct {
	TokenID string `json:"token_id"`
	Amount  string `json:"amount"`
}

// mSignedRequest is the relayer request: a signed business plus the NEAR
// deposit the relayer attaches to exec.
type mSignedRequest struct {
	SignerWallet  map[string]string `json:"signer_wallet"`
	Business      json.RawMessage   `json:"business"`
	Signature     string            `json:"signature"`
	AttachDeposit string            `json:"attach_deposit"`
}

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

// parseBusiness decodes a business and returns it with its canonical
// encoding.
func parseBusiness(raw []byte) (*mBusiness, []byte, error) {
	var b mBusiness
	if err := decodeStrict(raw, &b); err != nil {
		return nil, nil, fmt.Errorf("bad business: %w", err)
	}
	if len(b.TxRequests) == 0 {
		return nil, nil, errors.New("Transaction requests cannot be empty")
	}
	for i, r := range b.TxRequests {
		if (r.FunctionCall == nil) == (r.GasPayment == nil) {
			return nil, nil, fmt.Errorf("request %d is not exactly one of FunctionCall, GasPayment", i)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&b); err != nil {
		return nil, nil, err
	}
	return &b, bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// recoverEVM recovers the EVM address (lowercase hex, no 0x) behind an
// EIP-191 personal_sign signature: 65 bytes r||s||v, v in {27, 28}.
func recoverEVM(msg []byte, sigHex string) (string, error) {
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != 65 {
		return "", errors.New("signature is not 65 bytes of hex")
	}
	if sig[64] != 27 && sig[64] != 28 {
		return "", errors.New("bad recovery byte")
	}
	sig[64] -= 27
	prefix := "\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg))
	pub, err := crypto.SigToPub(crypto.Keccak256([]byte(prefix), msg), sig)
	if err != nil {
		return "", err
	}
	return strings.ToLower(hex.EncodeToString(crypto.PubkeyToAddress(*pub).Bytes())), nil
}

func parseAmountArg(s string) (*big.Int, bool) {
	v, ok := new(big.Int).SetString(s, 10)
	return v, ok && v.Sign() >= 0
}

// submit accepts a relayer request for mca and schedules its exec. It
// returns the batch ID, or an error the relayer refuses the request with.
func (w *rheaWorld) submit(mca, wallet, request string) (string, error) {
	var req mSignedRequest
	if err := json.Unmarshal([]byte(request), &req); err != nil {
		return "", fmt.Errorf("bad request: %v", err)
	}
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if _, ok := w.st.wallets[mca]; !ok {
		return "", fmt.Errorf("unknown MCA %s", mca)
	}
	batch := &rheaBatch{id: newBatchID(), mca: mca, wallet: wallet, request: request, hash: randomHash()}
	w.batches[batch.id] = batch

	argsJSON, _ := json.Marshal(map[string]any{
		"signer_wallet": req.SignerWallet,
		"business":      req.Business,
		"signature":     req.Signature,
	})
	// The relayer forwards the business with its keys reordered; the MCA
	// verifies over its own canonical encoding.
	var reordered any
	_ = json.Unmarshal(argsJSON, &reordered)
	argsJSON, _ = json.Marshal(reordered)

	deposit := req.AttachDeposit
	if deposit == "" {
		deposit = "0"
	}
	tx := w.newTx(w.dep.Relayer, mca, []any{callAction("exec", argsJSON, 300_000_000_000_000, deposit)},
		time.Now().Add(w.faults.ExecDelay))
	tx.onInclude = func(tx *rheaTx) {
		batch.status, batch.txHash = 2, tx.hash
		w.histID++
		w.history[mca] = append([]*rheaHistory{{
			id: w.histID, mca: mca, wallet: wallet, txHash: tx.hash, batchID: batch.id, status: "pending",
			created: time.Now().UTC().Format("2006-01-02 15:04:05"),
		}}, w.history[mca]...)
	}
	w.firstReceipt(tx, func(r *rheaReceipt) {
		msg := w.runExec(r, mca, &req, deposit)
		for _, h := range w.history[mca] {
			if h.txHash == tx.hash {
				h.status = "success"
				if msg != "" {
					h.status = "failed"
				}
			}
		}
		batch.txErr = msg
	})
	w.log.Printf("rhea: batch %s for %s accepted, exec %s", batch.id, mca, tx.hash)
	return batch.id, nil
}

// runExec executes the exec receipt. It returns the failure message, or ""
// on success.
func (w *rheaWorld) runExec(r *rheaReceipt, mca string, req *mSignedRequest, deposit string) string {
	fail := func(msg string) string {
		r.status = failure(msg)
		w.log.Printf("rhea: exec %s on %s failed: %s", r.tx.hash, mca, msg)
		return msg
	}
	b, canon, err := parseBusiness(req.Business)
	if err != nil {
		return fail(err.Error())
	}
	signer, err := recoverEVM(canon, req.Signature)
	if err != nil || signer != strings.ToLower(req.SignerWallet["EVM"]) {
		return fail("InvalidEvmSignature")
	}
	bound := false
	for _, id := range w.st.wallets[mca] {
		bound = bound || id == signer
	}
	if !bound {
		return fail("WalletDoesNotExist")
	}
	deadline, err := strconv.ParseInt(b.Deadline, 10, 64)
	if err != nil {
		return fail("bad deadline")
	}
	// The deadline is compared with the timestamp of the block the exec
	// receipt executes in.
	if w.producing.ts.UnixMilli() > deadline {
		return fail("DeadlineExpired")
	}
	nonce, err := strconv.ParseUint(b.Nonce, 10, 64)
	if err != nil || nonce != w.st.nonces[mca] {
		return fail("InvalidNonce")
	}
	// GasPayment in native NEAR leaves the MCA when exec creates the
	// transfer, so an MCA that cannot pay fails the whole exec.
	attached, _ := parseAmountArg(deposit)
	nearNeed := new(big.Int)
	for _, tr := range b.TxRequests {
		if g := tr.GasPayment; g != nil && g.TokenID == "near" {
			amt, ok := parseAmountArg(g.Amount)
			if !ok {
				return fail("bad GasPayment amount")
			}
			nearNeed.Add(nearNeed, amt)
		}
	}
	avail := w.st.unlockedNEAR(mca)
	if attached != nil {
		avail.Add(avail, attached)
	}
	if avail.Cmp(nearNeed) < 0 {
		return fail("NotEnoughBalance")
	}

	w.st.nonces[mca]++
	if attached != nil {
		w.st.nearBal[mca].Add(w.st.nearBal[mca], attached)
	}
	w.st.nearBal[mca].Sub(w.st.nearBal[mca], nearNeed)
	ids := make([]string, len(b.TxRequests))
	for i := range ids {
		ids[i] = randomHash()
	}
	r.children = ids
	w.scheduleRequest(r.tx, mca, b, ids, 0, r.height)
	return ""
}

// blockByHeight returns a produced block by height, or nil.
func (w *rheaWorld) blockByHeight(h uint64) *rheaBlock {
	for i := len(w.blocks) - 1; i >= 0; i-- {
		if w.blocks[i].height == h {
			return w.blocks[i]
		}
		if w.blocks[i].height < h {
			break
		}
	}
	return nil
}

// scheduleRequest schedules business request i after height after. Each
// request runs after the previous one executed (as the MCA's
// rest_tx_requests_callback chain does), delayed by its interval_block.
// A failed request does not stop the ones after it.
func (w *rheaWorld) scheduleRequest(tx *rheaTx, mca string, b *mBusiness, ids []string, i int, after uint64) {
	if i >= len(b.TxRequests) {
		return
	}
	tr := b.TxRequests[i]
	next := func(r *rheaReceipt) {
		w.scheduleRequest(tx, mca, b, ids, i+1, r.height)
	}
	switch {
	case tr.GasPayment != nil:
		g := tr.GasPayment
		amt, _ := parseAmountArg(g.Amount)
		if g.TokenID == "near" {
			w.childWithID(ids[i], tx, after, mca, w.dep.Relayer, []any{transferAction(g.Amount)}, func(r *rheaReceipt) {
				next(r)
			})
			return
		}
		args, _ := json.Marshal(map[string]any{"receiver_id": w.dep.Relayer, "amount": g.Amount})
		w.childWithID(ids[i], tx, after, mca, g.TokenID, []any{callAction("ft_transfer", args, 10_000_000_000_000, "1")}, func(r *rheaReceipt) {
			if amt == nil || !w.st.debit(g.TokenID, mca, amt) {
				r.status = failure("The account doesn't have enough balance")
			} else {
				w.st.credit(g.TokenID, w.dep.Relayer, amt)
				r.logs = append(r.logs, nep141Log(mca, w.dep.Relayer, amt))
			}
			next(r)
		})
	case tr.FunctionCall != nil:
		fc := tr.FunctionCall
		delay := after
		if fc.IntervalBlock != nil {
			delay += *fc.IntervalBlock
		}
		actions := make([]any, len(fc.FunctionCalls))
		for k, c := range fc.FunctionCalls {
			gas, _ := strconv.ParseUint(c.Gas, 10, 64)
			actions[k] = callAction(c.MethodName, c.Args, gas, c.Deposit)
		}
		w.childWithID(ids[i], tx, delay, mca, fc.ReceiverID, actions, func(r *rheaReceipt) {
			w.runFunctionCalls(r, mca, fc)
			next(r)
		})
	}
}

// runFunctionCalls executes a batch of calls from mca on one receiver.
// Like a NEAR batch receipt, a failing call reverts the whole batch.
func (w *rheaWorld) runFunctionCalls(r *rheaReceipt, mca string, fc *mFunctionCall) {
	saved := w.st.clone()
	pokes := len(w.pokes)
	var logs []string
	var children []func(r *rheaReceipt) string
	for _, c := range fc.FunctionCalls {
		deposit, _ := parseAmountArg(c.Deposit)
		if deposit != nil && deposit.Sign() > 0 {
			bal := w.st.nearBal[mca]
			if bal == nil || bal.Cmp(deposit) < 0 {
				w.st, w.pokes = saved, w.pokes[:pokes]
				r.status = failure("LackBalanceForDeposit")
				return
			}
			bal.Sub(bal, deposit)
		}
		var err error
		var spawn func(r *rheaReceipt) string
		switch {
		case fc.ReceiverID == w.dep.Burrow:
			spawn, err = w.burrowCall(mca, c, &logs)
		case w.isToken(fc.ReceiverID):
			spawn, err = w.tokenCall(r.tx.hash, mca, fc.ReceiverID, c, &logs)
		default:
			err = fmt.Errorf("unsupported receiver %s", fc.ReceiverID)
		}
		if err != nil {
			w.st, w.pokes = saved, w.pokes[:pokes]
			r.status = failure(err.Error())
			return
		}
		if spawn != nil {
			children = append(children, spawn)
		}
	}
	r.logs = append(r.logs, logs...)
	for _, spawn := range children {
		if id := spawn(r); id != "" {
			r.children = append(r.children, id)
		}
	}
}

func (w *rheaWorld) isToken(id string) bool {
	return w.markets[id] != nil || id == w.dep.WrappedNEAR
}

// extraDecimals returns the extra decimals Burrow scales a token by.
func (w *rheaWorld) extraDecimals(token string) int {
	if m := w.markets[token]; m != nil {
		return m.extraDecimals
	}
	return 0
}

// burrowCall executes a call on Burrow. A returned spawn function creates
// the call's child receipts once the batch has committed.
func (w *rheaWorld) burrowCall(mca string, c mCall, logs *[]string) (func(r *rheaReceipt) string, error) {
	switch c.MethodName {
	case "storage_deposit":
		w.st.burrowReg[mca] = true
		return nil, nil
	case "simple_withdraw":
		var a struct {
			TokenID     string `json:"token_id"`
			Amount      string `json:"amount_with_inner_decimal"`
			RecipientID string `json:"recipient_id"`
		}
		if err := decodeStrict([]byte(c.Args), &a); err != nil {
			return nil, fmt.Errorf("bad simple_withdraw args: %v", err)
		}
		amt, ok := parseAmountArg(a.Amount)
		if !ok || amt.Sign() == 0 {
			return nil, errors.New("bad amount")
		}
		return w.burrowWithdraw(mca, a.TokenID, amt, a.RecipientID, logs)
	case "execute":
		var a struct {
			Actions []struct {
				Withdraw *struct {
					TokenID   string `json:"token_id"`
					MaxAmount string `json:"max_amount"`
				} `json:"Withdraw"`
			} `json:"actions"`
		}
		if err := json.Unmarshal([]byte(c.Args), &a); err != nil || len(a.Actions) != 1 || a.Actions[0].Withdraw == nil {
			return nil, errors.New("only a single Withdraw action is simulated")
		}
		wd := a.Actions[0].Withdraw
		if w.faults.FailWithdraws > 0 {
			w.faults.FailWithdraws--
			return nil, errors.New("Not enough liquidity (fault injection)")
		}
		max, ok := parseAmountArg(wd.MaxAmount)
		if !ok {
			return nil, errors.New("bad max_amount")
		}
		amt := w.st.suppliedOf(mca, wd.TokenID)
		if max.Cmp(amt) < 0 {
			amt = max
		}
		if amt.Sign() == 0 {
			return nil, errors.New("Nothing to withdraw")
		}
		return w.burrowWithdraw(mca, wd.TokenID, amt, mca, logs)
	}
	return nil, fmt.Errorf("method %s not simulated on Burrow", c.MethodName)
}

// burrowWithdraw debits mca's supply of token by amt (internal units) and
// pays recipient: an ft_transfer, then Burrow's callback logging
// withdraw_succeeded.
func (w *rheaWorld) burrowWithdraw(mca, token string, amt *big.Int, recipient string, logs *[]string) (func(r *rheaReceipt) string, error) {
	a := w.st.assets[token]
	if a == nil || !a.canWithdraw {
		return nil, fmt.Errorf("asset %s cannot be withdrawn", token)
	}
	if w.st.suppliedOf(mca, token).Cmp(amt) < 0 {
		return nil, errors.New("Not enough supplied balance")
	}
	w.st.addSupplied(mca, token, new(big.Int).Neg(amt))
	*logs = append(*logs, burrowLog("withdraw_started", mca, token, amt))
	tokenAmt := new(big.Int).Div(amt, tenPow(w.extraDecimals(token)))
	return func(parent *rheaReceipt) string {
		args, _ := json.Marshal(map[string]any{"receiver_id": recipient, "amount": tokenAmt.String()})
		xfer := w.child(parent, w.dep.Burrow, token, []any{callAction("ft_transfer", args, 10_000_000_000_000, "1")}, func(r *rheaReceipt) {
			w.st.credit(token, recipient, tokenAmt)
			w.st.register(token, recipient)
			r.logs = append(r.logs, nep141Log(w.dep.Burrow, recipient, tokenAmt))
			w.notifyDeposit(recipient, r.tx.hash)
		})
		cbID := randomHash()
		parent.children = append(parent.children, cbID)
		w.childWithID(cbID, parent.tx, parent.height+1, w.dep.Burrow, w.dep.Burrow, []any{callAction("after_ft_transfer", "{}", 10_000_000_000_000, "0")}, func(r *rheaReceipt) {
			r.logs = append(r.logs, burrowLog("withdraw_succeeded", mca, token, amt))
		})
		return xfer.id
	}, nil
}

// tokenCall executes a call from mca on a token contract.
func (w *rheaWorld) tokenCall(txHash, mca, token string, c mCall, logs *[]string) (func(r *rheaReceipt) string, error) {
	switch c.MethodName {
	case "storage_deposit":
		var a struct {
			AccountID        string `json:"account_id"`
			RegistrationOnly bool   `json:"registration_only"`
		}
		if err := json.Unmarshal([]byte(c.Args), &a); err != nil {
			return nil, err
		}
		if a.AccountID == "" {
			a.AccountID = mca
		}
		w.st.register(token, a.AccountID)
		return nil, nil
	case "ft_transfer":
		var a struct {
			ReceiverID string  `json:"receiver_id"`
			Amount     string  `json:"amount"`
			Memo       *string `json:"memo"`
		}
		if err := json.Unmarshal([]byte(c.Args), &a); err != nil {
			return nil, err
		}
		amt, ok := parseAmountArg(a.Amount)
		if !ok || amt.Sign() == 0 {
			return nil, errors.New("bad amount")
		}
		if !w.st.ftReg[token][a.ReceiverID] {
			return nil, fmt.Errorf("The account %s is not registered", a.ReceiverID)
		}
		if !w.st.debit(token, mca, amt) {
			return nil, errors.New("The account doesn't have enough balance")
		}
		w.st.credit(token, a.ReceiverID, amt)
		*logs = append(*logs, nep141Log(mca, a.ReceiverID, amt))
		w.notifyDeposit(a.ReceiverID, txHash)
		return nil, nil
	case "ft_transfer_call":
		var a struct {
			ReceiverID string `json:"receiver_id"`
			Amount     string `json:"amount"`
			Msg        string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(c.Args), &a); err != nil {
			return nil, err
		}
		if a.ReceiverID != w.dep.Burrow {
			return nil, fmt.Errorf("ft_transfer_call to %s not simulated", a.ReceiverID)
		}
		amt, ok := parseAmountArg(a.Amount)
		if !ok || amt.Sign() == 0 {
			return nil, errors.New("bad amount")
		}
		if !w.st.debit(token, mca, amt) {
			return nil, errors.New("The account doesn't have enough balance")
		}
		w.st.credit(token, w.dep.Burrow, amt)
		*logs = append(*logs, nep141Log(mca, w.dep.Burrow, amt))
		return func(parent *rheaReceipt) string {
			return w.supplyToBurrow(parent, token, mca, amt, nil)
		}, nil
	}
	return nil, fmt.Errorf("method %s not simulated on %s", c.MethodName, token)
}

// supplyToBurrow runs the receipts after an ft_transfer_call of amt from
// sender to Burrow: Burrow's ft_on_transfer (a deposit for sender, or a
// refusal) and the token's ft_resolve_transfer, which refunds what Burrow
// did not use. It lists both under parent and returns "" (the IDs are
// added directly). done, when set, runs after the resolution.
func (w *rheaWorld) supplyToBurrow(parent *rheaReceipt, token, sender string, amt *big.Int, done func(r *rheaReceipt)) string {
	resolveID := randomHash()
	unused := new(big.Int)
	onArgs, _ := json.Marshal(map[string]any{"sender_id": sender, "amount": amt.String(), "msg": ""})
	on := w.child(parent, token, w.dep.Burrow, []any{callAction("ft_on_transfer", onArgs, 30_000_000_000_000, "0")}, func(r *rheaReceipt) {
		a := w.st.assets[token]
		if w.faults.RejectBurrowDeposits > 0 || a == nil || !a.canDeposit {
			if w.faults.RejectBurrowDeposits > 0 {
				w.faults.RejectBurrowDeposits--
			}
			unused.Set(amt)
		} else {
			internal := new(big.Int).Mul(amt, tenPow(w.extraDecimals(token)))
			w.st.burrowReg[sender] = true
			w.st.addSupplied(sender, token, internal)
			r.logs = append(r.logs, burrowLog("deposit", sender, token, internal))
		}
		r.status = okValue(unused.String())
		resArgs, _ := json.Marshal(map[string]any{"sender_id": sender, "receiver_id": w.dep.Burrow, "amount": amt.String()})
		w.childWithID(resolveID, r.tx, r.height, token, token, []any{callAction("ft_resolve_transfer", resArgs, 5_000_000_000_000, "0")}, func(r *rheaReceipt) {
			used := new(big.Int).Sub(amt, unused)
			if unused.Sign() > 0 && w.st.debit(token, w.dep.Burrow, unused) {
				w.st.credit(token, sender, unused)
				r.logs = append(r.logs, nep141Log(w.dep.Burrow, sender, unused))
			}
			r.status = okValue(used.String())
			if done != nil {
				done(r)
			}
		})
	})
	parent.children = append(parent.children, on.id, resolveID)
	return ""
}

// newBatchID returns a UUID-shaped batch identifier, as the relayer issues.
func newBatchID() string {
	h := newCorrelationID() + newCorrelationID()
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
