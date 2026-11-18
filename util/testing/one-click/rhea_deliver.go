// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package oneclickmock

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Deliveries: the verifier (intents.near) paying a token out of the RHEA
// world's ledger, in the receipt shape of mainnet 1Click payouts. The
// transaction is signed by intents.near; its first, local receipt logs the
// dip4 ft_withdraw event and calls the token: ft_transfer for a plain
// payout, or ft_transfer_call with the customRecipientMsg, whose recipient
// handler (account manager or MCA) runs as further receipts. Mainnet
// captures: tx-supply-existing-Ey4MNC5d and tx-create-supply-1aqcDa8Y in
// wallet/rhea/testdata.

// payout pays amount of token from the verifier to recipient and returns
// the transaction hash. depositAccount is the 1Click account the tokens
// come from (the swap's deposit address).
func (w *rheaWorld) payout(token, recipient string, amount *big.Int, msg, depositAccount string) string {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	intent, _ := json.Marshal(map[string]any{
		"intent": "ft_withdraw", "token": token, "receiver_id": recipient, "amount": amount.String(), "msg": msg,
	})
	args, _ := json.Marshal(map[string]any{"signed": []any{map[string]any{"payload": map[string]any{"message": string(intent)}}}})
	tx := w.newTx(rheaVerifier, rheaVerifier, []any{callAction("execute_intents", args, 300_000_000_000_000, "0")}, time.Now())
	w.firstReceipt(tx, func(r *rheaReceipt) {
		r.logs = append(r.logs, dip4WithdrawLog(depositAccount, token, recipient, amount, msg))
		if msg == "" {
			xargs, _ := json.Marshal(map[string]any{"receiver_id": recipient, "amount": amount.String(), "memo": nil})
			x := w.child(r, rheaVerifier, token, []any{callAction("ft_transfer", xargs, 15_000_000_000_000, "1")}, func(r *rheaReceipt) {
				w.st.register(token, recipient)
				w.st.credit(token, recipient, amount)
				r.logs = append(r.logs, nep141Log(rheaVerifier, recipient, amount))
				w.notifyDeposit(recipient, r.tx.hash)
			})
			r.children = append(r.children, x.id)
			return
		}
		xargs, _ := json.Marshal(map[string]any{"receiver_id": recipient, "amount": amount.String(), "memo": nil, "msg": msg})
		x := w.child(r, rheaVerifier, token, []any{callAction("ft_transfer_call", xargs, 50_000_000_000_000, "1")}, func(r *rheaReceipt) {
			w.st.credit(token, recipient, amount)
			r.logs = append(r.logs, nep141Log(rheaVerifier, recipient, amount))
			w.transferCall(r, token, rheaVerifier, recipient, amount, msg, nil)
		})
		r.children = append(r.children, x.id)
	})
	w.log.Printf("rhea: payout of %s %s to %s in %s (msg %t)", amount, token, recipient, tx.hash, msg != "")
	return tx.hash
}

// transferCall runs the receipts after an ft_transfer_call of amount from
// sender to receiver (credited already): the receiver's ft_on_transfer
// handler, then the token's ft_resolve_transfer refunding what the
// receiver did not use. done, when set, runs after the resolution. The
// IDs of both receipts are listed under parent.
func (w *rheaWorld) transferCall(parent *rheaReceipt, token, sender, receiver string, amount *big.Int, msg string, done func(r *rheaReceipt, unused *big.Int)) {
	resolveID := randomHash()
	resolve := func(after *rheaReceipt, unused *big.Int) {
		args, _ := json.Marshal(map[string]any{"sender_id": sender, "receiver_id": receiver, "amount": amount.String()})
		w.childWithID(resolveID, after.tx, after.height, token, token, []any{callAction("ft_resolve_transfer", args, 5_000_000_000_000, "0")}, func(r *rheaReceipt) {
			if unused.Sign() > 0 && w.st.debit(token, receiver, unused) {
				w.st.credit(token, sender, unused)
				r.logs = append(r.logs, nep141Log(receiver, sender, unused))
			}
			r.status = okValue(new(big.Int).Sub(amount, unused).String())
			if done != nil {
				done(r, unused)
			}
		})
	}
	onArgs, _ := json.Marshal(map[string]any{"sender_id": sender, "amount": amount.String(), "msg": msg})
	on := w.child(parent, token, receiver, []any{callAction("ft_on_transfer", onArgs, 35_000_000_000_000, "0")}, func(r *rheaReceipt) {
		switch {
		case receiver == w.dep.AccountManager:
			w.amOnTransfer(r, token, amount, msg, resolve)
		case w.st.wallets[receiver] != nil:
			w.mcaOnTransfer(r, token, sender, amount, msg, resolve)
		default:
			r.status = failure("no contract to receive the transfer")
			resolve(r, amount)
		}
	})
	parent.children = append(parent.children, on.id, resolveID)
}

