package rhea

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("error reading %s: %v", name, err)
	}
	return b
}

type execVector struct {
	TxHash             string `json:"tx_hash"`
	MCA                string `json:"mca"`
	BlockHeight        uint64 `json:"block_height"`
	GlobalContractHash string `json:"global_contract_hash"`
	ExecDeposit        string `json:"exec_deposit"`
	Args               string `json:"args"`
}

func loadExecVectors(t *testing.T) []*execVector {
	t.Helper()
	var vs []*execVector
	if err := json.Unmarshal(readTestdata(t, "exec-vectors.json"), &vs); err != nil {
		t.Fatalf("error decoding exec vectors: %v", err)
	}
	return vs
}

// TestExecVectors checks the business encoding against signed businesses
// the RHEA frontend submitted and MCAs executed on mainnet (all on MCA code
// FDX6nYTm…, version 0.1.6): decoding and re-encoding reproduces the signed
// bytes exactly, and the signature recovers the signer wallet.
func TestExecVectors(t *testing.T) {
	for _, v := range loadExecVectors(t) {
		args, err := ParseExecArgs([]byte(v.Args))
		if err != nil {
			t.Fatalf("%s: %v", v.TxHash, err)
		}
		var raw struct {
			Business json.RawMessage `json:"business"`
		}
		if err := json.Unmarshal([]byte(v.Args), &raw); err != nil {
			t.Fatalf("%s: %v", v.TxHash, err)
		}
		canon, err := args.Business.Canonical()
		if err != nil {
			t.Fatalf("%s: Canonical: %v", v.TxHash, err)
		}
		if !bytes.Equal(canon, raw.Business) {
			t.Fatalf("%s: canonical encoding differs from the signed bytes:\n%s\n%s", v.TxHash, canon, raw.Business)
		}
		signer, err := args.Business.Signer(args.Signature)
		if err != nil {
			t.Fatalf("%s: Signer: %v", v.TxHash, err)
		}
		if args.SignerWallet.Kind != WalletKindEVM || Identity(args.SignerWallet.ID) != signer {
			t.Fatalf("%s: signature recovers %s, signer wallet %s", v.TxHash, signer, args.SignerWallet)
		}
		for _, r := range args.Business.TxRequests {
			if r.Other != nil {
				t.Fatalf("%s: unmodeled request %s", v.TxHash, r.Other)
			}
		}
		if _, err := args.Business.NonceValue(); err != nil {
			t.Fatalf("%s: %v", v.TxHash, err)
		}
		if _, err := args.Business.DeadlineTime(); err != nil {
			t.Fatalf("%s: %v", v.TxHash, err)
		}
		if v.ExecDeposit != StorageDeposit {
			t.Fatalf("%s: exec deposit %s, want %s", v.TxHash, v.ExecDeposit, StorageDeposit)
		}
	}
}

func execVectorBusiness(t *testing.T, hashPrefix string) *Business {
	t.Helper()
	for _, v := range loadExecVectors(t) {
		if strings.HasPrefix(v.TxHash, hashPrefix) {
			args, err := ParseExecArgs([]byte(v.Args))
			if err != nil {
				t.Fatal(err)
			}
			return args.Business
		}
	}
	t.Fatalf("no exec vector %s", hashPrefix)
	return nil
}

