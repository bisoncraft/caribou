//go:build harness

package masternode

// This test checks Terms.Validate against Dash Core v24, so the rules copied
// from its source cannot drift from what the node accepts. It only calls
// protx shared_register_prepare, which builds and checks a registration and
// broadcasts nothing.
//
// Prerequisites (run by the user): the Dash regtest harness from
// util/testing/dash with V24 active (height 750 or more) and wallet gamma
// funded. The node is alpha on port 20777.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"testing"

	"github.com/btcsuite/btcd/btcutil/base58"

	"github.com/bisoncraft/caribou/wallet/asset/dash/protx"
)

const harnessURL = "http://user:pass@127.0.0.1:20777/wallet/gamma"

func rpc(t *testing.T, method string, params ...any) (json.RawMessage, error) {
	t.Helper()
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(harnessURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Skipf("harness not reachable: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s", out.Error.Message)
	}
	return out.Result, nil
}

func rpcStr(t *testing.T, method string, params ...any) string {
	t.Helper()
	r, err := rpc(t, method, params...)
	if err != nil {
		t.Fatal(err)
	}
	var s string
	if err := json.Unmarshal(r, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// addr returns a new wallet address, its script and its key id.
func addr(t *testing.T) (string, []byte, [20]byte) {
	t.Helper()
	a := rpcStr(t, "getnewaddress")
	r, err := rpc(t, "validateaddress", a)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		ScriptPubKey string `json:"scriptPubKey"`
	}
	if err := json.Unmarshal(r, &v); err != nil {
		t.Fatal(err)
	}
	script := mustHexT(t, v.ScriptPubKey)
	payload, _, err := base58.CheckDecode(a)
	if err != nil {
		t.Fatal(err)
	}
	var id [20]byte
	copy(id[:], payload)
	return a, script, id
}

func p2pkhScript(id [20]byte) []byte {
	return append(append([]byte{0x76, 0xa9, 0x14}, id[:]...), 0x88, 0xac)
}

func mustHexT(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateMatchesDashCore(t *testing.T) {
	// Funding transaction: coins worth more than 1000 DASH, one change
	// output. It is only prepared, never signed or sent.
	r, err := rpc(t, "listunspent")
	if err != nil {
		t.Fatal(err)
	}
	var utxos []struct {
		TxID    string  `json:"txid"`
		Vout    int     `json:"vout"`
		Amount  float64 `json:"amount"`
		Spend   bool    `json:"spendable"`
		Address string  `json:"address"`
	}
	if err := json.Unmarshal(r, &utxos); err != nil {
		t.Fatal(err)
	}
	var ins []map[string]any
	var total float64
	for _, u := range utxos {
		if !u.Spend {
			continue
		}
		ins = append(ins, map[string]any{"txid": u.TxID, "vout": u.Vout})
		total += u.Amount
		if total >= 1001 {
			break
		}
	}
	if total < 1001 {
		t.Skipf("wallet gamma has %.2f DASH, need 1001", total)
	}
	changeAddr, _, _ := addr(t)
	fund := rpcStr(t, "createrawtransaction", ins, map[string]float64{changeAddr: total - 1000 - 0.01})

	opJSON, err := rpc(t, "bls", "generate")
	if err != nil {
		t.Fatal(err)
	}
	var opKey struct {
		Public string `json:"public"`
	}
	if err := json.Unmarshal(opJSON, &opKey); err != nil {
		t.Fatal(err)
	}
	votingAddr, _, votingID := addr(t)

	type args struct {
		amounts []int64
		ip      string
		reward  float64 // percent
		early   uint32
		penalty int64
		// payee makes share 0 refund to the owner key of "own" or "other"
		// or to the "voting" key. votingIsOwner makes share 0's owner key
		// the voting key.
		payee         string
		votingIsOwner bool
	}
	base := func() args {
		return args{amounts: []int64{500e8, 500e8}, ip: "127.0.0.1:19996", early: 10, penalty: 5e8}
	}
	cases := []struct {
		name   string
		mutate func(a *args)
	}{
		{"valid", func(a *args) {}},
		{"valid no early period", func(a *args) { a.early, a.penalty = 0, 0 }},
		{"valid empty address", func(a *args) { a.ip = "" }},
		{"valid 8 shares", func(a *args) { a.amounts = []int64{125e8, 125e8, 125e8, 125e8, 125e8, 125e8, 125e8, 125e8} }},
		{"valid uneven shares", func(a *args) { a.amounts = []int64{900e8, 100e8} }},
		{"one share", func(a *args) { a.amounts = []int64{1000e8} }},
		{"nine shares", func(a *args) { a.amounts = []int64{100e8, 100e8, 100e8, 100e8, 100e8, 100e8, 100e8, 100e8, 200e8} }},
		{"share below minimum", func(a *args) { a.amounts = []int64{950e8, 50e8} }},
		{"sum short", func(a *args) { a.amounts = []int64{500e8, 400e8} }},
		{"sum long", func(a *args) { a.amounts = []int64{500e8, 600e8} }},
		{"penalty at smallest share", func(a *args) { a.penalty = 500e8 }},
		{"penalty one below smallest", func(a *args) { a.penalty = 500e8 - 1 }},
		{"negative penalty", func(a *args) { a.penalty = -1 }},
		{"penalty without period", func(a *args) { a.early = 0 }},
		{"early period at max", func(a *args) { a.early = MaxEarlyPeriodBlocks }},
		{"early period over max", func(a *args) { a.early = MaxEarlyPeriodBlocks + 1 }},
		{"operator reward 100%", func(a *args) { a.reward = 100 }},
		{"port 0", func(a *args) { a.ip = "127.0.0.1:0" }},
		{"port 1023", func(a *args) { a.ip = "127.0.0.1:1023" }},
		{"port 1024", func(a *args) { a.ip = "127.0.0.1:1024" }},
		{"port 8333", func(a *args) { a.ip = "127.0.0.1:8333" }},
		{"port 6667", func(a *args) { a.ip = "127.0.0.1:6667" }},
		{"port 9999", func(a *args) { a.ip = "127.0.0.1:9999" }},
		{"rfc1918 on regtest", func(a *args) { a.ip = "10.1.2.3:19996" }},
		{"ipv6 first address", func(a *args) { a.ip = "[2606:4700::1]:19996" }},
		{"refund pays own owner key", func(a *args) { a.payee = "own" }},
		{"refund pays another owner key", func(a *args) { a.payee = "other" }},
		{"refund pays voting key", func(a *args) { a.payee = "voting" }},
		{"voting key is an owner key", func(a *args) { a.votingIsOwner = true }},
		{"unspecified address", func(a *args) { a.ip = "0.0.0.0:19996" }},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a := base()
			tt.mutate(&a)

			terms := Terms{
				KeyIDVoting:       votingID,
				OperatorReward:    uint16(a.reward * 100),
				EarlyPeriodBlocks: a.early,
				EarlyPenalty:      a.penalty,
			}
			bls, err := hex.DecodeString(opKey.Public)
			if err != nil {
				t.Fatal(err)
			}
			copy(terms.PubKeyOperator[:], bls)
			type who struct {
				refundAddr, ownerAddr string
				refund                []byte
				owner                 [20]byte
			}
			var ws []who
			for range a.amounts {
				var w who
				w.refundAddr, w.refund, _ = addr(t)
				w.ownerAddr, _, w.owner = addr(t)
				ws = append(ws, w)
			}
			voting := votingAddr
			if a.votingIsOwner {
				voting, terms.KeyIDVoting = ws[0].ownerAddr, ws[0].owner
			}
			// Share 0 may refund to a key the registration uses.
			switch a.payee {
			case "own":
				ws[0].refundAddr, ws[0].refund = ws[0].ownerAddr, p2pkhScript(ws[0].owner)
			case "other":
				ws[0].refundAddr, ws[0].refund = ws[1].ownerAddr, p2pkhScript(ws[1].owner)
			case "voting":
				ws[0].refundAddr, ws[0].refund = votingAddr, p2pkhScript(votingID)
			}
			var shares []map[string]any
			for i, amt := range a.amounts {
				w := ws[i]
				shares = append(shares, map[string]any{
					"amount": amt, "refundAddress": w.refundAddr, "rewardAddress": w.refundAddr, "ownerAddress": w.ownerAddr,
				})
				terms.Shares = append(terms.Shares, protx.Share{
					Amount: amt, RefundScript: w.refund, RewardScript: w.refund, KeyIDOwner: w.owner,
				})
			}
			if a.ip == "" {
				terms.NetInfo = protx.NetInfo{Version: 1}
			} else if ap, err := netip.ParseAddrPort(a.ip); err == nil {
				terms.NetInfo, _ = CoreP2PNetInfo(ap)
			}

			_, coreErr := rpc(t, "protx", "shared_register_prepare", fund, shares,
				a.ip, opKey.Public, voting, fmt.Sprintf("%.2f", a.reward), a.early, a.penalty)
			ourErr := terms.Validate(RegtestRules)

			if (coreErr == nil) != (ourErr == nil) {
				t.Fatalf("Core err = %v, ours = %v", coreErr, ourErr)
			}
		})
	}
}