// recipientMsg is the decoded customRecipientMsg.
type recipientMsg struct {
	W []map[string]string `json:"w"`
	B struct {
		R string `json:"r"`
	} `json:"b"`
	S []string `json:"s"`
}

func parseRecipientMsg(msg string) (*recipientMsg, string, error) {
	var m recipientMsg
	if err := decodeStrict([]byte(msg), &m); err != nil {
		return nil, "", fmt.Errorf("Invalid JSON format in ft_on_transfer message: %v", err)
	}
	if len(m.W) != 1 || m.W[0]["EVM"] == "" || len(m.W[0]) != 1 {
		return nil, "", errors.New("exactly one EVM wallet is simulated")
	}
	return &m, strings.ToLower(m.W[0]["EVM"]), nil
}

// amOnTransfer is the account manager's ft_on_transfer for a creation
// delivery (BurrowRegisterAndSupply): it verifies the creation signature,
// creates the MCA, collects its fees, and forwards the rest to the new
// MCA with the same message. It keeps whatever the MCA returns.
func (w *rheaWorld) amOnTransfer(r *rheaReceipt, token string, amount *big.Int, msg string, resolve func(*rheaReceipt, *big.Int)) {
	reject := func(reason string) {
		r.status = failure(reason)
		resolve(r, amount)
	}
	m, id, err := parseRecipientMsg(msg)
	if err != nil {
		reject(err.Error())
		return
	}
	if m.B.R != "BurrowRegisterAndSupply" || len(m.S) != 1 {
		reject("the account manager only creates through BurrowRegisterAndSupply")
		return
	}
	signer, err := recoverEVM([]byte(`[{"EVM":"`+id+`"}]`), m.S[0])
	if err != nil || signer != id {
		reject("InvalidEvmSignature")
		return
	}
	if w.st.mcaByID[id] != "" {
		reject("WalletAlreadyExists")
		return
	}
	mk := w.markets[token]
	if mk == nil {
		reject("unsupported token")
		return
	}
	createFee, regFee := mk.createFee(), mk.initValue()
	fees := new(big.Int).Add(createFee, regFee)
	if amount.Cmp(fees) <= 0 {
		reject("amount does not cover the creation fee")
		return
	}
	remain := new(big.Int).Sub(amount, fees)
	mca := fmt.Sprintf("rhea%06d%s", w.st.nextMCA, w.dep.MCASuffix)
	w.st.nextMCA++

	newArgs, _ := json.Marshal(map[string]any{"wallets": []any{map[string]string{"EVM": id}}})
	create := w.child(r, w.dep.AccountManager, mca, []any{
		transferAction(rheaInitBalance.String()), "UseGlobalContract", callAction("new", newArgs, 10_000_000_000_000, "0"),
	}, func(c *rheaReceipt) {
		w.st.wallets[mca] = []string{id}
		w.st.mcaByID[id] = mca
		w.st.nonces[mca] = 0
		w.st.nearBal[mca] = new(big.Int).Set(rheaInitBalance)
	})
	cbArgs, _ := json.Marshal(map[string]any{"token_id": token, "remain_amount": remain.String(), "mca_id": mca})
	cbID := randomHash()
	r.children = []string{create.id, cbID}
	r.status = map[string]any{"SuccessReceiptId": cbID}
	w.childWithID(cbID, r.tx, r.height+1, w.dep.AccountManager, w.dep.AccountManager,
		[]any{callAction("create_mca_callback", cbArgs, 100_000_000_000_000, "0")}, func(cb *rheaReceipt) {
			cb.logs = append(cb.logs, amFeeLog("collect_create_mca_fee", token, createFee), amFeeLog("collect_dapp_registration_fee", token, regFee))
			regArgs, _ := json.Marshal(map[string]any{"account_id": mca, "registration_only": true})
			fwdArgs, _ := json.Marshal(map[string]any{"receiver_id": mca, "amount": remain.String(), "msg": msg})
			fwd := w.child(cb, w.dep.AccountManager, token, []any{
				callAction("storage_deposit", regArgs, 10_000_000_000_000, "10000000000000000000000"),
				callAction("ft_transfer_call", fwdArgs, 100_000_000_000_000, "1"),
			}, func(f *rheaReceipt) {
				w.st.register(token, mca)
				if !w.st.debit(token, w.dep.AccountManager, remain) {
					f.status = failure("The account doesn't have enough balance")
					resolve(f, amount)
					return
				}
				w.st.credit(token, mca, remain)
				f.logs = append(f.logs, nep141Log(w.dep.AccountManager, mca, remain))
				w.transferCall(f, token, w.dep.AccountManager, mca, remain, msg, func(res *rheaReceipt, unused *big.Int) {
					// The account manager keeps the fees and anything the
					// MCA returned; the verifier's transfer is fully used.
					resolve(res, new(big.Int))
				})
			})
			cb.children = append(cb.children, fwd.id)
		})
}

