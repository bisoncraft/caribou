// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package oneclickmock

// The RHEA world: a simulated RHEA lending deployment (account manager,
// MCAs, Burrow, and NEP-141 market tokens) on a simulated NEAR chain. The
// wallet's lending engine reaches it through a NEAR JSON-RPC subset and the
// relayer API (rhea_http.go); 1Click payouts to RHEA accounts are delivered
// into it (rhea_deliver.go); relayer businesses execute in it
// (rhea_exec.go).
//
// The chain produces a block every BlockInterval. Each block snapshots the
// whole state, so views at a named block and stale finality (a final block
// lagging the head) are served from history. Transactions execute as
// receipts, each in its own block: a business's nonce is consumed by the
// exec receipt, and every child receipt runs later, so a transaction stays
// incomplete (EXECUTED_OPTIMISTIC, outcomes missing) until its last receipt
// executes and that block is final.
//
// Signature verification and the business encoding it covers are written
// here independently of wallet/rhea, so the mock does not agree with the
// wallet by sharing its code.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/bisoncraft/caribou/wallet/rhea"
	"github.com/btcsuite/btcd/btcutil/base58"
)

// RheaConfig enables the simulated RHEA world.
type RheaConfig struct {
	// Deployment names the simulated contracts and markets. Default
	// rhea.Simnet.
	Deployment *rhea.Deployment
	// BlockInterval is the simulated NEAR block time. Default 1s.
	BlockInterval time.Duration
}

// RheaFaults are the RHEA world's fault injections, set through POST
// /test/rhea/faults. Counters apply to the next N occurrences.
type RheaFaults struct {
	// LostResponses: the next N relayer submissions are executed, but the
	// HTTP reply is lost (the connection is closed without a response).
	LostResponses int `json:"lostResponses"`
	// ExecDelay delays the inclusion of every exec after submission; a
	// delay past the business deadline makes the exec fail with
	// DeadlineExpired, leaving the nonce unconsumed.
	ExecDelay time.Duration `json:"execDelay"`
	// HoldReceipts holds every receipt except a transaction's first, so
	// an exec consumes its nonce while its child receipts wait, for as
	// long as the hold stands (past the admission window, for example).
	HoldReceipts bool `json:"holdReceipts"`
	// ReceiptDelayBlocks adds blocks before every child receipt.
	ReceiptDelayBlocks int `json:"receiptDelayBlocks"`
	// FinalLag is how many blocks the final block lags the head (stale
	// finalized views). Default 2.
	FinalLag int `json:"finalLag"`
	// FailWithdraws: the next N Burrow execute Withdraw calls fail.
	FailWithdraws int `json:"failWithdraws"`
	// RejectBurrowDeposits: Burrow refuses the next N deposits
	// (ft_on_transfer returns the full amount), so the tokens are refunded
	// to the sender: into the MCA for a delivery or a sweep into supply.
	RejectBurrowDeposits int `json:"rejectBurrowDeposits"`
	// RejectForwards: a new MCA refuses the account manager's next N
	// forwards of a creation delivery, so the tokens return to the
	// account manager.
	RejectForwards int `json:"rejectForwards"`
}

// Fixed parameters of the simulated deployment.
const (
	rheaVerifier = "intents.near"
	// rheaHistoryBlocks bounds the retained block history (views at older
	// blocks fail as garbage-collected).
	rheaHistoryBlocks = 4000
	// rheaCodeHash is every simulated MCA's global contract code hash.
	rheaCodeHash   = "SimnetMCACode1111111111111111111111111111111"
	rheaMCAStorage = 330
	rheaMCAVersion = "0.1.6"
)

var (
	yoctoPerNEAR = new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	// rheaInitBalance is the NEAR a new MCA is created with (0.1 NEAR).
	rheaInitBalance = new(big.Int).Div(yoctoPerNEAR, big.NewInt(10))
	// rheaStoragePrice is the storage staking price per byte.
	rheaStoragePrice = new(big.Int).Exp(big.NewInt(10), big.NewInt(19), nil)
	// rheaGasFeeNEAR is the relayer's native NEAR fee (0.001 NEAR).
	rheaGasFeeNEAR = new(big.Int).Exp(big.NewInt(10), big.NewInt(21), nil)
)

// rheaMarket is a simulated market token and its Burrow asset parameters.
type rheaMarket struct {
	symbol        string
	tokenID       string
	decimals      int
	extraDecimals int
}

