// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// RHEA (Burrow) lending through RHEA's cross-chain multichain accounts
// (MCAs). The protocol layer is wallet/rhea; this file holds the lending
// engine's read path: identity derivation at login, account discovery,
// market and account views, and the admission gate that decides whether a
// relayer business may be signed.
//
// Conclusions that something did not happen are drawn only from finalized
// chain evidence read at a named final block: an RPC failure is never
// absence, and the admission gate stays closed whenever its evidence is
// missing or incomplete. NEAR RPC responses are unauthenticated, so this
// evidence trusts the answering endpoint.

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/wallet/asset/near"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/rhea"
)

const (
	// rheaRefreshInterval is the cadence of background discovery and nonce
	// verification while logged in.
	rheaRefreshInterval = 5 * time.Minute
	// rheaPokeInterval throttles refreshes requested by views.
	rheaPokeInterval = 30 * time.Second
	// rheaHistoryPageSize is the relayer history page size.
	rheaHistoryPageSize = 50
	// rheaMaxHistoryRecords bounds the relayer records one nonce walk
	// receives, of every type, and rheaMaxHistoryPages the pages it
	// requests. A history longer than this leaves recovery admission
	// unresolved rather than walking without limit.
	rheaMaxHistoryRecords = 2000
	rheaMaxHistoryPages   = rheaMaxHistoryRecords / rheaHistoryPageSize
	// rheaWindowMargin pads the signature window. Earlier signatures are
	// considered admissible until a final block is this much past the
	// first-seen time plus rhea.MaxDeadlineWindow.
	rheaWindowMargin = 2 * time.Minute
	// rheaEVMSeedAssetID selects the EVM asset seed, the seed behind every
	// EVM wallet and the lending identity.
	rheaEVMSeedAssetID = 60
	// rheaLookupWriteInterval limits how often an account record's
	// LastLookup is re-written.
	rheaLookupWriteInterval = 10 * time.Minute
)

// Discovery statuses.
const (
	// RheaFound: exactly one MCA is bound to the identities.
	RheaFound = "found"
	// RheaNone: every account manager returned null for every identity.
	RheaNone = "none"
	// RheaUnavailable: at least one lookup failed, so absence (or a second
	// account) cannot be ruled out.
	RheaUnavailable = "unavailable"
	// RheaMultiple: more than one MCA is bound to the identities. Signing
	// is blocked.
	RheaMultiple = "multiple"
)

// RheaTestConfig points the lending engine at a non-mainnet deployment,
// such as a simnet mock. It is ignored on mainnet, where the pinned
// deployment and default endpoints are always used. Without it the engine
// is disabled on every other network.
type RheaTestConfig struct {
	Deployment        *rhea.Deployment
	RPCEndpoints      []string
	ArchivalEndpoints []string
	// RelayerURL overrides Deployment.RelayerAPI.
	RelayerURL string
}

// rheaChain is the NEAR access the engine needs.
type rheaChain interface {
	rhea.Viewer
	// CallFunctionAll runs a view on every configured endpoint at a named
	// block; any endpoint failing to answer fails the call.
	CallFunctionAll(ctx context.Context, contractID, method string, args []byte, blockHash string) ([]*rhea.ViewResult, error)
	FinalBlock(ctx context.Context) (*near.BlockHeader, error)
	ViewAccount(ctx context.Context, accountID, blockHash string) (*near.AccountView, error)
	TxStatus(ctx context.Context, txHash, senderID, waitUntil string) (json.RawMessage, error)
}

// nearRheaChain adapts a near.ViewClient to rheaChain.
type nearRheaChain struct {
	*near.ViewClient
}

// CallFunction satisfies rhea.Viewer.
func (c nearRheaChain) CallFunction(ctx context.Context, contractID, method string, args []byte, blockHash string) (*rhea.ViewResult, error) {
	res, err := c.ViewClient.CallFunction(ctx, contractID, method, args, blockHash)
	if err != nil {
		return nil, err
	}
	return &rhea.ViewResult{Result: res.Result, BlockHeight: res.BlockHeight, BlockHash: res.BlockHash}, nil
}

// CallFunctionAll satisfies rheaChain.
func (c nearRheaChain) CallFunctionAll(ctx context.Context, contractID, method string, args []byte, blockHash string) ([]*rhea.ViewResult, error) {
	res, err := c.ViewClient.CallFunctionAll(ctx, contractID, method, args, blockHash)
	if err != nil {
		return nil, err
	}
	out := make([]*rhea.ViewResult, len(res))
	for i, r := range res {
		out[i] = &rhea.ViewResult{Result: r.Result, BlockHeight: r.BlockHeight, BlockHash: r.BlockHash}
	}
	return out, nil
}

// rheaHistory is the relayer history source.
type rheaHistory interface {
	History(ctx context.Context, mcaID string, page, size int) (*rhea.HistoryPage, error)
}

// rheaIdentity is a public lending identity of one derivation version.
type rheaIdentity struct {
	Version  rhea.DerivationVersion
	Identity rhea.Identity
}

// setRheaIdentities derives the public lending identities of every
// supported derivation version from the decrypted app seed. Called at
// login; a seed restore reaches it through the first login.
func (c *Core) setRheaIdentities(appSeed []byte) {
	evmSeed, _ := AssetSeedAndPass(rheaEVMSeedAssetID, appSeed)
	defer encode.ClearBytes(evmSeed)
	ids := make([]rheaIdentity, 0, len(rhea.DerivationVersions))
	for _, v := range rhea.DerivationVersions {
		id, err := rhea.DeriveIdentity(v, evmSeed)
		if err != nil {
			c.log.Errorf("Error deriving lending identity version %d: %v", v, err)
			continue
		}
		ids = append(ids, rheaIdentity{Version: v, Identity: id})
	}
	c.rheaIDs.Store(&ids)
}

