// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package oneclickmock

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTP surface of the RHEA world:
//
//	POST /rhea/rpc                       NEAR JSON-RPC: block, query
//	                                     (call_function, view_account),
//	                                     EXPERIMENTAL_tx_status, status
//	GET  /rhea/relayer/get_multichain_lending_config
//	POST /rhea/relayer/multichain_lending_requests
//	GET  /rhea/relayer/get_multichain_lending_data?batch_id=
//	GET  /rhea/relayer/get_multichain_lending_history?mca_id=&page_number=&page_size=
//	GET  /test/rhea/state                debug dump
//	GET|POST /test/rhea/faults           read or set RheaFaults (POST merges)
//	POST /test/rhea/warp                 {"seconds": n}: advance chain time
//	POST /test/rhea/nonce                {"mca","nonce"}: set an MCA's nonce
//	POST /test/rhea/fund                 {"token","account","amount"}: credit
//	                                     a plain balance ("near" for native)

func (s *Server) rheaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /rhea/rpc", s.handleRheaRPC)
	mux.HandleFunc("GET /rhea/relayer/get_multichain_lending_config", s.handleRelayerConfig)
	mux.HandleFunc("POST /rhea/relayer/multichain_lending_requests", s.handleRelayerSubmit)
	mux.HandleFunc("GET /rhea/relayer/get_multichain_lending_data", s.handleRelayerBatch)
	mux.HandleFunc("GET /rhea/relayer/get_multichain_lending_history", s.handleRelayerHistory)
	mux.HandleFunc("GET /test/rhea/state", s.handleRheaState)
	mux.HandleFunc("GET /test/rhea/faults", s.handleRheaFaults)
	mux.HandleFunc("POST /test/rhea/faults", s.handleRheaFaults)
	mux.HandleFunc("POST /test/rhea/warp", s.handleRheaWarp)
	mux.HandleFunc("POST /test/rhea/nonce", s.handleRheaNonce)
	mux.HandleFunc("POST /test/rhea/fund", s.handleRheaFund)
}

// rpcErrorBody is a NEAR JSON-RPC error.
type rpcErrorBody struct {
	Name    string    `json:"name"`
	Cause   *rpcCause `json:"cause,omitempty"`
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Data    string    `json:"data"`
}

type rpcCause struct {
	Name string         `json:"name"`
	Info map[string]any `json:"info"`
}

func handlerError(cause, data string) *rpcErrorBody {
	return &rpcErrorBody{Name: "HANDLER_ERROR", Cause: &rpcCause{Name: cause, Info: map[string]any{}}, Code: -32000,
		Message: "Server error", Data: data}
}

func unknownTx(hash string) *rpcErrorBody {
	return handlerError("UNKNOWN_TRANSACTION", "Transaction "+hash+" doesn't exist")
}

func unknownAccount(id string) *rpcErrorBody {
	return handlerError("UNKNOWN_ACCOUNT", "account "+id+" does not exist while viewing")
}

func (s *Server) handleRheaRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad JSON-RPC body: %v", err)
		return
	}
	result, rpcErr := s.rheaRPC(req.Method, req.Params)
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	writeJSON(w, resp)
}