// rheaAsset is Burrow's state for one token, in internal units.
type rheaAsset struct {
	supplied, borrowed, reserved *big.Int
	canDeposit, canWithdraw      bool
	suppliedLimit                *big.Int
}

// rheaState is the RHEA world's state at one point in the chain.
type rheaState struct {
	ft        map[string]map[string]*big.Int // token → account → balance
	ftReg     map[string]map[string]bool     // token → registered accounts
	nearBal   map[string]*big.Int            // MCA → native NEAR (yocto)
	mcaByID   map[string]string              // EVM identity (lower hex) → MCA
	wallets   map[string][]string            // MCA → EVM identities
	nonces    map[string]uint64
	nextMCA   int
	assets    map[string]*rheaAsset
	supplied  map[string]map[string]*big.Int // account → token → internal
	burrowReg map[string]bool
}

func newRheaState() *rheaState {
	return &rheaState{
		ft:        make(map[string]map[string]*big.Int),
		ftReg:     make(map[string]map[string]bool),
		nearBal:   make(map[string]*big.Int),
		mcaByID:   make(map[string]string),
		wallets:   make(map[string][]string),
		nonces:    make(map[string]uint64),
		nextMCA:   1,
		assets:    make(map[string]*rheaAsset),
		supplied:  make(map[string]map[string]*big.Int),
		burrowReg: make(map[string]bool),
	}
}

func cloneBig(x *big.Int) *big.Int {
	if x == nil {
		return nil
	}
	return new(big.Int).Set(x)
}

func (s *rheaState) clone() *rheaState {
	c := newRheaState()
	for tok, m := range s.ft {
		cm := make(map[string]*big.Int, len(m))
		for a, v := range m {
			cm[a] = cloneBig(v)
		}
		c.ft[tok] = cm
	}
	for tok, m := range s.ftReg {
		cm := make(map[string]bool, len(m))
		for a, v := range m {
			cm[a] = v
		}
		c.ftReg[tok] = cm
	}
	for a, v := range s.nearBal {
		c.nearBal[a] = cloneBig(v)
	}
	for k, v := range s.mcaByID {
		c.mcaByID[k] = v
	}
	for k, v := range s.wallets {
		c.wallets[k] = append([]string(nil), v...)
	}
	for k, v := range s.nonces {
		c.nonces[k] = v
	}
	c.nextMCA = s.nextMCA
	for tok, a := range s.assets {
		ca := *a
		ca.supplied, ca.borrowed, ca.reserved = cloneBig(a.supplied), cloneBig(a.borrowed), cloneBig(a.reserved)
		ca.suppliedLimit = cloneBig(a.suppliedLimit)
		c.assets[tok] = &ca
	}
	for acct, m := range s.supplied {
		cm := make(map[string]*big.Int, len(m))
		for t, v := range m {
			cm[t] = cloneBig(v)
		}
		c.supplied[acct] = cm
	}
	for k, v := range s.burrowReg {
		c.burrowReg[k] = v
	}
	return c
}

func (s *rheaState) balance(token, account string) *big.Int {
	if v := s.ft[token][account]; v != nil {
		return new(big.Int).Set(v)
	}
	return new(big.Int)
}

func (s *rheaState) credit(token, account string, amt *big.Int) {
	m := s.ft[token]
	if m == nil {
		m = make(map[string]*big.Int)
		s.ft[token] = m
	}
	if m[account] == nil {
		m[account] = new(big.Int)
	}
	m[account].Add(m[account], amt)
}

// debit takes amt from account, reporting false (and changing nothing)
// when the balance is short.
func (s *rheaState) debit(token, account string, amt *big.Int) bool {
	bal := s.ft[token][account]
	if bal == nil || bal.Cmp(amt) < 0 {
		return false
	}
	bal.Sub(bal, amt)
	return true
}

func (s *rheaState) register(token, account string) {
	m := s.ftReg[token]
	if m == nil {
		m = make(map[string]bool)
		s.ftReg[token] = m
	}
	m[account] = true
}

func (s *rheaState) suppliedOf(account, token string) *big.Int {
	if v := s.supplied[account][token]; v != nil {
		return new(big.Int).Set(v)
	}
	return new(big.Int)
}