// clearRheaIdentities forgets the lending identities at logout.
func (c *Core) clearRheaIdentities() {
	c.rheaIDs.Store(nil)
}

// rheaIdentities returns the lending identities, or nil when logged out.
func (c *Core) rheaIdentities() []rheaIdentity {
	if ids := c.rheaIDs.Load(); ids != nil {
		return *ids
	}
	return nil
}

// rheaEngine runs RHEA lending.
type rheaEngine struct {
	// halt stops the background loop startRheaEngine began and waits for
	// it (see stop).
	halt       func()
	log        util.Logger
	dep        *rhea.Deployment
	chain      rheaChain
	reader     *rhea.Reader
	history    rheaHistory
	db         db.DB
	identities func() []rheaIdentity
	now        func() time.Time

	// acctMtx serializes read-modify-write of account records.
	acctMtx sync.Mutex

	walkMtx   sync.Mutex
	lastWalks map[string]*RheaWalk // by MCA

	trigger   chan struct{}
	pokeMtx   sync.Mutex
	lastPoked time.Time

	// viewed is set once the account view is requested this session. See
	// engaged.
	viewed atomic.Bool

	// Supply and operation polling (rhea-supply.go). c is nil in engines
	// built for read-path tests.
	c         *Core
	submitter rheaSubmitter // nil when the relayer is unavailable
	oneClick  func() (*intentsEngine, error)
	// tickerRates returns Caribou's fresh USD rates by normalized ticker,
	// the wallet's ticker rates; nil when no rates are available.
	tickerRates func() map[string]float64
	notify      func(Notification)
	opMtx       sync.Mutex // serializes read-modify-write of operation records
	opTrigger   chan struct{}

	attrMtx   sync.Mutex
	attrNext  map[string]time.Time     // by operation ID: next attribution attempt
	attrDelay map[string]time.Duration // by operation ID: current backoff
}

func newRheaEngine(log util.Logger, dep *rhea.Deployment, chain rheaChain, history rheaHistory, store db.DB, identities func() []rheaIdentity) *rheaEngine {
	return &rheaEngine{
		log:        log,
		dep:        dep,
		chain:      chain,
		reader:     rhea.NewReader(chain, dep),
		history:    history,
		db:         store,
		identities: identities,
		now:        time.Now,
		lastWalks:  make(map[string]*RheaWalk),
		trigger:    make(chan struct{}, 1),
		opTrigger:  make(chan struct{}, 1),
		attrNext:   make(map[string]time.Time),
		attrDelay:  make(map[string]time.Duration),
		oneClick:   func() (*intentsEngine, error) { return nil, errors.New("cross-chain swap engine not available") },
		notify:     func(Notification) {},
	}
}

// startRheaEngine launches the lending engine. On mainnet it uses the
// pinned deployment; elsewhere only a test configuration enables it.
// Failure disables lending for the session but is not fatal to Core.
func (c *Core) startRheaEngine(ctx context.Context) {
	logger := c.log.SubLogger("RHEA")
	dep, rpcEps, archEps, relayerURL := rhea.Mainnet, []string(nil), []string(nil), rhea.Mainnet.RelayerAPI
	tc := c.cfg.RheaTest
	switch {
	case c.net == util.Mainnet:
		if tc != nil {
			// An injected deployment or endpoint on mainnet would let a
			// configuration file redirect what the engine treats as
			// finalized evidence.
			logger.Warnf("Ignoring RHEA test configuration on mainnet")
		}
	case tc == nil || tc.Deployment == nil || len(tc.RPCEndpoints) == 0:
		logger.Infof("RHEA lending disabled: no deployment on %s", c.net)
		return
	default:
		dep, rpcEps, archEps = tc.Deployment, tc.RPCEndpoints, tc.ArchivalEndpoints
		relayerURL = dep.RelayerAPI
		if tc.RelayerURL != "" {
			relayerURL = tc.RelayerURL
		}
	}
	view, err := near.NewViewClient(&near.ViewClientConfig{
		Net:               c.net,
		Endpoints:         rpcEps,
		ArchivalEndpoints: archEps,
		TorProxy:          c.cfg.TorProxy,
	}, logger.SubLogger("NEAR"))
	if err != nil {
		logger.Errorf("Error creating NEAR view client: %v", err)
		return
	}
	relayer := rhea.NewRelayerClient(relayerURL, c.cfg.TorProxy, logger.SubLogger("RELAYER"))
	eng := newRheaEngine(logger, dep, nearRheaChain{view}, relayer, c.db, c.rheaIdentities)
	eng.c, eng.oneClick, eng.notify, eng.submitter = c, c.intentsEngine, c.notify, relayer
	eng.tickerRates = func() map[string]float64 { return c.tickerConversions(c.fiatConversions()) }
	c.rheaEng.Store(eng)
	logger.Infof("RHEA lending engine started: account manager %s, Burrow %s", dep.AccountManager, dep.Burrow)

	engCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	eng.halt = func() {
		cancel()
		<-done
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(done)
		eng.run(engCtx)
	}()
}

// stop ends a started engine's background loop and waits for it to exit.
// An engine that was never started has nothing to stop.
func (e *rheaEngine) stop() {
	if e.halt != nil {
		e.halt()
	}
}

// rheaEngine returns the running engine, or an error if lending is
// unavailable this session.
func (c *Core) rheaEngine() (*rheaEngine, error) {
	if eng, ok := c.rheaEng.Load().(*rheaEngine); ok {
		return eng, nil
	}
	return nil, errors.New("RHEA lending is not available")
}