func (s *Server) rheaRPC(method string, params json.RawMessage) (any, *rpcErrorBody) {
	w := s.rhea
	w.mtx.Lock()
	defer w.mtx.Unlock()
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	str := func(k string) string { v, _ := p[k].(string); return v }
	switch method {
	case "status":
		return map[string]any{"chain_id": "simnet-rhea", "sync_info": map[string]any{"latest_block_height": w.head().height}}, nil
	case "block":
		var blk *rheaBlock
		switch {
		case str("finality") != "":
			blk = w.final()
		case p["block_id"] != nil:
			id := p["block_id"]
			if h, ok := id.(float64); ok {
				blk = w.blockByHeight(uint64(h))
				if blk == nil {
					return nil, handlerError("UNKNOWN_BLOCK", "block not found")
				}
			} else {
				var e *rpcErrorBody
				if blk, e = w.blockAt(fmt.Sprint(id)); e != nil {
					return nil, e
				}
			}
		default:
			return nil, &rpcErrorBody{Name: "REQUEST_VALIDATION_ERROR", Code: -32700, Message: "Parse error", Data: "no block reference"}
		}
		return map[string]any{"author": "simnet", "chunks": []any{}, "header": map[string]any{
			"height": blk.height, "hash": blk.hash, "prev_hash": blk.prev, "timestamp": uint64(blk.ts.UnixNano()),
			"timestamp_nanosec": strconv.FormatInt(blk.ts.UnixNano(), 10),
		}}, nil
	case "query":
		blk, e := w.blockAt(str("block_id"))
		if e != nil {
			return nil, e
		}
		if str("block_id") == "" && str("finality") == "optimistic" {
			blk = w.head()
		}
		switch str("request_type") {
		case "call_function":
			args, err := base64.StdEncoding.DecodeString(str("args_base64"))
			if err != nil {
				return nil, handlerError("PARSE_ERROR", "bad args_base64")
			}
			res, e := w.view(blk.state, str("account_id"), str("method_name"), args)
			if e != nil {
				return nil, e
			}
			bs := make([]int, len(res))
			for i, b := range res {
				bs[i] = int(b)
			}
			return map[string]any{"result": bs, "logs": []string{}, "block_height": blk.height, "block_hash": blk.hash}, nil
		case "view_account":
			id := str("account_id")
			bal := blk.state.nearBal[id]
			if bal == nil {
				return nil, unknownAccount(id)
			}
			return map[string]any{
				"amount": bal.String(), "locked": "0", "code_hash": "11111111111111111111111111111111",
				"global_contract_hash": rheaCodeHash, "storage_usage": rheaMCAStorage, "storage_paid_at": 0,
				"block_height": blk.height, "block_hash": blk.hash,
			}, nil
		}
		return nil, handlerError("UNSUPPORTED", "request_type "+str("request_type")+" not simulated")
	case "EXPERIMENTAL_tx_status", "tx":
		return w.txStatus(str("tx_hash"), str("sender_account_id"))
	}
	return nil, &rpcErrorBody{Name: "REQUEST_VALIDATION_ERROR", Code: -32601, Message: "Method not found", Data: method}
}

// view runs a contract view against a state snapshot.
func (w *rheaWorld) view(st *rheaState, contract, method string, args []byte) ([]byte, *rpcErrorBody) {
	var a map[string]any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &a)
	}
	str := func(k string) string { v, _ := a[k].(string); return v }
	out := func(v any) ([]byte, *rpcErrorBody) {
		b, _ := json.Marshal(v)
		return b, nil
	}
	panicked := func(msg string) ([]byte, *rpcErrorBody) {
		return nil, handlerError("CONTRACT_EXECUTION_ERROR", "wasm execution failed with error: FunctionCallError(ExecutionError(\"Smart contract panicked: "+msg+"\"))")
	}
	switch {
	case contract == w.dep.AccountManager:
		switch method {
		case "get_mca_by_wallet":
			wallet, _ := a["wallet"].(map[string]any)
			id, _ := wallet["EVM"].(string)
			if mca := st.mcaByID[strings.ToLower(id)]; mca != "" && id == strings.ToLower(id) {
				return out(mca)
			}
			return []byte("null"), nil
		case "list_wallets_by_mca":
			ws := []any{}
			for _, id := range st.wallets[str("mca_id")] {
				ws = append(ws, map[string]string{"EVM": id})
			}
			return out(ws)
		case "get_create_mca_fee":
			if m := w.markets[str("token_id")]; m != nil {
				return out(m.createFee().String())
			}
			return panicked("token not supported")
		case "get_near_value":
			if m := w.markets[str("token_id")]; m != nil {
				return out(m.nearValue().String())
			}
			if str("token_id") == w.dep.WrappedNEAR {
				return out(yoctoPerNEAR.String())
			}
			return panicked("token not supported")
		case "get_mca_init_balance":
			return out(rheaInitBalance.String())
		case "get_latest_mca_wasm_code_hash":
			return out(rheaCodeHash)
		case "get_reliable_business_template_paged":
			ts := map[string]string{}
			for _, name := range []string{"BurrowSupply", "BurrowRegisterAndSupply"} {
				if t, ok := w.dep.PinnedTemplate(name); ok {
					ts[name] = t
				}
			}
			return out(ts)
		}
	case w.dep.IsMCA(contract):
		if _, ok := st.wallets[contract]; !ok {
			return nil, unknownAccount(contract)
		}
		switch method {
		case "get_nonce":
			return out(strconv.FormatUint(st.nonces[contract], 10))
		case "get_version":
			return out(rheaMCAVersion)
		}
	case contract == w.dep.Burrow:
		switch method {
		case "get_asset":
			return w.assetView(st, str("token_id"))
		case "get_account_all_positions":
			acct := str("account_id")
			if !st.burrowReg[acct] {
				return []byte("null"), nil
			}
			supplied := []any{}
			for tok, v := range st.supplied[acct] {
				if v.Sign() > 0 {
					supplied = append(supplied, map[string]string{"token_id": tok, "balance": v.String(), "shares": v.String(), "apr": "0"})
				}
			}
			return out(map[string]any{
				"account_id": acct, "supplied": supplied, "positions": map[string]any{}, "farms": []any{},
				"has_non_farmed_assets": false, "booster_staking": nil, "booster_stakings": map[string]any{}, "is_locked": false,
			})
		}
	case w.isToken(contract):
		switch method {
		case "ft_metadata":
			dec, sym := 24, "wNEAR"
			if m := w.markets[contract]; m != nil {
				dec, sym = m.decimals, m.symbol
			}
			return out(map[string]any{"spec": "ft-1.0.0", "name": sym, "symbol": sym, "icon": nil,
				"reference": nil, "reference_hash": nil, "decimals": dec})
		case "ft_balance_of":
			return out(st.balance(contract, str("account_id")).String())
		case "storage_balance_of":
			if st.ftReg[contract][str("account_id")] {
				return out(map[string]string{"total": "1250000000000000000000", "available": "0"})
			}
			return []byte("null"), nil
		}
	default:
		return nil, unknownAccount(contract)
	}
	return panicked("MethodNotFound " + method)
}