// mcaOnTransfer is an MCA's ft_on_transfer for a delivery: the message
// must name a wallet bound to the MCA and a supply template. The MCA
// keeps the tokens and runs the template: Burrow registration for a
// creation, then ft_transfer_call to Burrow with msg "".
func (w *rheaWorld) mcaOnTransfer(r *rheaReceipt, token, sender string, amount *big.Int, msg string, resolve func(*rheaReceipt, *big.Int)) {
	mca := r.receiver
	m, id, err := parseRecipientMsg(msg)
	bound := false
	if err == nil {
		for _, wid := range w.st.wallets[mca] {
			bound = bound || wid == id
		}
	}
	switch {
	case err != nil:
		r.status = failure(err.Error())
		resolve(r, amount)
		return
	case !bound:
		r.status = failure("WalletDoesNotExist")
		resolve(r, amount)
		return
	case m.B.R != "BurrowSupply" && m.B.R != "BurrowRegisterAndSupply":
		r.status = failure("unknown reliable business " + m.B.R)
		resolve(r, amount)
		return
	case sender == w.dep.AccountManager && w.faults.RejectForwards > 0:
		w.faults.RejectForwards--
		r.status = okValue(amount.String())
		resolve(r, amount)
		return
	}
	r.status = okValue("0")
	resolve(r, new(big.Int))
	pmArgs, _ := json.Marshal(map[string]any{"sender_id": sender, "token_id": token, "amount": amount.String(), "msg": msg})
	pm := w.child(r, mca, mca, []any{callAction("process_msg", pmArgs, 200_000_000_000_000, "0")}, func(p *rheaReceipt) {
		supply := func(after *rheaReceipt) *rheaReceipt {
			args, _ := json.Marshal(map[string]any{"amount": amount.String(), "msg": "", "receiver_id": w.dep.Burrow})
			return w.child(after, mca, token, []any{callAction("ft_transfer_call", args, 50_000_000_000_000, "1")}, func(x *rheaReceipt) {
				if !w.st.debit(token, mca, amount) {
					x.status = failure("The account doesn't have enough balance")
					return
				}
				w.st.credit(token, w.dep.Burrow, amount)
				x.logs = append(x.logs, nep141Log(mca, w.dep.Burrow, amount))
				w.supplyToBurrow(x, token, mca, amount, nil)
			})
		}
		if m.B.R == "BurrowRegisterAndSupply" && !w.st.burrowReg[mca] {
			reg := w.child(p, mca, w.dep.Burrow, []any{
				callAction("storage_deposit", `{"registration_only":true}`, 10_000_000_000_000, "100000000000000000000000"),
			}, func(s *rheaReceipt) {
				w.st.burrowReg[mca] = true
				s.status = okValue(map[string]string{"total": "100000000000000000000000", "available": "0"})
				x := supply(s)
				s.children = append(s.children, x.id)
			})
			p.children = append(p.children, reg.id)
			return
		}
		x := supply(p)
		p.children = append(p.children, x.id)
	})
	r.children = append(r.children, pm.id)
}

// The RHEA world as the 1Click side's driver for market tokens (and for
// NEAR when no NEAR harness is enabled): deposit addresses are implicit
// accounts in the ledger, and payouts are verifier payouts.

func (w *rheaWorld) Slug() string { return "near" }

func (w *rheaWorld) NewDepositAddress() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// CheckDeposit reports wrapped NEAR received at addr (native NEAR stands
// in as wrap.near inside the world).
func (w *rheaWorld) CheckDeposit(addr string) (*big.Int, string, error) {
	return w.CheckTokenDeposit(w.dep.WrappedNEAR, addr)
}

func (w *rheaWorld) Send(addr string, amount *big.Int) (string, error) {
	return w.payout(w.dep.WrappedNEAR, addr, amount, "", ""), nil
}

func (w *rheaWorld) Confirm() error { return nil }

func (w *rheaWorld) Tokens() []*driverToken { return nil }

func (w *rheaWorld) CheckTokenDeposit(contract, addr string) (*big.Int, string, error) {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	return w.st.balance(contract, addr), "", nil
}

func (w *rheaWorld) SendToken(_, addr string, amount *big.Int, _ int) (string, error) {
	return "", errors.New("the RHEA world pays out through payout")
}