func (s *rheaState) addSupplied(account, token string, delta *big.Int) {
	m := s.supplied[account]
	if m == nil {
		m = make(map[string]*big.Int)
		s.supplied[account] = m
	}
	if m[token] == nil {
		m[token] = new(big.Int)
	}
	m[token].Add(m[token], delta)
	if a := s.assets[token]; a != nil {
		a.supplied.Add(a.supplied, delta)
	}
}

// unlockedNEAR is an MCA's NEAR balance less its storage staking.
func (s *rheaState) unlockedNEAR(mca string) *big.Int {
	bal := cloneBig(s.nearBal[mca])
	if bal == nil {
		return new(big.Int)
	}
	locked := new(big.Int).Mul(big.NewInt(rheaMCAStorage), rheaStoragePrice)
	bal.Sub(bal, locked)
	if bal.Sign() < 0 {
		bal.SetInt64(0)
	}
	return bal
}

// rheaBlock is one simulated block with the state after it.
type rheaBlock struct {
	height uint64
	hash   string
	prev   string
	ts     time.Time
	state  *rheaState
}

// rheaReceipt is one receipt of a simulated transaction. run executes it
// against the head state (under the world lock), filling logs, status,
// and children.
type rheaReceipt struct {
	id          string
	tx          *rheaTx
	predecessor string
	receiver    string
	actions     []any
	// first marks a transaction's first receipt; local marks a first
	// receipt the transaction's own signer executes (signer ==
	// receiver), which endpoints report only as an outcome.
	first bool
	local bool
	at    uint64
	run   func(r *rheaReceipt)

	done      bool
	height    uint64
	blockHash string
	logs      []string
	status    map[string]any
	children  []string
}

// rheaTx is a simulated transaction.
type rheaTx struct {
	hash      string
	signer    string
	receiver  string
	actions   []any
	first     string
	includeAt time.Time
	included  uint64
	inclBlock string
	receipts  []*rheaReceipt
	onInclude func(tx *rheaTx)
}

// rheaBatch is a relayer submission.
type rheaBatch struct {
	id, mca, wallet, request, hash string
	status                         int
	txHash, txErr                  string
}

// rheaHistory is one relayer history record.
type rheaHistory struct {
	id                       int64
	mca, wallet, txHash      string
	batchID, status, created string
}

// rheaWorld is the simulated deployment and chain.
type rheaWorld struct {
	dep      *rhea.Deployment
	markets  map[string]*rheaMarket
	interval time.Duration
	log      *log.Logger
	// poke tells the 1Click side that a deposit address received tokens.
	poke func(addr, txHash string)

	mtx       sync.Mutex
	st        *rheaState
	blocks    []*rheaBlock
	byHash    map[string]*rheaBlock
	warp      time.Duration
	queue     []*rheaReceipt
	pendingTx []*rheaTx
	txs       map[string]*rheaTx
	batches   map[string]*rheaBatch
	history   map[string][]*rheaHistory
	histID    int64
	faults    RheaFaults
	// producing is the block whose receipts are executing.
	producing *rheaBlock
	// pokes collects deposit notifications raised under the lock.
	pokes [][2]string
}

func newRheaWorld(cfg *RheaConfig, logger *log.Logger) *rheaWorld {
	dep := cfg.Deployment
	if dep == nil {
		dep = rhea.Simnet
	}
	interval := cfg.BlockInterval
	if interval == 0 {
		interval = time.Second
	}
	w := &rheaWorld{
		dep:      dep,
		markets:  make(map[string]*rheaMarket),
		interval: interval,
		log:      logger,
		st:       newRheaState(),
		byHash:   make(map[string]*rheaBlock),
		txs:      make(map[string]*rheaTx),
		batches:  make(map[string]*rheaBatch),
		history:  make(map[string][]*rheaHistory),
		faults:   RheaFaults{FinalLag: 2},
	}
	for _, m := range dep.Markets {
		mk := &rheaMarket{symbol: m.Symbol, tokenID: m.TokenID, decimals: 6, extraDecimals: 12}
		w.markets[m.TokenID] = mk
		// Ample liquidity: ten million tokens supplied by others.
		liquidity := new(big.Int).Mul(big.NewInt(10_000_000), tenPow(mk.decimals+mk.extraDecimals))
		w.st.assets[m.TokenID] = &rheaAsset{
			supplied: liquidity, borrowed: new(big.Int), reserved: new(big.Int),
			canDeposit: true, canWithdraw: true,
			suppliedLimit: new(big.Int).Mul(liquidity, big.NewInt(100)),
		}
	}
	genesis := &rheaBlock{height: 1, hash: randomHash(), ts: time.Now(), state: w.st.clone()}
	w.blocks = []*rheaBlock{genesis}
	w.byHash[genesis.hash] = genesis
	return w
}