// assetView renders Burrow's get_asset.
func (w *rheaWorld) assetView(st *rheaState, token string) ([]byte, *rpcErrorBody) {
	a := st.assets[token]
	if a == nil {
		return nil, handlerError("CONTRACT_EXECUTION_ERROR", "Smart contract panicked: Asset not found")
	}
	b, _ := json.Marshal(map[string]any{
		"token_id":              token,
		"supplied":              map[string]string{"shares": a.supplied.String(), "balance": a.supplied.String()},
		"borrowed":              map[string]string{"shares": a.borrowed.String(), "balance": a.borrowed.String()},
		"margin_debt":           map[string]string{"shares": "0", "balance": "0"},
		"margin_pending_debt":   "0",
		"margin_position":       "0",
		"reserved":              a.reserved.String(),
		"prot_fee":              "0",
		"beneficiary_fees":      map[string]string{},
		"last_update_timestamp": strconv.FormatInt(time.Now().UnixNano(), 10),
		"supply_apr":            "0.05",
		"borrow_apr":            "0.08",
		"config": map[string]any{
			"reserve_ratio": 2500, "target_utilization": 8000,
			"extra_decimals": w.extraDecimals(token), "can_deposit": a.canDeposit, "can_withdraw": a.canWithdraw,
			"can_use_as_collateral": true, "can_borrow": true, "net_tvl_multiplier": 10000,
			"supplied_limit": a.suppliedLimit.String(), "borrowed_limit": a.suppliedLimit.String(),
		},
	})
	return b, nil
}

func (s *Server) handleRelayerConfig(w http.ResponseWriter, _ *http.Request) {
	fees := map[string]string{"near": rheaGasFeeNEAR.String(), s.rhea.dep.WrappedNEAR: rheaGasFeeNEAR.String()}
	for tok, m := range s.rhea.markets {
		fees[tok] = m.gasFee().String()
	}
	fb, _ := json.Marshal(fees)
	writeJSON(w, []map[string]string{
		{"key": "COLLECT_FEE_ACCOUNT_ID", "value": s.rhea.dep.Relayer},
		{"key": "GAS_FEE", "value": string(fb)},
		{"key": "MCA_VERSION", "value": rheaMCAVersion},
	})
}