// run refreshes discovery and nonce verification on a timer and when
// poked, and polls active operations. The operation ticker is parked while
// no operation is active; pokeOps resumes it.
func (e *rheaEngine) run(ctx context.Context) {
	ticker := time.NewTicker(rheaRefreshInterval)
	defer ticker.Stop()
	opTicker := time.NewTicker(rheaOpPollInterval)
	defer opTicker.Stop()
	if active, loaded := e.pollOps(ctx); loaded && active == 0 {
		opTicker.Stop()
	}
	for {
		select {
		case <-ticker.C:
			e.refresh(ctx)
		case <-e.trigger:
			e.refresh(ctx)
		case <-opTicker.C:
			if active, loaded := e.pollOps(ctx); loaded && active == 0 {
				opTicker.Stop()
			}
		case <-e.opTrigger:
			opTicker.Reset(rheaOpPollInterval)
			e.pollOps(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// pokeOps nudges the operation poller without waiting for the next tick.
func (e *rheaEngine) pokeOps() {
	select {
	case e.opTrigger <- struct{}{}:
	default:
	}
}

// poke requests a refresh, at most once per rheaPokeInterval.
func (e *rheaEngine) poke() {
	e.pokeMtx.Lock()
	if e.now().Sub(e.lastPoked) < rheaPokeInterval {
		e.pokeMtx.Unlock()
		return
	}
	e.lastPoked = e.now()
	e.pokeMtx.Unlock()
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

// engaged reports whether background lending work is warranted: the
// account view was opened this session, this installation has recorded an
// account for one of the identities, or an operation is active. Discovery
// sends the identity (the EVM address) to NEAR RPC providers, so a wallet
// that never uses lending makes no lending requests.
func (e *rheaEngine) engaged(ids []rheaIdentity) bool {
	if e.viewed.Load() {
		return true
	}
	for _, id := range ids {
		_, err := e.db.RheaAccount(e.dep.AccountManager, uint32(id.Version), string(id.Identity))
		if err == nil {
			return true
		}
		if !errors.Is(err, db.ErrRheaAccountNotFound) {
			e.log.Errorf("Error loading the lending account record: %v", err)
			return false
		}
	}
	active, err := e.db.ActiveRheaOps()
	if err != nil {
		e.log.Errorf("Error loading lending operations: %v", err)
		return false
	}
	return len(active) > 0
}

// refresh discovers the account and advances the nonce checkpoint.
func (e *rheaEngine) refresh(ctx context.Context) {
	ids := e.identities()
	if len(ids) == 0 || !e.engaged(ids) {
		return
	}
	block, err := e.chain.FinalBlock(ctx)
	if err != nil {
		e.log.Meter("final-block", 10*time.Minute).Warnf("Error reading the final block: %v", err)
		return
	}
	disc := e.discover(ctx, ids, block.Hash)
	if disc.Status != RheaFound {
		return
	}
	acct, err := e.recordAccount(disc.found, block)
	if err != nil {
		e.log.Errorf("Error recording the lending account: %v", err)
		return
	}
	nonce, _, err := e.reader.Nonce(ctx, acct.MCA, block.Hash)
	if err != nil {
		e.log.Meter("nonce", 10*time.Minute).Warnf("Error reading the MCA nonce: %v", err)
		return
	}
	if acct.VerifiedBelow >= nonce {
		return
	}
	err = e.walkNonces(ctx, acct, nonce)
	walk := &RheaWalk{At: e.now()}
	if err != nil {
		walk.Error = err.Error()
		e.log.Infof("Recovery admission for %s unresolved: %v", acct.MCA, err)
	}
	e.walkMtx.Lock()
	e.lastWalks[acct.MCA] = walk
	e.walkMtx.Unlock()
}

// RheaBinding is the result of looking up one identity on one account
// manager.
type RheaBinding struct {
	Version        uint32 `json:"version"`
	Identity       string `json:"identity"`
	AccountManager string `json:"accountManager"`
	// Status is RheaFound, RheaNone, or RheaUnavailable.
	Status string `json:"status"`
	MCA    string `json:"mca,omitempty"`
	Error  string `json:"error,omitempty"`
}

type rheaDiscovery struct {
	Status   string
	Bindings []*RheaBinding
	found    *RheaBinding // set when Status is RheaFound
}

// discover looks every identity up on every known account manager at a
// final block.
func (e *rheaEngine) discover(ctx context.Context, ids []rheaIdentity, blockHash string) *rheaDiscovery {
	d := &rheaDiscovery{}
	var found []*RheaBinding
	unavailable := false
	for _, am := range []string{e.dep.AccountManager} {
		for _, id := range ids {
			b := &RheaBinding{Version: uint32(id.Version), Identity: string(id.Identity), AccountManager: am}
			mca, _, err := e.reader.MCAByWallet(ctx, id.Identity, blockHash)
			switch {
			case err != nil:
				b.Status, b.Error = RheaUnavailable, err.Error()
				unavailable = true
			case mca == "":
				b.Status = RheaNone
			default:
				b.Status, b.MCA = RheaFound, mca
				found = append(found, b)
			}
			d.Bindings = append(d.Bindings, b)
		}
	}
	switch {
	case len(found) > 1:
		d.Status = RheaMultiple
	case unavailable:
		d.Status = RheaUnavailable
	case len(found) == 1:
		d.Status, d.found = RheaFound, found[0]
	default:
		d.Status = RheaNone
	}
	return d
}

// recordAccount returns the account record for a found binding, creating
// it with the given block as first-seen when this installation has not seen
// the binding before. A stored record for a different MCA is replaced: its
// first-seen time and checkpoint belong to the other account.
func (e *rheaEngine) recordAccount(b *RheaBinding, block *near.BlockHeader) (*db.RheaAccount, error) {
	e.acctMtx.Lock()
	defer e.acctMtx.Unlock()
	a, err := e.db.RheaAccount(b.AccountManager, b.Version, b.Identity)
	switch {
	case errors.Is(err, db.ErrRheaAccountNotFound):
		a = nil
	case err != nil:
		return nil, err
	}
	if a != nil && a.MCA != b.MCA {
		e.log.Warnf("Identity %s is now bound to %s, not %s; starting a new account record", b.Identity, b.MCA, a.MCA)
		a = nil
	}
	now := e.now()
	if a != nil && now.Sub(a.LastLookup) < rheaLookupWriteInterval {
		return a, nil
	}
	if a == nil {
		a = &db.RheaAccount{
			AccountManager:    b.AccountManager,
			Identity:          b.Identity,
			DerivationVersion: b.Version,
			MCA:               b.MCA,
			FirstSeenHeight:   block.Height,
			FirstSeenHash:     block.Hash,
			FirstSeenTime:     block.Timestamp,
		}
		e.log.Infof("Lending account %s first seen at block %d", b.MCA, block.Height)
	}
	a.LastLookup = now
	if err := e.db.StoreRheaAccount(a); err != nil {
		return nil, err
	}
	return a, nil
}

// rheaExec is an executed business found for a nonce.
type rheaExec struct {
	txHash   string
	complete error // nil when the receipt tree is final and fully reported
}

// walkNonces verifies, from the relayer history, that every business the
// MCA admitted with a nonce in [acct.VerifiedBelow, nonce) is complete: an
// exec on the MCA carrying that nonce, whose transaction is final with
// every receipt reported and known. The checkpoint advances over the
// contiguous verified prefix and is persisted. It returns an error naming
// the first nonce it could not verify; recovery admission then stays
// unresolved. A nonce consumed outside the relayer leaves a gap and is
// reported the same way, and two executed businesses claiming one nonce
// (which the one-nonce-per-exec assumption rules out) stop the walk.
func (e *rheaEngine) walkNonces(ctx context.Context, acct *db.RheaAccount, nonce uint64) error {
	from := acct.VerifiedBelow
	if from >= nonce {
		return nil
	}
	found := make(map[uint64]*rheaExec)
	var fetchErrs []string
	allFound := func() bool {
		for n := from; n < nonce; n++ {
			if found[n] == nil {
				return false
			}
		}
		return true
	}
	// The walk stops once every needed nonce is found and an exec below the
	// checkpoint has been seen: history is newest first, so a second exec
	// claiming a needed nonce would appear before older ones. A walk from
	// zero reads the whole history. The relayer's pagination is untrusted:
	// every received record counts toward the bound, pages are capped
	// independently of the reported page count, and a page with no new
	// records ends the reading.
	seenBelow := false
	done := func() bool { return seenBelow && allFound() }
	var stopped error // why reading ended before the history did
	received := 0
	seen := make(map[string]bool)
read:
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if page > rheaMaxHistoryPages {
			stopped = fmt.Errorf("history exceeds %d pages", rheaMaxHistoryPages)
			break
		}
		hp, err := e.history.History(ctx, acct.MCA, page, rheaHistoryPageSize)
		if err != nil {
			return fmt.Errorf("relayer history unavailable: %w", err)
		}
		if len(hp.Records) > rheaHistoryPageSize {
			stopped = fmt.Errorf("history page %d has %d records, more than the %d requested", page, len(hp.Records), rheaHistoryPageSize)
			break
		}
		fresh := 0
		for _, rec := range hp.Records {
			if err := ctx.Err(); err != nil {
				return err
			}
			if received++; received > rheaMaxHistoryRecords {
				stopped = fmt.Errorf("history exceeds %d records", rheaMaxHistoryRecords)
				break read
			}
			key := rheaHistoryKey(rec)
			if seen[key] {
				continue
			}
			seen[key] = true
			fresh++
			if rec.Type != rhea.HistoryTypeRelayer {
				continue
			}
			x, err := e.execsInTx(ctx, rec.RequestHash, acct.MCA)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fetchErrs = append(fetchErrs, fmt.Sprintf("%s: %v", rec.RequestHash, err))
				continue
			}
			for _, n := range x.nonces {
				if n < from {
					seenBelow = true
					continue
				}
				if n >= nonce {
					continue
				}
				if prev := found[n]; prev != nil && prev.txHash != rec.RequestHash {
					return fmt.Errorf("two executed businesses carry nonce %d (%s, %s)", n, prev.txHash, rec.RequestHash)
				}
				found[n] = &rheaExec{txHash: rec.RequestHash, complete: x.incomplete}
			}
			if done() {
				break read
			}
		}
		if len(hp.Records) > 0 && fresh == 0 {
			stopped = fmt.Errorf("history page %d repeats earlier records", page)
			break
		}
		if len(hp.Records) == 0 || page >= hp.TotalPage {
			break
		}
	}
	verified := from
	for verified < nonce && found[verified] != nil && found[verified].complete == nil {
		verified++
	}
	if verified > from {
		if err := e.advanceCheckpoint(acct, verified); err != nil {
			return fmt.Errorf("error storing the nonce checkpoint: %w", err)
		}
	}
	if verified == nonce {
		return nil
	}
	var err error
	if x := found[verified]; x != nil {
		err = fmt.Errorf("the exec for nonce %d (%s) is not complete: %v", verified, x.txHash, x.complete)
	} else {
		err = fmt.Errorf("no exec found for nonce %d", verified)
		if len(fetchErrs) > 0 {
			err = fmt.Errorf("%w; %d history transactions could not be checked (first: %s)", err, len(fetchErrs), fetchErrs[0])
		}
	}
	if stopped != nil {
		err = fmt.Errorf("%w; relayer history reading stopped: %v", err, stopped)
	}
	return err
}

// rheaHistoryKey identifies a history record for duplicate detection.
func rheaHistoryKey(rec *rhea.HistoryRecord) string {
	if rec.ID != 0 {
		return "id:" + strconv.FormatInt(rec.ID, 10)
	}
	return strconv.Itoa(rec.Type) + "/" + rec.RequestHash + "/" + rec.CreatedAt
}

// rheaTxExecs is what an exec transaction shows about an MCA's nonces.
type rheaTxExecs struct {
	// nonces are the nonces of the businesses executed on the MCA. An exec
	// whose own receipt failed was not admitted and consumed no nonce.
	nonces []uint64
	// incomplete is nil when the receipt tree is final and fully reported.
	incomplete error
}

// execsInTx reads an exec transaction from the chain.
func (e *rheaEngine) execsInTx(ctx context.Context, txHash, mca string) (*rheaTxExecs, error) {
	raw, err := e.chain.TxStatus(ctx, txHash, e.dep.Relayer, near.TxWaitNone)
	if err != nil {
		return nil, err
	}
	tree, err := rhea.DecodeTxTree(raw)
	if err != nil {
		return nil, err
	}
	if tree.Hash != txHash {
		return nil, fmt.Errorf("status for %s describes %s", txHash, tree.Hash)
	}
	calls, err := tree.ExecCalls(e.dep)
	if err != nil {
		return nil, err
	}
	x := &rheaTxExecs{}
	for _, c := range calls {
		if c.MCA != mca || c.Outcome == nil || !c.Outcome.Status.OK() {
			continue
		}
		n, err := c.Args.Business.NonceValue()
		if err != nil {
			return nil, err
		}
		x.nonces = append(x.nonces, n)
	}
	if x.incomplete = tree.Complete(); x.incomplete == nil {
		x.incomplete = tree.EvidenceAvailable()
	}
	return x, nil
}

// advanceCheckpoint raises the stored nonce checkpoint. It never lowers it
// and never applies to a record that now names another MCA.
func (e *rheaEngine) advanceCheckpoint(acct *db.RheaAccount, verified uint64) error {
	e.acctMtx.Lock()
	defer e.acctMtx.Unlock()
	a, err := e.db.RheaAccount(acct.AccountManager, acct.DerivationVersion, acct.Identity)
	if err != nil {
		return err
	}
	if a.MCA != acct.MCA {
		return fmt.Errorf("account record now names %s", a.MCA)
	}
	if verified <= a.VerifiedBelow {
		return nil
	}
	a.VerifiedBelow = verified
	if err := e.db.StoreRheaAccount(a); err != nil {
		return err
	}
	acct.VerifiedBelow = verified
	e.log.Infof("Verified %s businesses below nonce %d", a.MCA, verified)
	return nil
}

// RheaWalk is the outcome of the last nonce verification.
type RheaWalk struct {
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// RheaAdmission is the admission gate for relayer businesses (withdraw and
// sweep). It is open only when no earlier signature can still be newly
// admitted and no admitted business can still move funds: no local
// business is pending, the final chain time is past the signature window
// measured from first discovery, the local clock agrees with chain time,
// and every nonce the MCA has consumed is verified complete. The window
// covers only businesses signed under Caribou's validator; a signature made
// with the same key elsewhere may carry a longer deadline and is visible
// only after it executes, through the nonce chain.
type RheaAdmission struct {
	Open bool `json:"open"`
	// RecoveryUnresolved is set when the MCA's consumed nonces are not all
	// verified complete.
	RecoveryUnresolved bool     `json:"recoveryUnresolved"`
	Reasons            []string `json:"reasons"`
	Nonce              uint64   `json:"nonce"`
	VerifiedBelow      uint64   `json:"verifiedBelow"`
	// WindowOpensAt is the chain time after which no earlier Caribou
	// signature can be newly admitted.
	WindowOpensAt *time.Time `json:"windowOpensAt,omitempty"`
	LastWalk      *RheaWalk  `json:"lastWalk,omitempty"`
}

type rheaAdmissionInput struct {
	discovery string
	mca       string
	account   *db.RheaAccount // nil when no record could be made
	nonce     uint64
	blockTime time.Time
	now       time.Time
	activeOps []*db.RheaOp
}

// rheaAdmission evaluates the admission gate. Every condition it cannot
// establish keeps the gate closed.
func rheaAdmission(in *rheaAdmissionInput) *RheaAdmission {
	adm := &RheaAdmission{Nonce: in.nonce, Reasons: []string{}}
	closeFor := func(format string, args ...any) {
		adm.Reasons = append(adm.Reasons, fmt.Sprintf(format, args...))
	}
	if in.discovery != RheaFound {
		closeFor("account discovery is %s", in.discovery)
		return adm
	}
	for _, op := range in.activeOps {
		if op.MCA == in.mca && db.RheaPendingBusinessStates[op.State] {
			closeFor("operation %s may still be admitted (%s)", op.ID, op.State)
		}
	}
	if in.account == nil {
		closeFor("no first-seen record for %s", in.mca)
	} else {
		adm.VerifiedBelow = in.account.VerifiedBelow
		opens := in.account.FirstSeenTime.Add(rhea.MaxDeadlineWindow + rheaWindowMargin)
		adm.WindowOpensAt = &opens
		if !in.blockTime.After(opens) {
			closeFor("earlier signatures may still be admitted until %s (chain time)", opens.UTC().Format(time.RFC3339))
		}
		switch {
		case in.account.VerifiedBelow < in.nonce:
			adm.RecoveryUnresolved = true
			closeFor("recovery admission unresolved: businesses with nonces %d to %d are not verified complete",
				in.account.VerifiedBelow, in.nonce-1)
		case in.account.VerifiedBelow > in.nonce:
			adm.RecoveryUnresolved = true
			closeFor("nonce checkpoint %d is above the account's nonce %d", in.account.VerifiedBelow, in.nonce)
		}
	}
	skew := in.now.Sub(in.blockTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > rhea.MaxClockSkew {
		closeFor("local clock differs from chain time by %s", skew.Round(time.Second))
	}
	adm.Open = len(adm.Reasons) == 0
	return adm
}

// RheaBlock names a final block.
type RheaBlock struct {
	Height uint64    `json:"height"`
	Hash   string    `json:"hash"`
	Time   time.Time `json:"time"`
}

func rheaBlock(h *near.BlockHeader) RheaBlock {
	return RheaBlock{Height: h.Height, Hash: h.Hash, Time: h.Timestamp}
}

// RheaMarket is an allowlisted lending market. Amounts are token base
// units (internal amounts rounded down).
type RheaMarket struct {
	Symbol          string `json:"symbol"`
	TokenID         string `json:"tokenID"`
	OneClickAssetID string `json:"oneClickAssetID"`
	Decimals        uint8  `json:"decimals"`
	ExtraDecimals   uint8  `json:"extraDecimals"`
	// SupplyAPR is the base supply APR as a decimal fraction, excluding
	// farm rewards.
	SupplyAPR string `json:"supplyAPR"`
	Supplied  string `json:"supplied"`
	Borrowed  string `json:"borrowed"`
	// AvailableLiquidity is an estimate of what Burrow can pay out now.
	AvailableLiquidity string `json:"availableLiquidity"`
	// Utilization is borrowed / supplied, for display.
	Utilization float64 `json:"utilization"`
	CanDeposit  bool    `json:"canDeposit"`
	CanWithdraw bool    `json:"canWithdraw"`
	// SupplyHeadroom is how much more can be supplied, or empty when the
	// market has no supply limit.
	SupplyHeadroom string `json:"supplyHeadroom,omitempty"`
	// SupplyUnavailable is set, with the reason, while no current USD rate
	// is known for the market token. No supply to the market is planned
	// until one is; withdrawals and sweeps are unaffected.
	SupplyUnavailable string `json:"supplyUnavailable,omitempty"`
}

// RheaMarketsView is the market list read at one final block.
type RheaMarketsView struct {
	Block   RheaBlock     `json:"block"`
	Markets []*RheaMarket `json:"markets"`
}

func internalToToken(v *big.Int, extra uint8) string {
	return rhea.FromInternal(v, extra).String()
}

// RheaMarkets reads the allowlisted markets at the latest final block.
func (c *Core) RheaMarkets(ctx context.Context) (*RheaMarketsView, error) {
	e, err := c.rheaEngine()
	if err != nil {
		return nil, err
	}
	return e.markets(ctx)
}

func (e *rheaEngine) markets(ctx context.Context) (*RheaMarketsView, error) {
	block, err := e.chain.FinalBlock(ctx)
	if err != nil {
		return nil, fmt.Errorf("error reading the final block: %w", err)
	}
	view := &RheaMarketsView{Block: rheaBlock(block)}
	for _, m := range e.dep.Markets {
		a, _, err := e.reader.Asset(ctx, m.TokenID, block.Hash)
		if err != nil {
			return nil, err
		}
		md, _, err := e.reader.FTMetadata(ctx, m.TokenID, block.Hash)
		if err != nil {
			return nil, err
		}
		x := a.Config.ExtraDecimals
		mk := &RheaMarket{
			Symbol:             m.Symbol,
			TokenID:            m.TokenID,
			OneClickAssetID:    m.OneClickAssetID,
			Decimals:           md.Decimals,
			ExtraDecimals:      x,
			SupplyAPR:          a.SupplyAPR,
			Supplied:           internalToToken(a.Supplied.Balance, x),
			Borrowed:           internalToToken(a.Borrowed.Balance, x),
			AvailableLiquidity: internalToToken(a.AvailableLiquidity(), x),
			CanDeposit:         a.Config.CanDeposit,
			CanWithdraw:        a.Config.CanWithdraw,
		}
		if a.Supplied.Balance.Sign() > 0 {
			u, _ := new(big.Rat).SetFrac(a.Borrowed.Balance, a.Supplied.Balance).Float64()
			mk.Utilization = u
		}
		if h := a.SupplyHeadroom(); h != nil {
			mk.SupplyHeadroom = internalToToken(h, x)
		}
		if _, err := e.marketUSDRate(m); err != nil {
			mk.SupplyUnavailable = err.Error()
		}
		view.Markets = append(view.Markets, mk)
	}
	return view, nil
}

// RheaWallet is a wallet bound to the MCA. Ours marks this installation's
// identities; any other wallet can also sign businesses for the MCA.
type RheaWallet struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Ours bool   `json:"ours"`
}

// RheaPosition is the account's standing in one allowlisted market, in
// token base units. Supplied is the plain supplied balance, the only part
// Caribou withdraws; collateral and debt are reported separately.
type RheaPosition struct {
	Symbol     string `json:"symbol"`
	TokenID    string `json:"tokenID"`
	Decimals   uint8  `json:"decimals"`
	Supplied   string `json:"supplied"`
	Collateral string `json:"collateral"`
	Borrowed   string `json:"borrowed"`
	// SuppliedInternal is the exact plain supplied balance in Burrow
	// internal units.
	SuppliedInternal string `json:"suppliedInternal"`
	// PlainBalance is the MCA's own token balance outside Burrow.
	PlainBalance string `json:"plainBalance"`
	CanWithdraw  bool   `json:"canWithdraw"`
	// SupplyUnavailable is set, with the reason, while no current USD rate
	// is known for the market token (see RheaMarket.SupplyUnavailable).
	SupplyUnavailable string `json:"supplyUnavailable,omitempty"`
}

// RheaHolding is a Burrow balance in a token outside the allowlist, in
// internal units. Such holdings come from activity outside Caribou.
type RheaHolding struct {
	TokenID  string `json:"tokenID"`
	Kind     string `json:"kind"` // supplied, collateral, borrowed
	Internal string `json:"internal"`
}

// RheaAccountView is the lending account read at one final block.
type RheaAccountView struct {
	Block      RheaBlock      `json:"block"`
	Identities []*RheaBinding `json:"identities"`
	// Discovery is RheaFound, RheaNone, RheaUnavailable, or RheaMultiple.
	Discovery string     `json:"discovery"`
	MCA       string     `json:"mca,omitempty"`
	FirstSeen *RheaBlock `json:"firstSeen,omitempty"`
	Nonce     *uint64    `json:"nonce,omitempty"`
	// CodeHash is the MCA's global contract code hash; LatestCodeHash is
	// the account manager's latest. They differ for an MCA running older
	// code.
	CodeHash       string          `json:"codeHash,omitempty"`
	LatestCodeHash string          `json:"latestCodeHash,omitempty"`
	CodeIsLatest   bool            `json:"codeIsLatest"`
	Wallets        []*RheaWallet   `json:"wallets,omitempty"`
	ForeignWallets bool            `json:"foreignWallets"`
	Positions      []*RheaPosition `json:"positions,omitempty"`
	Unlisted       []*RheaHolding  `json:"unlisted,omitempty"`
	HasDebt        bool            `json:"hasDebt"`
	BurrowAccount  bool            `json:"burrowAccount"`
	Ops            []*db.RheaOp    `json:"ops"`
	Admission      *RheaAdmission  `json:"admission"`
	// CreationFees is set when discovery finds no account: per
	// allowlisted market, the one-time fee a first supply there pays to
	// create the account and the creation minimum that supply must
	// guarantee, shown before an amount is chosen. A market whose fee could
	// not be read, or that has no current USD rate, carries Unavailable and
	// no minimum. When discovery is
	// unavailable, CreationFeesUnavailable is set instead; with an account
	// found, neither is set.
	CreationFees            []*RheaFeeLine `json:"creationFees,omitempty"`
	CreationFeesUnavailable bool           `json:"creationFeesUnavailable,omitempty"`
}

// RheaAccount reads the lending account of the logged-in identities at the
// latest final block, with the admission gate evaluated at that block.
func (c *Core) RheaAccount(ctx context.Context) (*RheaAccountView, error) {
	e, err := c.rheaEngine()
	if err != nil {
		return nil, err
	}
	return e.account(ctx)
}

func (e *rheaEngine) account(ctx context.Context) (*RheaAccountView, error) {
	ids := e.identities()
	if len(ids) == 0 {
		return nil, errors.New("log in to view the lending account")
	}
	e.viewed.Store(true)
	block, err := e.chain.FinalBlock(ctx)
	if err != nil {
		return nil, fmt.Errorf("error reading the final block: %w", err)
	}
	active, err := e.db.ActiveRheaOps()
	if err != nil {
		return nil, fmt.Errorf("error loading lending operations: %w", err)
	}
	disc := e.discover(ctx, ids, block.Hash)
	view := &RheaAccountView{
		Block:      rheaBlock(block),
		Identities: disc.Bindings,
		Discovery:  disc.Status,
		Ops:        active,
	}
	in := &rheaAdmissionInput{discovery: disc.Status, blockTime: block.Timestamp, now: e.now(), activeOps: active}
	if disc.Status != RheaFound {
		view.Admission = rheaAdmission(in)
		switch disc.Status {
		case RheaNone:
			view.CreationFees = e.creationFeeLines(ctx, block.Hash)
		case RheaUnavailable:
			view.CreationFeesUnavailable = true
		}
		return view, nil
	}
	mca := disc.found.MCA
	view.MCA, in.mca = mca, mca
	if err := e.readAccount(ctx, view, ids, block.Hash); err != nil {
		return nil, err
	}
	in.nonce = *view.Nonce
	ops := make([]*db.RheaOp, 0, len(active))
	for _, op := range active {
		if op.MCA == mca {
			ops = append(ops, op)
		}
	}
	view.Ops = ops
	acct, err := e.recordAccount(disc.found, block)
	if err != nil {
		e.log.Errorf("Error recording the lending account: %v", err)
	} else {
		in.account = acct
		view.FirstSeen = &RheaBlock{Height: acct.FirstSeenHeight, Hash: acct.FirstSeenHash, Time: acct.FirstSeenTime}
	}
	view.Admission = rheaAdmission(in)
	e.walkMtx.Lock()
	view.Admission.LastWalk = e.lastWalks[mca]
	e.walkMtx.Unlock()
	if view.Admission.RecoveryUnresolved {
		e.poke()
	}
	return view, nil
}

// creationFeeLines reads, at blockHash, the creation fee line of every
// allowlisted market, each with the market's creation minimum. USD values
// and the minimum come from Caribou's fiat rates; a market without a
// current rate is unavailable, as is one whose fee cannot be read. The
// minimum is computed from the fee and rate in hand, with no further
// reads.
func (e *rheaEngine) creationFeeLines(ctx context.Context, blockHash string) []*RheaFeeLine {
	lines := make([]*RheaFeeLine, 0, len(e.dep.Markets))
	for _, m := range e.dep.Markets {
		unavailable := func(err error) {
			lines = append(lines, &RheaFeeLine{TokenID: m.TokenID, Symbol: m.Symbol, OneTime: true, Unavailable: err.Error()})
		}
		rate, err := e.marketUSDRate(m)
		if err != nil {
			unavailable(err)
			continue
		}
		cost, err := e.creationCost(ctx, m.TokenID, blockHash)
		if err != nil {
			unavailable(err)
			continue
		}
		md, _, err := e.reader.FTMetadata(ctx, m.TokenID, blockHash)
		if err != nil {
			unavailable(err)
			continue
		}
		d := int(md.Decimals)
		least, err := creationMinimum(cost.total(), d, rate)
		if err != nil {
			unavailable(err)
			continue
		}
		fl := creationFeeLine(m, d, cost, rate)
		fl.CreationMinimum = &RheaCreationMinimum{Amount: least.String(), USD: rheaUSD(least, d, rate)}
		lines = append(lines, fl)
	}
	return lines
}

// readAccount fills the MCA's chain state into view, all read at blockHash.
func (e *rheaEngine) readAccount(ctx context.Context, view *RheaAccountView, ids []rheaIdentity, blockHash string) error {
	mca := view.MCA
	nonce, _, err := e.reader.Nonce(ctx, mca, blockHash)
	if err != nil {
		return err
	}
	view.Nonce = &nonce
	acctView, err := e.chain.ViewAccount(ctx, mca, blockHash)
	if err != nil {
		return err
	}
	if acctView.BlockHash != blockHash {
		return fmt.Errorf("account %s read at block %s, requested %s", mca, acctView.BlockHash, blockHash)
	}
	latest, _, err := e.reader.LatestMCACodeHash(ctx, blockHash)
	if err != nil {
		return err
	}
	view.CodeHash, view.LatestCodeHash = acctView.GlobalContractHash, latest
	view.CodeIsLatest = acctView.GlobalContractHash != "" && acctView.GlobalContractHash == latest

	wallets, _, err := e.reader.WalletsByMCA(ctx, mca, blockHash)
	if err != nil {
		return err
	}
	ours := make(map[string]bool, len(ids))
	for _, id := range ids {
		ours[string(id.Identity)] = true
	}
	for _, w := range wallets {
		rw := &RheaWallet{Kind: w.Kind, ID: w.ID, Ours: w.Kind == rhea.WalletKindEVM && ours[w.ID]}
		view.Wallets = append(view.Wallets, rw)
		if !rw.Ours {
			view.ForeignWallets = true
		}
	}

	pos, _, err := e.reader.AccountPositions(ctx, mca, blockHash)
	if err != nil {
		return err
	}
	view.BurrowAccount = pos != nil
	if pos == nil {
		pos = &rhea.Positions{AccountID: mca, Positions: map[string]*rhea.Position{}}
	}
	view.HasDebt = pos.HasDebt()
	listed := make(map[string]bool, len(e.dep.Markets))
	for _, m := range e.dep.Markets {
		listed[m.TokenID] = true
		a, _, err := e.reader.Asset(ctx, m.TokenID, blockHash)
		if err != nil {
			return err
		}
		md, _, err := e.reader.FTMetadata(ctx, m.TokenID, blockHash)
		if err != nil {
			return err
		}
		bal, _, err := e.reader.FTBalance(ctx, m.TokenID, mca, blockHash)
		if err != nil {
			return err
		}
		x := a.Config.ExtraDecimals
		supplied := pos.SuppliedBalance(m.TokenID)
		var supplyUnavailable string
		if _, err := e.marketUSDRate(m); err != nil {
			supplyUnavailable = err.Error()
		}
		view.Positions = append(view.Positions, &RheaPosition{
			Symbol:            m.Symbol,
			TokenID:           m.TokenID,
			Decimals:          md.Decimals,
			Supplied:          internalToToken(supplied, x),
			Collateral:        internalToToken(pos.CollateralBalance(m.TokenID), x),
			Borrowed:          internalToToken(pos.BorrowedBalance(m.TokenID), x),
			SuppliedInternal:  supplied.String(),
			PlainBalance:      bal.String(),
			CanWithdraw:       a.Config.CanWithdraw,
			SupplyUnavailable: supplyUnavailable,
		})
	}
	addUnlisted := func(kind string, bals []*rhea.AssetBalance) {
		for _, b := range bals {
			if !listed[b.TokenID] && b.Balance.Sign() > 0 {
				view.Unlisted = append(view.Unlisted, &RheaHolding{TokenID: b.TokenID, Kind: kind, Internal: b.Balance.String()})
			}
		}
	}
	addUnlisted("supplied", pos.Supplied)
	for _, p := range pos.Positions {
		addUnlisted("collateral", p.Collateral)
		addUnlisted("borrowed", p.Borrowed)
	}
	return nil
}

// RheaOp is a persisted lending operation. The type and its storage live
// in wallet/db.
type RheaOp = db.RheaOp

// RheaOps returns every lending operation, newest first.
func (c *Core) RheaOps() ([]*RheaOp, error) {
	return c.db.RheaOps()
}