func requestBytes(t *testing.T, r *TxRequest) string {
	t.Helper()
	var b bytes.Buffer
	if err := r.writeCanonical(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const (
	usdcToken  = "17208628f84f5d6ad33f0da3bbbeb27ffcb398eac501a31bd6ad2011e36133a1"
	testMCA    = "rhea001594.multica.near"
	testDepose = "0b3e64d9ca0d2a4aaff3b78494781de580da729c0bab921c3b857772c2bf3f45"
)

var testBlockTime = time.UnixMilli(1794949549020)

func validWithdrawParams() *WithdrawParams {
	return &WithdrawParams{
		MCA:            testMCA,
		TokenID:        usdcToken,
		ExtraDecimals:  12,
		Nonce:          32,
		BlockTime:      testBlockTime,
		Deadline:       DeadlineFrom(testBlockTime),
		Fee:            big.NewInt(4919),
		FeeCap:         big.NewInt(2_000_000),
		Amount:         big.NewInt(1_000_000),
		DepositAddress: testDepose,
	}
}

// liveWithdrawParams are the values of the mainnet withdrawal 4SDDqzRj….
// Its fee is 3.4 % of the amount, above Caribou's 2 % cap, so it is built
// without validation for comparison only.
func liveWithdrawParams() *WithdrawParams {
	p := validWithdrawParams()
	p.Amount = big.NewInt(143574)
	return p
}

// TestBuildWithdrawMatchesFrontend compares the built requests with the
// mainnet supply-only withdrawal 4SDDqzRj… for the same values. The fee
// and deposit-transfer requests must be byte-identical. The live business
// withdraws the whole balance (Withdraw without max_amount); Caribou's
// withdraws an exact amount, so that request is checked against a golden.
func TestBuildWithdrawMatchesFrontend(t *testing.T) {
	live := execVectorBusiness(t, "4SDDqzRj")
	if _, err := BuildWithdraw(Mainnet, liveWithdrawParams()); err == nil {
		t.Fatalf("live fee ratio passed the 2%% cap")
	}
	b, err := buildWithdraw(Mainnet, liveWithdrawParams())
	if err != nil {
		t.Fatalf("buildWithdraw: %v", err)
	}
	if len(b.TxRequests) != 3 || len(live.TxRequests) != 3 {
		t.Fatalf("request counts %d, %d", len(b.TxRequests), len(live.TxRequests))
	}
	if got, want := requestBytes(t, b.TxRequests[0]), requestBytes(t, live.TxRequests[0]); got != want {
		t.Fatalf("fee request differs:\n%s\n%s", got, want)
	}
	if got, want := requestBytes(t, b.TxRequests[2]), requestBytes(t, live.TxRequests[2]); got != want {
		t.Fatalf("transfer request differs:\n%s\n%s", got, want)
	}
	const wantExecute = `{"FunctionCall":{"receiver_id":"contract.main.burrow.near","function_calls":[{"method_name":"execute",` +
		`"args":"{\"actions\":[{\"Withdraw\":{\"token_id\":\"` + usdcToken + `\",\"max_amount\":\"143574000000000000\"}}]}",` +
		`"gas":"120000000000000","deposit":"1"}]}}`
	if got := requestBytes(t, b.TxRequests[1]); got != wantExecute {
		t.Fatalf("execute request:\n%s\nwant\n%s", got, wantExecute)
	}
	// Apart from max_amount, the execute request matches the live one.
	liveExec := strings.Replace(requestBytes(t, live.TxRequests[1]), `}}]}`, `,\"max_amount\":\"143574000000000000\"}}]}`, 1)
	if requestBytes(t, b.TxRequests[1]) != liveExec {
		t.Fatalf("execute request differs from the live request beyond max_amount")
	}
	if b.Nonce != "32" || b.Deadline != "1794949849020" {
		t.Fatalf("nonce %s deadline %s", b.Nonce, b.Deadline)
	}

	// With MCA registration, a storage_deposit for the MCA precedes the
	// withdrawal.
	p := liveWithdrawParams()
	p.RegisterMCA = true
	b, err = buildWithdraw(Mainnet, p)
	if err != nil {
		t.Fatalf("BuildWithdraw: %v", err)
	}
	const wantRegister = `{"FunctionCall":{"receiver_id":"` + usdcToken + `","function_calls":[{"method_name":"storage_deposit",` +
		`"args":"{\"registration_only\":false,\"account_id\":\"` + testMCA + `\"}","gas":"10000000000000","deposit":"1250000000000000000000"}]}}`
	if len(b.TxRequests) != 4 || requestBytes(t, b.TxRequests[1]) != wantRegister {
		t.Fatalf("registration request:\n%s", requestBytes(t, b.TxRequests[1]))
	}
	if p.StorageDeposits() != 2 || validWithdrawParams().StorageDeposits() != 1 {
		t.Fatalf("wrong storage deposit counts")
	}
}

// TestValidateWithdrawRejects mutates a valid business and valid parameters
// one way at a time; every mutation must be refused.
func TestValidateWithdrawRejects(t *testing.T) {
	now := testBlockTime.Add(time.Second)
	good, err := BuildWithdraw(Mainnet, validWithdrawParams())
	if err != nil {
		t.Fatalf("BuildWithdraw: %v", err)
	}
	if err := ValidateWithdraw(Mainnet, good, validWithdrawParams(), now); err != nil {
		t.Fatalf("valid business rejected: %v", err)
	}

	fresh := func() *Business {
		b, err := BuildWithdraw(Mainnet, validWithdrawParams())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	fc := func(b *Business, req, call int) *FunctionCall {
		return b.TxRequests[req].FunctionCall.FunctionCalls[call]
	}
	other := func(s string) *TxRequest { return &TxRequest{Other: json.RawMessage(s)} }

	businessMutations := map[string]func(b *Business){
		"nonce":        func(b *Business) { b.Nonce = "33" },
		"deadline":     func(b *Business) { b.Deadline = "1794950149020" },
		"fee receiver": func(b *Business) { b.TxRequests[0].FunctionCall.ReceiverID = "evil.near" },
		"fee method":   func(b *Business) { fc(b, 0, 0).MethodName = "withdraw" },
		"fee amount": func(b *Business) {
			fc(b, 0, 0).Args = strings.Replace(fc(b, 0, 0).Args, "4919000000000000", "9919000000000000", 1)
		},
		"fee recipient": func(b *Business) {
			fc(b, 0, 0).Args = strings.Replace(fc(b, 0, 0).Args, Mainnet.Relayer, "evil.near", 1)
		},
		"fee deposit":       func(b *Business) { fc(b, 0, 0).Deposit = "2" },
		"fee gas":           func(b *Business) { fc(b, 0, 0).Gas = "200000000000000" },
		"withdraw receiver": func(b *Business) { b.TxRequests[1].FunctionCall.ReceiverID = "evil.near" },
		"withdraw method":   func(b *Business) { fc(b, 1, 0).MethodName = "execute_with_pyth" },
		"withdraw amount": func(b *Business) {
			fc(b, 1, 0).Args = strings.Replace(fc(b, 1, 0).Args, "1000000000000000000", "1000000000000000001", 1)
		},
		"withdraw token":        func(b *Business) { fc(b, 1, 0).Args = strings.Replace(fc(b, 1, 0).Args, usdcToken, "wrap.near", 1) },
		"withdraw extra action": func(b *Business) { fc(b, 1, 0).Args = strings.Replace(fc(b, 1, 0).Args, `]}`, `,{"Borrow":{}}]}`, 1) },
		"transfer token":        func(b *Business) { b.TxRequests[2].FunctionCall.ReceiverID = "usdt.tether-token.near" },
		"transfer receiver": func(b *Business) {
			fc(b, 2, 1).Args = strings.Replace(fc(b, 2, 1).Args, testDepose, strings.Repeat("a", 64), 1)
		},
		"transfer amount":      func(b *Business) { fc(b, 2, 1).Args = strings.Replace(fc(b, 2, 1).Args, `"1000000"`, `"1000001"`, 1) },
		"transfer method":      func(b *Business) { fc(b, 2, 1).MethodName = "ft_transfer_call" },
		"registration deposit": func(b *Business) { fc(b, 2, 0).Deposit = "125000000000000000000000" },
		"registration account": func(b *Business) { fc(b, 2, 0).Args = strings.Replace(fc(b, 2, 0).Args, testDepose, "evil.near", 1) },
		"interval removed":     func(b *Business) { b.TxRequests[2].FunctionCall.IntervalBlock = nil },
		"interval changed":     func(b *Business) { n := uint64(0); b.TxRequests[2].FunctionCall.IntervalBlock = &n },
		"extra call": func(b *Business) {
			f := b.TxRequests[2].FunctionCall
			f.FunctionCalls = append(f.FunctionCalls, fc(b, 2, 1))
		},
		"request removed":          func(b *Business) { b.TxRequests = b.TxRequests[:2] },
		"requests reordered":       func(b *Business) { b.TxRequests[0], b.TxRequests[1] = b.TxRequests[1], b.TxRequests[0] },
		"appended UpgradeToLatest": func(b *Business) { b.TxRequests = append(b.TxRequests, other(`"UpgradeToLatest"`)) },
		"appended AddWallet": func(b *Business) {
			b.TxRequests = append(b.TxRequests, other(`{"AddWallet":{"wallet":{"EVM":"0000000000000000000000000000000000000001"},"signature":"00"}}`))
		},
		"appended RemoveWallet": func(b *Business) {
			b.TxRequests = append(b.TxRequests, other(`{"RemoveWallet":{"EVM":"242c9fc3162321d326892cfe044fc0d9ffc3e9b0"}}`))
		},
		"prepended Transfer": func(b *Business) {
			b.TxRequests = append([]*TxRequest{{Transfer: &Transfer{ReceiverID: "evil.near", Amount: "1"}}}, b.TxRequests...)
		},
		"appended GasPayment": func(b *Business) {
			b.TxRequests = append(b.TxRequests, &TxRequest{GasPayment: &GasPayment{TokenID: GasTokenNEAR, Amount: "1"}})
		},
		"two variants": func(b *Business) { b.TxRequests[0].Transfer = &Transfer{ReceiverID: "evil.near", Amount: "1"} },
		"nil request":  func(b *Business) { b.TxRequests[1] = nil },
	}
	for name, mutate := range businessMutations {
		b := fresh()
		mutate(b)
		if err := ValidateWithdraw(Mainnet, b, validWithdrawParams(), now); err == nil {
			t.Errorf("%s: mutated business accepted", name)
		}
	}

	paramMutations := map[string]func(p *WithdrawParams){
		"deadline beyond window":   func(p *WithdrawParams) { p.Deadline = p.BlockTime.Add(MaxDeadlineWindow + time.Millisecond) },
		"deadline at block time":   func(p *WithdrawParams) { p.Deadline = p.BlockTime },
		"deadline sub-millisecond": func(p *WithdrawParams) { p.Deadline = p.Deadline.Add(time.Microsecond) },
		"no block time":            func(p *WithdrawParams) { p.BlockTime = time.Time{} },
		"fee above cap":            func(p *WithdrawParams) { p.FeeCap = big.NewInt(4918) },
		"fee above 2%":             func(p *WithdrawParams) { p.Amount = big.NewInt(4919*50 - 1) },
		"zero fee":                 func(p *WithdrawParams) { p.Fee = big.NewInt(0) },
		"nil fee":                  func(p *WithdrawParams) { p.Fee = nil },
		"nil fee cap":              func(p *WithdrawParams) { p.FeeCap = nil },
		"zero amount":              func(p *WithdrawParams) { p.Amount = big.NewInt(0) },
		"negative amount":          func(p *WithdrawParams) { p.Amount = big.NewInt(-1) },
		"not an MCA":               func(p *WithdrawParams) { p.MCA = "evil.near" },
		"nested MCA":               func(p *WithdrawParams) { p.MCA = "a.rhea001594.multica.near" },
		"bare suffix":              func(p *WithdrawParams) { p.MCA = ".multica.near" },
		"market not allowlisted":   func(p *WithdrawParams) { p.TokenID = "eth.bridge.near" },
		"named deposit address":    func(p *WithdrawParams) { p.DepositAddress = "evil.near" },
		"uppercase deposit":        func(p *WithdrawParams) { p.DepositAddress = strings.ToUpper(testDepose) },
		"short deposit":            func(p *WithdrawParams) { p.DepositAddress = testDepose[1:] },
		"extra decimals":           func(p *WithdrawParams) { p.ExtraDecimals = maxExtraDecimals + 1 },
	}
	for name, mutate := range paramMutations {
		p := validWithdrawParams()
		mutate(p)
		// The business is built from the valid parameters; validation of
		// the mutated ones must still fail.
		if err := ValidateWithdraw(Mainnet, fresh(), p, now); err == nil {
			t.Errorf("%s: parameters accepted", name)
		}
		if _, err := BuildWithdraw(Mainnet, p); err == nil {
			t.Errorf("%s: BuildWithdraw accepted", name)
		}
	}

	// The local clock must agree with the reference block time.
	for _, skew := range []time.Duration{MaxClockSkew + time.Second, -MaxClockSkew - time.Second} {
		if err := ValidateWithdraw(Mainnet, good, validWithdrawParams(), testBlockTime.Add(skew)); err == nil {
			t.Errorf("clock skew %s accepted", skew)
		}
	}
	if err := ValidateWithdraw(Mainnet, good, validWithdrawParams(), testBlockTime.Add(MaxClockSkew)); err != nil {
		t.Errorf("clock skew at the limit rejected: %v", err)
	}
	if err := ValidateWithdraw(Mainnet, nil, validWithdrawParams(), now); err == nil {
		t.Errorf("nil business accepted")
	}
}

func TestSignWithdraw(t *testing.T) {
	k := testKey(t)
	p := validWithdrawParams()
	b, err := BuildWithdraw(Mainnet, p)
	if err != nil {
		t.Fatalf("BuildWithdraw: %v", err)
	}
	now := testBlockTime.Add(time.Second)
	req, err := k.SignWithdraw(Mainnet, b, p, now)
	if err != nil {
		t.Fatalf("SignWithdraw: %v", err)
	}
	if signer, err := b.Signer(req.Signature); err != nil || signer != k.Identity() {
		t.Fatalf("signature recovers %s, %v", signer, err)
	}
	enc, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	canon, _ := b.Canonical()
	want := `{"signer_wallet":{"EVM":"` + string(k.Identity()) + `"},"business":` + string(canon) +
		`,"signature":"` + req.Signature + `","attach_deposit":"1250000000000000000000"}`
	if string(enc) != want {
		t.Fatalf("signed request JSON:\n%s\nwant\n%s", enc, want)
	}
	// The relayer receives a request whose business re-decodes to the same
	// signed bytes.
	var back struct {
		Business *Business `json:"business"`
	}
	if err := json.Unmarshal(enc, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if again, _ := back.Business.Canonical(); !bytes.Equal(again, canon) {
		t.Fatalf("business does not round trip")
	}

	// A persisted request parses back to identical bytes.
	parsed, err := ParseSignedRequest(enc)
	if err != nil {
		t.Fatalf("ParseSignedRequest: %v", err)
	}
	if again, _ := parsed.MarshalJSON(); !bytes.Equal(again, enc) {
		t.Fatalf("signed request does not round trip:\n%s\n%s", again, enc)
	}
	for _, bad := range []string{`{}`, `{"signer_wallet":null,"business":null,"signature":"","attach_deposit":""}`, string(enc[:len(enc)-1]) + `,"x":1}`} {
		if _, err := ParseSignedRequest([]byte(bad)); err == nil {
			t.Fatalf("signed request %s accepted", bad)
		}
	}

	b.TxRequests = append(b.TxRequests, &TxRequest{Other: json.RawMessage(`"UpgradeToLatest"`)})
	if _, err := k.SignWithdraw(Mainnet, b, p, now); err == nil {
		t.Fatalf("signed an altered business")
	}
	b, _ = BuildWithdraw(Mainnet, p)
	if _, err := k.SignWithdraw(Mainnet, b, p, testBlockTime.Add(time.Hour)); err == nil {
		t.Fatalf("signed with a skewed clock")
	}
}

func TestRelayerFee(t *testing.T) {
	tests := []struct {
		name      string
		gasFee    int64
		nearValue int64
		deposits  int
		want      int64
	}{
		// USDC: 0.001 USDC base fee plus 0.00125 NEAR at 3 USDC/NEAR.
		{"usdc one deposit", 1000, 3_000_000, 1, 1000 + 3750},
		{"usdc two deposits", 1000, 3_000_000, 2, 1000 + 7500},
		{"zec", 300, 300_000, 1, 300 + 375},
		{"rounds up", 0, 1, 1, 1},
		{"no deposits", 1000, 3_000_000, 0, 1000},
	}
	for _, tt := range tests {
		got, err := RelayerFee(big.NewInt(tt.gasFee), big.NewInt(tt.nearValue), tt.deposits)
		if err != nil || got.Int64() != tt.want {
			t.Fatalf("%s: RelayerFee = %v, %v; want %d", tt.name, got, err, tt.want)
		}
	}
	// wrap.near: GAS_FEE 0.001 NEAR and 1 NEAR per NEAR.
	nv, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	gf, _ := new(big.Int).SetString("1000000000000000000000", 10)
	got, err := RelayerFee(gf, nv, 1)
	if err != nil || got.String() != "2250000000000000000000" {
		t.Fatalf("wrap.near fee %v, %v", got, err)
	}
	for name, args := range map[string][3]any{
		"nil gas fee":      {(*big.Int)(nil), big.NewInt(1), 1},
		"nil near value":   {big.NewInt(1), (*big.Int)(nil), 1},
		"zero near value":  {big.NewInt(1), big.NewInt(0), 1},
		"negative deposit": {big.NewInt(1), big.NewInt(1), -1},
	} {
		if _, err := RelayerFee(args[0].(*big.Int), args[1].(*big.Int), args[2].(int)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestInternalConversion(t *testing.T) {
	if got := ToInternal(big.NewInt(143574), 12); got.String() != "143574000000000000" {
		t.Fatalf("ToInternal %s", got)
	}
	v, _ := new(big.Int).SetString("1387814608947877", 10)
	if got := FromInternal(v, 12); got.String() != "1387" {
		t.Fatalf("FromInternal %s", got)
	}
}

func validSweepParams() *SweepParams {
	return &SweepParams{
		MCA:            "rhea001589.multica.near",
		TokenID:        "usdt.tether-token.near",
		Nonce:          37,
		BlockTime:      testBlockTime,
		Deadline:       DeadlineFrom(testBlockTime),
		GasToken:       GasTokenNEAR,
		Fee:            mustBig("2250000000000000000000"),
		FeeCap:         mustBig("10000000000000000000000"),
		NearValue:      big.NewInt(3_000_000),
		Amount:         big.NewInt(4159321),
		DepositAddress: "32408ca823619f2dd8b004e024191f8a73b73593e83ddae57d5ba2d5d73b955a",
	}
}

func mustBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(s)
	}
	return v
}

// TestBuildSweepMatchesFrontend compares a sweep to a deposit address with
// the mainnet GasPayment business 4175G6iY… for the same values.
func TestBuildSweepMatchesFrontend(t *testing.T) {
	live := execVectorBusiness(t, "4175G6iY")
	p := validSweepParams()
	b, err := BuildSweep(Mainnet, p)
	if err != nil {
		t.Fatalf("BuildSweep: %v", err)
	}
	if len(b.TxRequests) != 2 {
		t.Fatalf("%d requests", len(b.TxRequests))
	}
	for i := range b.TxRequests {
		if got, want := requestBytes(t, b.TxRequests[i]), requestBytes(t, live.TxRequests[i]); got != want {
			t.Fatalf("request %d differs:\n%s\n%s", i, got, want)
		}
	}
	now := testBlockTime.Add(time.Second)
	if err := ValidateSweep(Mainnet, b, p, now); err != nil {
		t.Fatalf("ValidateSweep: %v", err)
	}

	// Back into supply: plain ft_transfer_call to Burrow.
	p.DepositAddress = ""
	p.GasToken = p.TokenID
	p.Fee = big.NewInt(4750)
	p.FeeCap = big.NewInt(2_000_000)
	b, err = BuildSweep(Mainnet, p)
	if err != nil {
		t.Fatalf("BuildSweep: %v", err)
	}
	const want = `{"FunctionCall":{"receiver_id":"usdt.tether-token.near","function_calls":[{"method_name":"ft_transfer_call",` +
		`"args":"{\"receiver_id\":\"contract.main.burrow.near\",\"amount\":\"4159321\",\"msg\":\"\"}","gas":"50000000000000","deposit":"1"}]}}`
	if got := requestBytes(t, b.TxRequests[1]); got != want {
		t.Fatalf("supply request:\n%s\nwant\n%s", got, want)
	}
	if got := requestBytes(t, b.TxRequests[0]); got != `{"GasPayment":{"token_id":"usdt.tether-token.near","amount":"4750"}}` {
		t.Fatalf("gas payment %s", got)
	}
	k := testKey(t)
	if _, err := k.SignSweep(Mainnet, b, p, now); err != nil {
		t.Fatalf("SignSweep: %v", err)
	}
	b.TxRequests[1].FunctionCall.FunctionCalls[0].Args = strings.Replace(b.TxRequests[1].FunctionCall.FunctionCalls[0].Args, `"msg":""`, `"msg":"\"OnlyIncreaseCollateral\""`, 1)
	if _, err := k.SignSweep(Mainnet, b, p, now); err == nil {
		t.Fatalf("signed a collateral sweep")
	}

	for name, mutate := range map[string]func(p *SweepParams){
		"foreign gas token": func(p *SweepParams) { p.GasToken = "wrap.near" },
		"fee above 2%":      func(p *SweepParams) { p.Fee = big.NewInt(4159321/50 + 1); p.FeeCap = big.NewInt(1e9) },
		"fee above cap":     func(p *SweepParams) { p.FeeCap = big.NewInt(4749) },
		"bad deposit":       func(p *SweepParams) { p.DepositAddress = "evil.near" },
		"zero amount":       func(p *SweepParams) { p.Amount = big.NewInt(0) },
		"window":            func(p *SweepParams) { p.Deadline = p.BlockTime.Add(time.Hour) },
	} {
		q := *p
		mutate(&q)
		if _, err := BuildSweep(Mainnet, &q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBusinessDecodingKeepsUnmodeledVariants(t *testing.T) {
	const raw = `{"nonce":"5","deadline":"1","tx_requests":["UpgradeToLatest",` +
		`{"AddWallet":{"wallet":{"EVM":"00"},"signature":"ab"}},{"RemoveWallet":{"EVM":"00"}},` +
		`{"FunctionCall":{"receiver_id":"x.near","function_calls":[],"extra":1}},` +
		`{"Transfer":{"receiver_id":"x.near","amount":"1"}}]}`
	var b Business
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for i := range 4 {
		if b.TxRequests[i].Other == nil {
			t.Fatalf("request %d not kept verbatim", i)
		}
	}
	if b.TxRequests[4].Transfer == nil {
		t.Fatalf("transfer not decoded")
	}
	canon, err := b.Canonical()
	if err != nil || string(canon) != raw {
		t.Fatalf("canonical %s, %v", canon, err)
	}
	for _, bad := range []string{`{"nonce":"1","tx_requests":[]}`, `{"nonce":"1","deadline":"2","tx_requests":[],"x":1}`, `{"nonce":1,"deadline":"2"}`} {
		var b Business
		if err := json.Unmarshal([]byte(bad), &b); err == nil {
			t.Fatalf("business %s accepted", bad)
		}
	}
}

// TestSweepRelativeFeeCap checks the 2 % cap on sweeps whatever the fee
// token: native NEAR for wrapped NEAR compares directly (both yoctoNEAR),
// native NEAR for another token is valued at the token's near value, and a
// conversion that is needed but missing is refused.
func TestSweepRelativeFeeCap(t *testing.T) {
	now := testBlockTime.Add(time.Second)
	base := func() *SweepParams {
		return &SweepParams{
			MCA:       testMCA,
			TokenID:   Mainnet.WrappedNEAR,
			Nonce:     1,
			BlockTime: testBlockTime,
			Deadline:  DeadlineFrom(testBlockTime),
			GasToken:  GasTokenNEAR,
			Fee:       mustBig("2250000000000000000000"), // 0.00225 NEAR
			FeeCap:    mustBig("1000000000000000000000000"),
		}
	}
	check := func(name string, p *SweepParams, wantOK bool) {
		t.Helper()
		b, err := BuildSweep(Mainnet, p)
		if wantOK {
			if err != nil {
				t.Fatalf("%s: BuildSweep: %v", name, err)
			}
			if err := ValidateSweep(Mainnet, b, p, now); err != nil {
				t.Fatalf("%s: ValidateSweep: %v", name, err)
			}
			return
		}
		if err == nil {
			t.Fatalf("%s: BuildSweep accepted", name)
		}
		// Validation of a business built without checks must also fail.
		if b, err = buildSweep(Mainnet, p); err == nil {
			if err := ValidateSweep(Mainnet, b, p, now); err == nil {
				t.Fatalf("%s: ValidateSweep accepted", name)
			}
		}
	}

	// Native NEAR fee sweeping wrapped NEAR: same units.
	p := base()
	p.Amount = new(big.Int).Mul(p.Fee, big.NewInt(50)) // exactly 2 %
	check("native fee at 2%", p, true)
	p = base()
	p.Amount = new(big.Int).Sub(new(big.Int).Mul(p.Fee, big.NewInt(50)), big.NewInt(1))
	check("native fee above 2%", p, false)
	// The review probe: 1 yoctoNEAR moved for a 1e15 yoctoNEAR fee.
	p = base()
	p.Amount, p.Fee, p.FeeCap = big.NewInt(1), mustBig("1000000000000000"), mustBig("1000000000000000")
	check("dust sweep", p, false)

	// Native NEAR fee sweeping USDT at 3 USDT per NEAR: 0.00225 NEAR is
	// worth 6750 base units.
	usdt := func() *SweepParams {
		p := base()
		p.TokenID = "usdt.tether-token.near"
		p.NearValue = big.NewInt(3_000_000)
		return p
	}
	p = usdt()
	p.Amount = big.NewInt(6750 * 50)
	check("converted fee at 2%", p, true)
	p = usdt()
	p.Amount = big.NewInt(6750*50 - 1)
	check("converted fee above 2%", p, false)
	// Fractional values round up against the sweep.
	p = usdt()
	p.Fee = mustBig("2250000000000000000001")
	p.Amount = big.NewInt(6750 * 50)
	check("converted fee rounds up", p, false)
	p = usdt()
	p.NearValue = nil
	p.Amount = big.NewInt(1_000_000_000)
	check("missing near value", p, false)
	p = usdt()
	p.NearValue = big.NewInt(0)
	p.Amount = big.NewInt(1_000_000_000)
	check("zero near value", p, false)

	// A fee in the swept token itself needs no conversion.
	p = usdt()
	p.GasToken, p.NearValue = p.TokenID, nil
	p.Fee, p.FeeCap = big.NewInt(4750), big.NewInt(2_000_000)
	p.Amount = big.NewInt(4750 * 50)
	check("token fee at 2%", p, true)
	p.Amount = big.NewInt(4750*50 - 1)
	check("token fee above 2%", p, false)
}
