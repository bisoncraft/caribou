package nearintentsmock

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/decred/base58"
)

// TestZecDepositAddress checks the address encoding against an address
// derived by zcash-devtool, the alpha funder's transparent address in
// util/testing/zec/harness.sh.
func TestZecDepositAddress(t *testing.T) {
	const want = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm"
	var pkHash [20]byte
	copy(pkHash[:], base58.Decode(want)[2:22])
	if got := encodeZecP2PKH(pkHash); got != want {
		t.Fatalf("encodeZecP2PKH = %s, want %s", got, want)
	}

	d := &zecDriver{}
	a, err := d.NewDepositAddress()
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.NewDepositAddress()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "tm") || len(base58.Decode(a)) != 26 {
		t.Fatalf("not a regtest transparent address: %s", a)
	}
	if a == b {
		t.Fatalf("deposit addresses repeat: %s", a)
	}
}

// TestZecCheckDeposit checks that mined and unmined funds are summed and
// that the transaction ID comes from the mempool when the deposit is there,
// and from the address index otherwise.
func TestZecCheckDeposit(t *testing.T) {
	const addr, other = "tmDeposit", "tmOther"
	vout := func(to string, zats int64) map[string]any {
		return map[string]any{
			"valueZat":     zats,
			"scriptPubKey": map[string]any{"addresses": []string{to}},
		}
	}

	var received int64
	var mempool []string
	txs := map[string][]map[string]any{
		"pays":      {vout(other, 7), vout(addr, 50_000_000)},
		"unrelated": {vout(other, 9)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request: %v", err)
		}
		var result, rpcErr any
		switch req.Method {
		case "getaddressbalance":
			result = map[string]any{"balance": received, "received": received}
		case "getrawmempool":
			result = mempool
		case "getrawtransaction":
			var id string
			_ = json.Unmarshal(req.Params[0], &id)
			if outs, found := txs[id]; found {
				result = map[string]any{"vout": outs}
			} else {
				rpcErr = map[string]any{"code": -5, "message": "No such mempool or main chain transaction"}
			}
		case "getaddresstxids":
			result = []string{"older", "mined"}
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": rpcErr})
	}))
	defer srv.Close()
	d := &zecDriver{rpc: srv.URL}

	check := func(wantAmt int64, wantTxid string) {
		t.Helper()
		amt, txid, err := d.CheckDeposit(addr)
		if err != nil {
			t.Fatalf("CheckDeposit: %v", err)
		}
		if amt.Cmp(big.NewInt(wantAmt)) != 0 || txid != wantTxid {
			t.Fatalf("CheckDeposit = %s %q, want %d %q", amt, txid, wantAmt, wantTxid)
		}
	}

	// Nothing paid.
	mempool = []string{"unrelated"}
	check(0, "")

	// An unmined deposit. A transaction that left the mempool after it was
	// listed is skipped.
	mempool = []string{"unrelated", "gone", "pays"}
	check(50_000_000, "pays")

	// A mined deposit alone.
	received, mempool = 150_000_000, nil
	check(150_000_000, "mined")

	// Mined and unmined together.
	mempool = []string{"pays"}
	check(200_000_000, "pays")
}