// nearValue is get_near_value for a market token: 3 tokens per NEAR.
func (m *rheaMarket) nearValue() *big.Int {
	return new(big.Int).Mul(big.NewInt(3), tenPow(m.decimals))
}

// createFee is get_create_mca_fee for a market token: 0.3 tokens.
func (m *rheaMarket) createFee() *big.Int {
	return new(big.Int).Mul(big.NewInt(3), tenPow(m.decimals-1))
}

// initValue is the MCA's initial NEAR balance valued in the token, rounded
// up (the account manager's dapp registration fee).
func (m *rheaMarket) initValue() *big.Int {
	v := new(big.Int).Mul(rheaInitBalance, m.nearValue())
	v.Add(v, new(big.Int).Sub(yoctoPerNEAR, big.NewInt(1)))
	return v.Div(v, yoctoPerNEAR)
}

// gasFee is the relayer's GAS_FEE for the token: 0.001 tokens.
func (m *rheaMarket) gasFee() *big.Int {
	return tenPow(m.decimals - 3)
}

// randomHash returns a base58 32-byte hash, the form of NEAR block,
// transaction, and receipt IDs.
func randomHash() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base58.Encode(b[:])
}

func (w *rheaWorld) head() *rheaBlock {
	return w.blocks[len(w.blocks)-1]
}

func (w *rheaWorld) final() *rheaBlock {
	i := len(w.blocks) - 1 - w.faults.FinalLag
	if i < 0 {
		i = 0
	}
	return w.blocks[i]
}

// run produces blocks until ctx ends.
func (w *rheaWorld) run(done <-chan struct{}) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			w.produceBlock()
		}
	}
}

// produceBlock includes due transactions, executes due receipts, and
// snapshots the state.
func (w *rheaWorld) produceBlock() {
	w.mtx.Lock()
	prev := w.head()
	h := prev.height + 1
	ts := time.Now().Add(w.warp)
	if !ts.After(prev.ts) {
		ts = prev.ts.Add(time.Millisecond)
	}
	hash := randomHash()
	blk := &rheaBlock{height: h, hash: hash, prev: prev.hash, ts: ts}

	now := time.Now()
	var waiting []*rheaTx
	for _, tx := range w.pendingTx {
		if now.Before(tx.includeAt) {
			waiting = append(waiting, tx)
			continue
		}
		tx.included, tx.inclBlock = h, hash
		for _, r := range tx.receipts {
			if r.first {
				r.at = h + 1
				if r.local {
					r.at = h
				}
			}
		}
		if tx.onInclude != nil {
			tx.onInclude(tx)
		}
	}
	w.pendingTx = waiting

	// Receipts scheduled while executing this block run in later blocks.
	due := make([]*rheaReceipt, 0, len(w.queue))
	var rest []*rheaReceipt
	for _, r := range w.queue {
		ready := r.tx.included != 0 && r.at != 0 && r.at <= h
		if ready && w.faults.HoldReceipts && !r.first {
			ready = false
		}
		if ready {
			due = append(due, r)
		} else {
			rest = append(rest, r)
		}
	}
	w.queue = rest
	w.producing = blk
	for _, r := range due {
		r.done, r.height, r.blockHash = true, h, hash
		if r.status == nil {
			r.status = okValue(nil)
		}
		r.run(r)
	}
	w.producing = nil
	blk.state = w.st.clone()
	w.blocks = append(w.blocks, blk)
	w.byHash[hash] = blk
	if len(w.blocks) > rheaHistoryBlocks {
		old := w.blocks[0]
		delete(w.byHash, old.hash)
		w.blocks = w.blocks[1:]
	}
	pokes := w.pokes
	w.pokes = nil
	w.mtx.Unlock()
	for _, p := range pokes {
		if w.poke != nil {
			w.poke(p[0], p[1])
		}
	}
}