func (s *Server) handleRelayerSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MCAID   string   `json:"mca_id"`
		Wallet  string   `json:"wallet"`
		Request []string `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Request) != 1 {
		writeJSON(w, map[string]any{"code": 400, "data": "", "msg": "bad request"})
		return
	}
	batchID, err := s.rhea.submit(req.MCAID, req.Wallet, req.Request[0])
	if err != nil {
		writeJSON(w, map[string]any{"code": 500, "data": "", "msg": err.Error()})
		return
	}
	s.rhea.mtx.Lock()
	lost := s.rhea.faults.LostResponses > 0
	if lost {
		s.rhea.faults.LostResponses--
	}
	s.rhea.mtx.Unlock()
	if lost {
		s.log.Printf("rhea: dropping the reply for batch %s (fault injection)", batchID)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"code": 0, "data": batchID, "msg": "success"})
}

func (s *Server) handleRelayerBatch(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("batch_id")
	s.rhea.mtx.Lock()
	defer s.rhea.mtx.Unlock()
	b := s.rhea.batches[id]
	if b == nil {
		writeJSON(w, []any{})
		return
	}
	var result any
	if b.txHash != "" {
		rb, _ := json.Marshal(map[string]string{"tx_hash": b.txHash, "tx_err_msg": b.txErr, "other_err_msg": ""})
		result = string(rb)
	}
	writeJSON(w, []any{map[string]any{
		"id": 1, "mca_id": b.mca, "wallet": b.wallet, "request": b.request, "request_hash": b.hash,
		"batch_id": b.id, "batch_status": b.status, "sequence": 0, "request_result": result,
	}})
}

func (s *Server) handleRelayerHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mca := q.Get("mca_id")
	page, _ := strconv.Atoi(q.Get("page_number"))
	size, _ := strconv.Atoi(q.Get("page_size"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 10
	}
	s.rhea.mtx.Lock()
	all := s.rhea.history[mca]
	recs := []any{}
	for i := (page - 1) * size; i < len(all) && i < page*size; i++ {
		h := all[i]
		recs = append(recs, map[string]any{
			"id": h.id, "type": 1, "mca_id": h.mca, "wallet": h.wallet, "request_hash": h.txHash,
			"batch_id": h.batchID, "status": h.status, "request_result": nil, "created_at": h.created,
			"updated_at": h.created, "completed_at": h.created, "page_display_data": "{}",
			"dest_tx_hash": nil, "dest_chain": nil,
		})
	}
	total := len(all)
	s.rhea.mtx.Unlock()
	writeJSON(w, map[string]any{
		"record_list": recs, "page_number": page, "page_size": size,
		"total_page": (total + size - 1) / size, "total_size": total,
	})
}

func (s *Server) handleRheaState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.rhea.debugState())
}

func (s *Server) handleRheaFaults(w http.ResponseWriter, r *http.Request) {
	s.rhea.mtx.Lock()
	defer s.rhea.mtx.Unlock()
	if r.Method == http.MethodPost {
		// Unmarshalling over the current faults merges: only the members
		// present change.
		f := s.rhea.faults
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			writeError(w, http.StatusBadRequest, "Bad Request", "bad faults: %v", err)
			return
		}
		s.rhea.faults = f
		s.log.Printf("rhea: faults now %+v", f)
	}
	writeJSON(w, s.rhea.faults)
}

func (s *Server) handleRheaWarp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds float64 `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Seconds < 0 {
		writeError(w, http.StatusBadRequest, "Bad Request", "need a non-negative seconds")
		return
	}
	s.rhea.Warp(time.Duration(req.Seconds * float64(time.Second)))
	writeJSON(w, map[string]string{"warp": s.rhea.WarpOffset().String()})
}

func (s *Server) handleRheaNonce(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MCA   string `json:"mca"`
		Nonce uint64 `json:"nonce"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad body: %v", err)
		return
	}
	s.rhea.mtx.Lock()
	defer s.rhea.mtx.Unlock()
	if _, ok := s.rhea.st.wallets[req.MCA]; !ok {
		writeError(w, http.StatusNotFound, "Not Found", "unknown MCA %s", req.MCA)
		return
	}
	s.rhea.st.nonces[req.MCA] = req.Nonce
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleRheaFund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token   string `json:"token"`
		Account string `json:"account"`
		Amount  string `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad body: %v", err)
		return
	}
	amt, ok := new(big.Int).SetString(req.Amount, 10)
	if !ok || amt.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "Bad Request", "bad amount %q", req.Amount)
		return
	}
	if err := s.rhea.Fund(req.Token, req.Account, amt); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "%v", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// Warp advances the simulated chain clock by d (cumulative).
func (w *rheaWorld) Warp(d time.Duration) {
	w.mtx.Lock()
	w.warp += d
	w.mtx.Unlock()
}

// WarpOffset is the chain clock's lead over the host clock.
func (w *rheaWorld) WarpOffset() time.Duration {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	return w.warp
}

// Fund credits account's plain balance of token ("near" for native NEAR)
// out of nowhere: a balance of unknown origin, as after a restore.
func (w *rheaWorld) Fund(token, account string, amt *big.Int) error {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	if token == "near" {
		if w.st.nearBal[account] == nil {
			return fmt.Errorf("unknown account %s", account)
		}
		w.st.nearBal[account].Add(w.st.nearBal[account], amt)
		return nil
	}
	if !w.isToken(token) {
		return fmt.Errorf("unknown token %s", token)
	}
	w.st.register(token, account)
	w.st.credit(token, account, amt)
	return nil
}

// SetFaults replaces the fault injections.
func (w *rheaWorld) SetFaults(f RheaFaults) {
	w.mtx.Lock()
	w.faults = f
	w.mtx.Unlock()
}

// Faults returns the current fault injections.
func (w *rheaWorld) Faults() RheaFaults {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	return w.faults
}