// newTx registers a transaction for inclusion in the next block at or
// after includeAt. Its first receipt is built by the caller with first
// set.
func (w *rheaWorld) newTx(signer, receiver string, actions []any, includeAt time.Time) *rheaTx {
	tx := &rheaTx{hash: randomHash(), signer: signer, receiver: receiver, actions: actions, includeAt: includeAt}
	w.txs[tx.hash] = tx
	w.pendingTx = append(w.pendingTx, tx)
	return tx
}

// firstReceipt adds a transaction's first receipt.
func (w *rheaWorld) firstReceipt(tx *rheaTx, run func(r *rheaReceipt)) *rheaReceipt {
	r := &rheaReceipt{
		id: randomHash(), tx: tx, predecessor: tx.signer, receiver: tx.receiver, actions: tx.actions,
		first: true, local: tx.signer == tx.receiver, run: run,
	}
	tx.first = r.id
	tx.receipts = append(tx.receipts, r)
	w.queue = append(w.queue, r)
	return r
}

// child creates a receipt of parent's transaction with a fresh ID, to run
// in the block after h (plus any configured delay). The caller lists the
// ID among the parent's children.
func (w *rheaWorld) child(parent *rheaReceipt, predecessor, receiver string, actions []any, run func(r *rheaReceipt)) *rheaReceipt {
	return w.childWithID(randomHash(), parent.tx, parent.height, predecessor, receiver, actions, run)
}

// childWithID schedules a receipt whose ID was listed earlier (a callback
// created when its parent ran but executed only after other work).
func (w *rheaWorld) childWithID(id string, tx *rheaTx, after uint64, predecessor, receiver string, actions []any, run func(r *rheaReceipt)) *rheaReceipt {
	r := &rheaReceipt{
		id: id, tx: tx, predecessor: predecessor, receiver: receiver, actions: actions,
		at: after + 1 + uint64(w.faults.ReceiptDelayBlocks), run: run,
	}
	tx.receipts = append(tx.receipts, r)
	w.queue = append(w.queue, r)
	return r
}

// notifyDeposit records a credit to account for the 1Click side.
func (w *rheaWorld) notifyDeposit(account, txHash string) {
	w.pokes = append(w.pokes, [2]string{account, txHash})
}

// Receipt statuses and actions, in RPC form.

func okValue(v any) map[string]any {
	if v == nil {
		return map[string]any{"SuccessValue": ""}
	}
	b, _ := json.Marshal(v)
	return map[string]any{"SuccessValue": base64.StdEncoding.EncodeToString(b)}
}

func failure(msg string) map[string]any {
	return map[string]any{"Failure": map[string]any{"ActionError": map[string]any{
		"index": 0,
		"kind":  map[string]any{"FunctionCallError": map[string]any{"ExecutionError": "Smart contract panicked: " + msg}},
	}}}
}

func callAction(method string, args any, gas uint64, deposit string) map[string]any {
	var raw []byte
	switch a := args.(type) {
	case []byte:
		raw = a
	case string:
		raw = []byte(a)
	default:
		raw, _ = json.Marshal(args)
	}
	return map[string]any{"FunctionCall": map[string]any{
		"method_name": method, "args": base64.StdEncoding.EncodeToString(raw), "gas": gas, "deposit": deposit,
	}}
}

func transferAction(deposit string) map[string]any {
	return map[string]any{"Transfer": map[string]any{"deposit": deposit}}
}

// Event logs in the shapes mainnet contracts emit.

func eventLog(standard, event string, data map[string]any) string {
	b, _ := json.Marshal(map[string]any{"standard": standard, "version": "1.0.0", "event": event, "data": []any{data}})
	return "EVENT_JSON:" + string(b)
}

func nep141Log(from, to string, amount *big.Int) string {
	return eventLog("nep141", "ft_transfer", map[string]any{"old_owner_id": from, "new_owner_id": to, "amount": amount.String()})
}

func burrowLog(event, account, token string, amount *big.Int) string {
	return eventLog("burrow", event, map[string]any{"account_id": account, "token_id": token, "amount": amount.String()})
}

func dip4WithdrawLog(account, token, receiver string, amount *big.Int, msg string) string {
	d := map[string]any{"account_id": account, "token": token, "receiver_id": receiver, "amount": amount.String()}
	if msg != "" {
		d["msg"] = msg
	}
	return eventLog("dip4", "ft_withdraw", d)
}

func amFeeLog(event, token string, amount *big.Int) string {
	return eventLog("account_manager", event, map[string]any{"token_id": token, "amount": amount.String()})
}

// txStatus renders EXPERIMENTAL_tx_status for a transaction, or an RPC
// error.
func (w *rheaWorld) txStatus(hash, sender string) (map[string]any, *rpcErrorBody) {
	tx := w.txs[hash]
	if tx == nil || tx.included == 0 || tx.signer != sender {
		return nil, unknownTx(hash)
	}
	finalH := w.final().height
	allDone, maxH := true, tx.included
	firstDone := false
	var receipts, outcomes []any
	for _, r := range tx.receipts {
		if !r.done {
			allDone = false
			continue
		}
		if r.first {
			firstDone = true
		}
		if r.height > maxH {
			maxH = r.height
		}
		if !r.local {
			receipts = append(receipts, map[string]any{
				"receipt_id": r.id, "predecessor_id": r.predecessor, "receiver_id": r.receiver,
				"receipt": map[string]any{"Action": map[string]any{"actions": r.actions, "signer_id": tx.signer}},
			})
		}
		logs := r.logs
		if logs == nil {
			logs = []string{}
		}
		children := r.children
		if children == nil {
			children = []string{}
		}
		outcomes = append(outcomes, map[string]any{
			"id": r.id, "block_hash": r.blockHash,
			"outcome": map[string]any{"executor_id": r.receiver, "logs": logs, "receipt_ids": children, "status": r.status},
		})
	}
	status := "INCLUDED"
	switch {
	case allDone && maxH <= finalH:
		status = "FINAL"
	case firstDone:
		status = "EXECUTED_OPTIMISTIC"
	}
	if receipts == nil {
		receipts = []any{}
	}
	if outcomes == nil {
		outcomes = []any{}
	}
	return map[string]any{
		"final_execution_status": status,
		"transaction": map[string]any{
			"hash": tx.hash, "signer_id": tx.signer, "receiver_id": tx.receiver, "actions": tx.actions,
		},
		"transaction_outcome": map[string]any{
			"id": tx.hash, "block_hash": tx.inclBlock,
			"outcome": map[string]any{
				"executor_id": tx.signer, "logs": []string{}, "receipt_ids": []string{tx.first},
				"status": map[string]any{"SuccessReceiptId": tx.first},
			},
		},
		"receipts":         receipts,
		"receipts_outcome": outcomes,
	}, nil
}

// blockAt resolves a block reference: a hash, or final for none.
func (w *rheaWorld) blockAt(hash string) (*rheaBlock, *rpcErrorBody) {
	if hash == "" {
		return w.final(), nil
	}
	if b := w.byHash[hash]; b != nil {
		return b, nil
	}
	return nil, &rpcErrorBody{Name: "HANDLER_ERROR", Cause: &rpcCause{Name: "UNKNOWN_BLOCK"}, Code: -32000,
		Message: "Server error", Data: fmt.Sprintf("DB Not Found Error: BLOCK: %s", hash)}
}

// state returns a debug view of the head state.
func (w *rheaWorld) debugState() map[string]any {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	st := w.st
	mcas := make([]string, 0, len(st.wallets))
	for m := range st.wallets {
		mcas = append(mcas, m)
	}
	sort.Strings(mcas)
	accounts := make([]any, 0, len(mcas))
	for _, m := range mcas {
		supplied := map[string]string{}
		for t, v := range st.supplied[m] {
			supplied[t] = v.String()
		}
		plain := map[string]string{}
		for t := range w.markets {
			if b := st.balance(t, m); b.Sign() > 0 {
				plain[t] = b.String()
			}
		}
		accounts = append(accounts, map[string]any{
			"mca": m, "wallets": st.wallets[m], "nonce": st.nonces[m], "near": cloneBig(st.nearBal[m]).String(),
			"supplied": supplied, "plain": plain,
		})
	}
	h, f := w.head(), w.final()
	return map[string]any{
		"head":  map[string]any{"height": h.height, "hash": h.hash, "timestamp": h.ts.UTC().Format(timestampFormat)},
		"final": map[string]any{"height": f.height, "hash": f.hash, "timestamp": f.ts.UTC().Format(timestampFormat)},
		"warp":  w.warp.String(), "faults": w.faults, "accounts": accounts,
		"queuedReceipts": len(w.queue), "pendingTxs": len(w.pendingTx),
	}
}
