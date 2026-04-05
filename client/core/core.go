// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/comms"
	"decred.org/dcrdex/client/db"
	"decred.org/dcrdex/client/db/bolt"
	"decred.org/dcrdex/client/mnemonic"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/config"
	"decred.org/dcrdex/dex/dexnet"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/encrypt"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
	serverdex "decred.org/dcrdex/server/dex"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/hdkeychain/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const (
	// tickCheckDivisions is how many times to tick trades per broadcast timeout
	// interval. e.g. 12 min btimeout / 8 divisions = 90 sec between checks.
	tickCheckDivisions = 8
	// defaultTickInterval is the tick interval used before the broadcast
	// timeout is known (e.g. startup with down server).
	defaultTickInterval = 30 * time.Second

	marketTradeRedemptionSlippageBuffer = 2

	// preimageReqTimeout the server's preimage request timeout period. When
	// considered with a market's epoch duration, this is used to detect when an
	// order should have gone through matching for a certain epoch. TODO:
	// consider sharing const for the preimage timeout with the server packages,
	// or a config response field if it should be considered variable.
	preimageReqTimeout = 20 * time.Second

	// wsMaxAnomalyCount is the maximum websocket connection anomaly after which
	// a client receives a notification to check their connectivity.
	wsMaxAnomalyCount = 3
	// If a client's websocket connection to a server disconnects before
	// wsAnomalyDuration since last connect time, the client's websocket
	// connection anomaly count is increased.
	wsAnomalyDuration = 60 * time.Minute

	// This is a configurable server parameter, but we're assuming servers have
	// changed it from the default , We're using this for the v1 ConnectResult,
	// where we don't have the necessary information to calculate our bonded
	// tier, so we calculate our bonus/revoked tier from the score in the
	// ConnectResult.
	// defaultPenaltyThreshold = 20 unused

	// legacySeedLength is the length of the generated app seed used for app protection.
	legacySeedLength = 64

	// pokesCapacity is the maximum number of poke notifications that
	// will be cached.
	pokesCapacity = 100

	// walletLockTimeout is the default timeout used when locking wallets.
	walletLockTimeout = 5 * time.Second
)

var (
	unbip = dex.BipIDSymbol
	// The coin waiters will query for transaction data every recheckInterval.
	recheckInterval = time.Second * 5
	// When waiting for a wallet to sync, a SyncStatus check will be performed
	// every syncTickerPeriod. var instead of const for testing purposes.
	syncTickerPeriod = 3 * time.Second
	// supportedAPIVers are the DEX server API versions this client is capable
	// of communicating with.
	//
	// NOTE: API version may change at any time. Keep this in mind when
	// updating the API. Long-running operations may start and end with
	// differing versions.
	supportedAPIVers = []int32{serverdex.PerMatchAddrVersion}
	// ActiveOrdersLogoutErr is returned from logout when there are active
	// orders.
	ActiveOrdersLogoutErr = errors.New("cannot log out with active orders")
	// ErrTooManyActiveMatches is returned from trade when the number of
	// active matches exceeds the limit.
	ErrTooManyActiveMatches = errors.New("too many active matches")
	// walletDisabledErrStr is the error message returned when trying to use a
	// disabled wallet.
	walletDisabledErrStr = "%s wallet is disabled"

	errTimeout = errors.New("timeout")
)

type pendingFeeState struct {
	confs uint32
	asset uint32
}

// DefaultResponseTimeout is the default timeout for responses after a request is
// successfully sent.
const (
	DefaultResponseTimeout = comms.DefaultResponseTimeout
	fundingTxWait          = time.Minute // TODO: share var with server/market or put in config
)

// temporaryOrderIDCounter is used for inflight orders and must never be zero
// when used for an inflight order.
var temporaryOrderIDCounter uint64

// blockWaiter is a message waiting to be stamped, signed, and sent once a
// specified coin has the requisite confirmations. The blockWaiter is similar to
// dcrdex/server/blockWaiter.Waiter, but is different enough to warrant a
// separate type.
type blockWaiter struct {
	assetID uint32
	trigger func() (bool, error)
	action  func(error)
}

// Config is the configuration for the Core.
type Config struct {
	// DBPath is a filepath to use for the client database. If the database does
	// not already exist, it will be created.
	DBPath string
	// Net is the current network.
	Net dex.Network
	// Logger is the Core's logger and is also used to create the sub-loggers
	// for the asset backends.
	Logger dex.Logger
	// Onion is the address (host:port) of a Tor proxy for use with DEX hosts
	// with a .onion address. To use Tor with regular DEX addresses as well, set
	// TorProxy.
	Onion string
	// TorProxy specifies the address of a Tor proxy server.
	TorProxy string
	// TorIsolation specifies whether to enable Tor circuit isolation.
	TorIsolation bool
	// Language. A BCP 47 language tag. Default is en-US.
	Language string

	// NoAutoWalletLock instructs Core to skip locking the wallet on shutdown or
	// logout. This can be helpful if the user wants the wallet to remain
	// unlocked. e.g. They started with the wallet unlocked, or they intend to
	// start Core again and wish to avoid the time to unlock a locked wallet on
	// startup.
	NoAutoWalletLock bool // zero value is legacy behavior
	// NoAutoDBBackup instructs the DB to skip the creation of a backup DB file
	// on shutdown. This is useful if the consumer is using the BackupDB method,
	// or simply creating manual backups of the DB file after shutdown.
	NoAutoDBBackup bool // zero value is legacy behavior
	// UnlockCoinsOnLogin indicates that on wallet connect during login, or on
	// creation of a new wallet, all coins with the wallet should be unlocked.
	UnlockCoinsOnLogin bool
	// ExtensionModeFile is the path to a file that specifies configuration
	// for running core in extension mode, which gives the caller options for
	// e.g. limiting the ability to configure wallets.
	ExtensionModeFile string

	TheOneHost string

	Mesh bool
	// MaxActiveMatches is the maximum number of active swap matches allowed
	// per DEX connection before new orders are deferred. Zero means use the
	// default (48).
	MaxActiveMatches int
}

// locale is data associated with the currently selected language.
type locale struct {
	lang    language.Tag
	m       map[Topic]*translation
	printer *message.Printer
}

// Core is the core client application. Core manages DEX connections, wallets,
// database access, match negotiation and more.
type Core struct {
	ctx           context.Context
	wg            sync.WaitGroup
	ready         chan struct{}
	rotate        chan struct{}
	cfg           *Config
	log           dex.Logger
	db            db.DB
	net           dex.Network
	lockTimeTaker time.Duration
	lockTimeMaker time.Duration
	intl          atomic.Value // *locale

	extensionModeConfig *ExtensionModeConfig

	// construction or init sets credentials
	credMtx     sync.RWMutex
	credentials *db.PrimaryCredentials

	loginMtx      sync.Mutex
	loggedIn      bool
	multisigXPriv *hdkeychain.ExtendedKey // derived from creds.EncSeed on login

	seedGenerationTime uint64

	newCrypter func([]byte) encrypt.Crypter
	reCrypter  func([]byte, []byte) (encrypt.Crypter, error)

	walletMtx sync.RWMutex
	wallets   map[uint32]*xcWallet

	waiterMtx    sync.RWMutex
	blockWaiters map[string]*blockWaiter

	tipMtx     sync.Mutex
	tipPending map[uint32]uint64 // assetID -> latest pending tip height
	tipActive  map[uint32]bool   // assetID -> whether tipChange is running

	balMtx     sync.Mutex
	balPending map[uint32]*asset.Balance // assetID -> latest pending balance
	balActive  map[uint32]bool           // assetID -> whether balance processing is running

	noteMtx   sync.RWMutex
	noteChans map[uint64]chan Notification

	sentCommitsMtx sync.Mutex
	sentCommits    map[order.Commitment]chan struct{}

	ratesMtx        sync.RWMutex
	fiatRateSources map[string]*commonRateSource

	reFiat chan struct{}

	notes chan asset.WalletNotification

	requestedActionMtx sync.RWMutex
	requestedActions   map[string]*asset.ActionRequiredNote

}

// New is the constructor for a new Core.
func New(cfg *Config) (*Core, error) {
	if cfg.Logger == nil {
		return nil, fmt.Errorf("Core.Config must specify a Logger")
	}
	dbOpts := bolt.Opts{
		BackupOnShutdown: !cfg.NoAutoDBBackup,
	}
	boltDB, err := bolt.NewDB(cfg.DBPath, cfg.Logger.SubLogger("DB"), dbOpts)
	if err != nil {
		return nil, fmt.Errorf("database initialization error: %w", err)
	}
	if cfg.TorProxy != "" {
		if _, _, err = net.SplitHostPort(cfg.TorProxy); err != nil {
			return nil, err
		}
		dexnet.SetProxy(cfg.TorProxy)
	}
	if cfg.Onion != "" {
		if _, _, err = net.SplitHostPort(cfg.Onion); err != nil {
			return nil, err
		}
	} else { // default to torproxy if onion not set explicitly
		cfg.Onion = cfg.TorProxy
	}

	parseLanguage := func(langStr string) (language.Tag, error) {
		acceptLang, err := language.Parse(langStr)
		if err != nil {
			return language.Und, fmt.Errorf("unable to parse requested language: %w", err)
		}
		var langs []language.Tag
		for locale := range locales {
			tag, err := language.Parse(locale)
			if err != nil {
				return language.Und, fmt.Errorf("bad %v: %w", locale, err)
			}
			langs = append(langs, tag)
		}
		matcher := language.NewMatcher(langs)
		_, idx, conf := matcher.Match(acceptLang) // use index because tag may end up as something hyper specific like zh-Hans-u-rg-cnzzzz
		tag := langs[idx]
		switch conf {
		case language.Exact:
		case language.High, language.Low:
			cfg.Logger.Infof("Using language %v", tag)
		case language.No:
			// Fallback to English instead of returning error
			cfg.Logger.Warnf("Language %q not supported, falling back to %s", langStr, originLang)
			return language.AmericanEnglish, nil
		}
		return tag, nil
	}

	lang := language.Und

	// Check if the user has set a language with SetLanguage.
	if langStr, err := boltDB.Language(); err != nil {
		cfg.Logger.Errorf("Error loading language from database: %v", err)
	} else if len(langStr) > 0 {
		if lang, err = parseLanguage(langStr); err != nil {
			cfg.Logger.Errorf("Error parsing language retrieved from database %q: %w", langStr, err)
		}
	}

	// If they haven't changed the language through the UI, perhaps its set in
	// configuration.
	if lang.IsRoot() && cfg.Language != "" {
		if lang, err = parseLanguage(cfg.Language); err != nil {
			return nil, err
		}
	}

	// Default language is English.
	if lang.IsRoot() {
		lang = language.AmericanEnglish
	}

	cfg.Logger.Debugf("Using locale printer for %q", lang)

	translations, found := locales[lang.String()]
	if !found {
		cfg.Logger.Warnf("Language %q not supported, falling back to %s", lang, originLang)
		lang = language.AmericanEnglish
		translations = locales[originLang]
	}

	// Try to get the primary credentials, but ignore no-credentials error here
	// because the client may not be initialized.
	creds, err := boltDB.PrimaryCredentials()
	if err != nil && !errors.Is(err, db.ErrNoCredentials) {
		return nil, err
	}

	seedGenerationTime, err := boltDB.SeedGenerationTime()
	if err != nil && !errors.Is(err, db.ErrNoSeedGenTime) {
		return nil, err
	}

	var xCfg *ExtensionModeConfig
	if cfg.ExtensionModeFile != "" {
		b, err := os.ReadFile(cfg.ExtensionModeFile)
		if err != nil {
			return nil, fmt.Errorf("error reading extension mode file at %q: %w", cfg.ExtensionModeFile, err)
		}
		if err := json.Unmarshal(b, &xCfg); err != nil {
			return nil, fmt.Errorf("error unmarshalling extension mode file: %w", err)
		}
	}

	c := &Core{
		cfg:           cfg,
		credentials:   creds,
		ready:         make(chan struct{}),
		rotate:        make(chan struct{}, 1),
		log:           cfg.Logger,
		db:            boltDB,
		wallets:       make(map[uint32]*xcWallet),
		net:           cfg.Net,
		lockTimeTaker: dex.LockTimeTaker(cfg.Net),
		lockTimeMaker: dex.LockTimeMaker(cfg.Net),
		blockWaiters:  make(map[string]*blockWaiter),
		tipPending:    make(map[uint32]uint64),
		tipActive:     make(map[uint32]bool),
		balPending:    make(map[uint32]*asset.Balance),
		balActive:     make(map[uint32]bool),
		newCrypter:    encrypt.NewCrypter,
		reCrypter:     encrypt.Deserialize,
		noteChans:     make(map[uint64]chan Notification),

		extensionModeConfig: xCfg,
		seedGenerationTime:  seedGenerationTime,

		fiatRateSources: make(map[string]*commonRateSource),
		reFiat:          make(chan struct{}, 1),

		notes:            make(chan asset.WalletNotification, 128),
		requestedActions: make(map[string]*asset.ActionRequiredNote),
	}

	c.intl.Store(&locale{
		lang:    lang,
		m:       translations,
		printer: message.NewPrinter(lang),
	})

	// Populate the initial user data. User won't include any DEX info yet, as
	// those are retrieved when Run is called and the core connects to the DEXes.
	c.log.Debugf("new client core created")
	return c, nil
}

// Run runs the core. Satisfies the runner.Runner interface.
func (c *Core) Run(ctx context.Context) {
	c.log.Infof("Starting Bison Wallet core")
	// Store the context as a field, since we will need to spawn new DEX threads
	// when new accounts are registered.
	c.ctx = ctx
	err := c.initialize()
	if err != nil { // connectDEX gets ctx for the wsConn
		c.log.Critical(err)
		close(c.ready) // unblock <-Ready()
		return
	}
	close(c.ready)

	// The DB starts first and stops last.
	ctxDB, stopDB := context.WithCancel(context.Background())
	var dbWG sync.WaitGroup
	dbWG.Add(1)
	go func() {
		defer dbWG.Done()
		c.db.Run(ctxDB)
	}()

	// Retrieve disabled fiat rate sources from database.
	disabledSources, err := c.db.DisabledRateSources()
	if err != nil {
		c.log.Errorf("Unable to retrieve disabled fiat rate source: %v", err)
	}

	// Construct enabled fiat rate sources.
fetchers:
	for token, rateFetcher := range fiatRateFetchers {
		for _, v := range disabledSources {
			if token == v {
				continue fetchers
			}
		}
		c.fiatRateSources[token] = newCommonRateSource(rateFetcher)
	}
	c.fetchFiatExchangeRates(ctx)

	// Start a goroutine to keep the FeeState updated.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			tick := time.NewTicker(time.Minute * 5)
			select {
			case <-tick.C:
				for _, w := range c.xcWallets() {
					if w.connected() {
						w.feeRate() // updates the fee state internally.
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Handle wallet notifications.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			select {
			case n := <-c.notes:
				c.handleWalletNotification(n)
			case <-ctx.Done():
				return
			}
		}
	}()

	c.wg.Wait() // block here until all goroutines except DB complete

	if err := c.db.SavePokes(c.pokes()); err != nil {
		c.log.Errorf("Error saving pokes: %v", err)
	}

	// Stop the DB after dexConnections and other goroutines are done.
	stopDB()
	dbWG.Wait()

	// Lock and disconnect the wallets.
	c.walletMtx.Lock()
	defer c.walletMtx.Unlock()
	for assetID, wallet := range c.wallets {
		delete(c.wallets, assetID)
		if !wallet.connected() {
			continue
		}
		if !c.cfg.NoAutoWalletLock && wallet.unlocked() { // no-op if Logout did it
			if mw, is := wallet.Wallet.(asset.FundsMixer); is {
				if stats, err := mw.FundsMixingStats(); err == nil && stats.Enabled {
					c.log.Infof("Skipping lock for %s wallet (mixing active)", strings.ToUpper(unbip(assetID)))
					wallet.Disconnect()
					continue
				}
			}
			symb := strings.ToUpper(unbip(assetID))
			c.log.Infof("Locking %s wallet", symb)
			if err := wallet.Lock(walletLockTimeout); err != nil {
				c.log.Errorf("Failed to lock %v wallet: %v", symb, err)
			}
		}
		wallet.Disconnect()
	}

	c.log.Infof("Bison Wallet core off")
}

// Ready returns a channel that is closed when Run completes its initialization
// tasks and Core becomes ready for use.
func (c *Core) Ready() <-chan struct{} {
	return c.ready
}

func (c *Core) locale() *locale {
	return c.intl.Load().(*locale)
}

// SetLanguage sets the langauge used for notifications. The language set with
// SetLanguage persists through restarts and will override any language set in
// configuration.
func (c *Core) SetLanguage(lang string) error {
	tag, err := language.Parse(lang)
	if err != nil {
		return fmt.Errorf("error parsing language %q: %w", lang, err)
	}

	translations, found := locales[lang]
	if !found {
		c.log.Warnf("Language %q not supported, using %s", lang, originLang)
		lang = originLang
		tag, _ = language.Parse(originLang) // Safe to ignore error, originLang is known valid
		translations = locales[originLang]
	}
	if err := c.db.SetLanguage(lang); err != nil {
		return fmt.Errorf("error storing language: %w", err)
	}
	c.intl.Store(&locale{
		lang:    tag,
		m:       translations,
		printer: message.NewPrinter(tag),
	})
	return nil
}

// Language is the currently configured language.
func (c *Core) Language() string {
	return c.locale().lang.String()
}

// SetCompanionToken stores the companion app auth token in the database.
func (c *Core) SetCompanionToken(token string) error {
	return c.db.SetCompanionToken(token)
}

// CompanionToken retrieves the companion app auth token from the database.
func (c *Core) CompanionToken() (string, error) {
	return c.db.CompanionToken()
}

// BackupDB makes a backup of the database at the specified location, optionally
// overwriting any existing file and compacting the database.
func (c *Core) BackupDB(dst string, overwrite, compact bool) error {
	return c.db.BackupTo(dst, overwrite, compact)
}

const defaultDEXPort = "7232"

// addrHost returns the host or url:port pair for an address.
func addrHost(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	const defaultHost = "localhost"
	const missingPort = "missing port in address"
	// Empty addresses are localhost.
	if addr == "" {
		return defaultHost + ":" + defaultDEXPort, nil
	}
	host, port, splitErr := net.SplitHostPort(addr)
	_, portErr := strconv.ParseUint(port, 10, 16)

	// net.SplitHostPort will error on anything not in the format
	// string:string or :string or if a colon is in an unexpected position,
	// such as in the scheme.
	// If the port isn't a port, it must also be parsed.
	if splitErr != nil || portErr != nil {
		// Any address with no colons is appended with the default port.
		var addrErr *net.AddrError
		if errors.As(splitErr, &addrErr) && addrErr.Err == missingPort {
			host = strings.Trim(addrErr.Addr, "[]") // JoinHostPort expects no brackets for ipv6 hosts
			return net.JoinHostPort(host, defaultDEXPort), nil
		}
		// These are addresses with at least one colon in an unexpected
		// position.
		a, err := url.Parse(addr)
		// This address is of an unknown format.
		if err != nil {
			return "", fmt.Errorf("addrHost: unable to parse address '%s'", addr)
		}
		host, port = a.Hostname(), a.Port()
		// If the address parses but there is no port, append the default port.
		if port == "" {
			return net.JoinHostPort(host, defaultDEXPort), nil
		}
	}
	// We have a port but no host. Replace with localhost.
	if host == "" {
		host = defaultHost
	}
	return net.JoinHostPort(host, port), nil
}

// creds returns the *PrimaryCredentials.
func (c *Core) creds() *db.PrimaryCredentials {
	c.credMtx.RLock()
	defer c.credMtx.RUnlock()
	if c.credentials == nil {
		return nil
	}
	if len(c.credentials.EncInnerKey) == 0 {
		// database upgraded, but Core hasn't updated the PrimaryCredentials.
		return nil
	}
	return c.credentials
}

// setCredentials stores the *PrimaryCredentials.
func (c *Core) setCredentials(creds *db.PrimaryCredentials) {
	c.credMtx.Lock()
	c.credentials = creds
	c.credMtx.Unlock()
}

// Network returns the current DEX network.
func (c *Core) Network() dex.Network {
	return c.net
}

// TorProxy returns the configured Tor proxy address, or "" if none.
func (c *Core) TorProxy() string {
	return c.cfg.TorProxy
}

// Exchanges returns an empty map; DEX connectivity is not available in this build.
func (c *Core) Exchanges() map[string]*Exchange {
	return make(map[string]*Exchange)
}

// Exchange always returns an error; DEX connectivity is not available in this build.
func (c *Core) Exchange(host string) (*Exchange, error) {
	return nil, fmt.Errorf("DEX connectivity not available in this build")
}

// ExchangeMarket always returns an error; DEX connectivity is not available in this build.
func (c *Core) ExchangeMarket(host string, baseID, quoteID uint32) (*Market, error) {
	return nil, fmt.Errorf("DEX connectivity not available in this build")
}

// MarketConfig always returns an error; DEX connectivity is not available in this build.
func (c *Core) MarketConfig(host string, baseID, quoteID uint32) (*msgjson.Market, error) {
	return nil, fmt.Errorf("DEX connectivity not available in this build")
}

// wallet gets the wallet for the specified asset ID in a thread-safe way.
func (c *Core) wallet(assetID uint32) (*xcWallet, bool) {
	c.walletMtx.RLock()
	w, found := c.wallets[assetID]
	c.walletMtx.RUnlock()
	return w, found
}

func (c *Core) initializeTokenWallet(tokenID uint32, tkn *asset.Token) error {
	if _, found := c.wallet(tkn.ParentID); !found {
		return fmt.Errorf("no parent wallet %d for token %s", tkn.ParentID, tkn.Name)
	}
	dbWallet, err := c.createTokenDBWallet(tokenID, tkn, &WalletForm{
		AssetID: tokenID,
		Config:  make(map[string]string),
		Type:    tkn.Definition.Type,
	})
	if err != nil {
		return fmt.Errorf("error creating %s token wallet with existing %s parent wallet: %w",
			dex.BipIDSymbol(tokenID), dex.BipIDSymbol(tkn.ParentID), err)
	}
	w, err := c.loadXCWallet(dbWallet)
	if err != nil {
		return fmt.Errorf("error loading newly created %s token wallet: %w", tkn.Name, err)
	}
	bals := &WalletBalance{Balance: &db.Balance{Balance: asset.Balance{Other: make(map[asset.BalanceCategory]asset.CustomBalance)}}}
	w.setBalance(bals) // update xcWallet's WalletBalance
	dbWallet.Balance = bals.Balance
	// Store the wallet in the database.
	err = c.db.UpdateWallet(dbWallet)
	if err != nil {
		return fmt.Errorf("error storing new token wallet credentials: %w", err)
	}
	c.log.Infof("Token wallet for %s automatically created", dex.BipIDSymbol(tokenID))
	c.updateWallet(tokenID, w)
	return nil
}

// encryptionKey retrieves the application encryption key. The password is used
// to recreate the outer key/crypter, which is then used to decode and recreate
// the inner key/crypter.
func (c *Core) encryptionKey(pw []byte) (encrypt.Crypter, error) {
	creds := c.creds()
	if creds == nil {
		return nil, fmt.Errorf("primary credentials not retrieved. Is the client initialized?")
	}
	outerCrypter, err := c.reCrypter(pw, creds.OuterKeyParams)
	if err != nil {
		return nil, fmt.Errorf("outer key deserialization error: %w", err)
	}
	defer outerCrypter.Close()
	innerKey, err := outerCrypter.Decrypt(creds.EncInnerKey)
	if err != nil {
		return nil, fmt.Errorf("inner key decryption error: %w", err)
	}
	innerCrypter, err := c.reCrypter(innerKey, creds.InnerKeyParams)
	if err != nil {
		return nil, fmt.Errorf("inner key deserialization error: %w", err)
	}
	return innerCrypter, nil
}

func (c *Core) storeDepositAddress(wdbID []byte, addr string) error {
	// Store the new address in the DB.
	dbWallet, err := c.db.Wallet(wdbID)
	if err != nil {
		return fmt.Errorf("error retrieving DB wallet: %w", err)
	}
	dbWallet.Address = addr
	return c.db.UpdateWallet(dbWallet)
}

// connectAndUpdateWalletResumeTrades creates a connection to a wallet and
// updates the balance. If resumeTrades is set to true, an attempt to resume
// any trades that were unable to be resumed at startup will be made.
func (c *Core) connectAndUpdateWalletResumeTrades(w *xcWallet, resumeTrades bool) error {
	assetID := w.AssetID

	token := asset.TokenInfo(assetID)
	if token != nil {
		parentWallet, found := c.wallet(token.ParentID)
		if !found {
			return fmt.Errorf("token %s wallet has no %s parent?", unbip(assetID), unbip(token.ParentID))
		}
		if !parentWallet.connected() {
			if err := c.connectAndUpdateWalletResumeTrades(parentWallet, resumeTrades); err != nil {
				return fmt.Errorf("failed to connect %s parent wallet for %s token: %v",
					unbip(token.ParentID), unbip(assetID), err)
			}
		}
	}

	c.log.Debugf("Connecting wallet for %s", unbip(assetID))
	addr := w.currentDepositAddress()
	newAddr, err := c.connectWalletResumeTrades(w, resumeTrades)
	if err != nil {
		return fmt.Errorf("connectWallet: %w", err) // core.Error with code connectWalletErr
	}
	if newAddr != addr {
		c.log.Infof("New deposit address for %v wallet: %v", unbip(assetID), newAddr)
		if err = c.storeDepositAddress(w.dbID, newAddr); err != nil {
			return fmt.Errorf("storeDepositAddress: %w", err)
		}
	}
	// First update balances since it is included in WalletState. Ignore errors
	// because some wallets may not reveal balance until unlocked.
	_, err = c.updateWalletBalance(w)
	if err != nil {
		// Warn because the balances will be stale.
		c.log.Warnf("Could not retrieve balances from %s wallet: %v", unbip(assetID), err)
	}

	c.notify(newWalletStateNote(w.state()))
	return nil
}

// connectAndUpdateWallet creates a connection to a wallet and updates the
// balance.
func (c *Core) connectAndUpdateWallet(w *xcWallet) error {
	return c.connectAndUpdateWalletResumeTrades(w, true)
}

// connectedWallet fetches a wallet and will connect the wallet if it is not
// already connected. If the wallet gets connected, this also emits WalletState
// and WalletBalance notification.
func (c *Core) connectedWallet(assetID uint32) (*xcWallet, error) {
	wallet, exists := c.wallet(assetID)
	if !exists {
		return nil, newError(missingWalletErr, "no configured wallet found for %s (%d)",
			strings.ToUpper(unbip(assetID)), assetID)
	}
	if !wallet.connected() {
		err := c.connectAndUpdateWallet(wallet)
		if err != nil {
			return nil, err
		}
	}
	return wallet, nil
}

// connectWalletResumeTrades connects to the wallet and returns the deposit
// address validated by the xcWallet after connecting. If the wallet backend
// is still syncing, this also starts a goroutine to monitor sync status,
// emitting WalletStateNotes on each progress update. If resumeTrades is set to
// true, an attempt to resume any trades that were unable to be resumed at
// startup will be made.
func (c *Core) connectWalletResumeTrades(w *xcWallet, resumeTrades bool) (depositAddr string, err error) {
	if w.isDisabled() {
		return "", fmt.Errorf(walletDisabledErrStr, w.Symbol)
	}

	err = w.Connect() // ensures valid deposit address
	if err != nil {
		return "", newError(connectWalletErr, "failed to connect %s wallet: %w", w.Symbol, err)
	}

	w.processWalletTransactions(w.PendingTransactions(c.ctx))

	w.mtx.RLock()
	depositAddr = w.address
	synced := w.syncStatus.Synced
	w.mtx.RUnlock()

	// If the wallet is synced, update the bond reserves, logging any balance
	// insufficiencies, otherwise start a loop to check the sync status until it
	// is.
	if synced {
		// bond reserves not applicable in this build
	} else {
		c.startWalletSyncMonitor(w)
	}

	return
}

// connectWallet connects to the wallet and returns the deposit address
// validated by the xcWallet after connecting. If the wallet backend is still
// syncing, this also starts a goroutine to monitor sync status, emitting
// WalletStateNotes on each progress update.
func (c *Core) connectWallet(w *xcWallet) (depositAddr string, err error) {
	return c.connectWalletResumeTrades(w, true)
}

// unlockWalletResumeTrades will unlock a wallet if it is not yet unlocked. If
// resumeTrades is set to true, an attempt to resume any trades that were
// unable to be resumed at startup will be made.
func (c *Core) unlockWalletResumeTrades(crypter encrypt.Crypter, wallet *xcWallet, resumeTrades bool) error {
	// Unlock if either the backend itself is locked or if we lack a cached
	// unencrypted password for encrypted wallets.
	if !wallet.unlocked() {
		if crypter == nil {
			return newError(noAuthError, "wallet locked and no password provided")
		}
		// Note that in cases where we already had the cached decrypted password
		// but it was just the backend reporting as locked, only unlocking the
		// backend is needed but this redecrypts the password using the provided
		// crypter. This case could instead be handled with a refreshUnlock.
		err := wallet.Unlock(crypter)
		if err != nil {
			return newError(walletAuthErr, "failed to unlock %s wallet: %w",
				unbip(wallet.AssetID), err)
		}
		// Notify new wallet state.
		c.notify(newWalletStateNote(wallet.state()))

	}

	return nil
}

// unlockWallet will unlock a wallet if it is not yet unlocked.
func (c *Core) unlockWallet(crypter encrypt.Crypter, wallet *xcWallet) error {
	return c.unlockWalletResumeTrades(crypter, wallet, true)
}

// connectAndUnlockResumeTrades will connect to the wallet if not already
// connected, and unlock the wallet if not already unlocked. If the wallet
// backend is still syncing, this also starts a goroutine to monitor sync
// status, emitting WalletStateNotes on each progress update. If resumeTrades
// is set to true, an attempt to resume any trades that were unable to be
// resumed at startup will be made.
func (c *Core) connectAndUnlockResumeTrades(crypter encrypt.Crypter, wallet *xcWallet, resumeTrades bool) error {
	if !wallet.connected() {
		err := c.connectAndUpdateWalletResumeTrades(wallet, resumeTrades)
		if err != nil {
			return err
		}
	}

	return c.unlockWalletResumeTrades(crypter, wallet, resumeTrades)
}

// connectAndUnlock will connect to the wallet if not already connected,
// and unlock the wallet if not already unlocked. If the wallet backend
// is still syncing, this also starts a goroutine to monitor sync status,
// emitting WalletStateNotes on each progress update.
func (c *Core) connectAndUnlock(crypter encrypt.Crypter, wallet *xcWallet) error {
	return c.connectAndUnlockResumeTrades(crypter, wallet, true)
}

// walletBalance gets the xcWallet's current WalletBalance, which includes the
// db.Balance plus order/contract locked amounts. The data is not stored. Use
// updateWalletBalance instead to also update xcWallet.balance and the DB.
func (c *Core) walletBalance(wallet *xcWallet) (*WalletBalance, error) {
	bal, err := wallet.Balance()
	if err != nil {
		return nil, err
	}
	return &WalletBalance{
		Balance: &db.Balance{
			Balance: *bal,
			Stamp:   time.Now(),
		},
	}, nil
}

// updateWalletBalance retrieves balances for the wallet, updates
// xcWallet.balance and the balance in the DB, and emits a BalanceNote.
func (c *Core) updateWalletBalance(wallet *xcWallet) (*WalletBalance, error) {
	walletBal, err := c.walletBalance(wallet)
	if err != nil {
		return nil, err
	}
	return walletBal, c.storeAndSendWalletBalance(wallet, walletBal)
}

func (c *Core) storeAndSendWalletBalance(wallet *xcWallet, walletBal *WalletBalance) error {
	wallet.setBalance(walletBal)

	// Store the db.Balance.
	err := c.db.UpdateBalance(wallet.dbID, walletBal.Balance)
	if err != nil {
		return fmt.Errorf("error updating %s balance in database: %w", unbip(wallet.AssetID), err)
	}
	c.notify(newBalanceNote(wallet.AssetID, walletBal))
	return nil
}

// updateBalances updates the balance for every key in the counter map.
// Notifications are sent.
func (c *Core) updateBalances(assets assetMap) {
	if len(assets) == 0 {
		return
	}
	for assetID := range assets {
		w, exists := c.wallet(assetID)
		if !exists {
			// This should never be the case, but log an error in case I'm
			// wrong or something changes.
			c.log.Errorf("non-existent %d wallet should exist", assetID)
			continue
		}
		_, err := c.updateWalletBalance(w)
		if err != nil {
			c.log.Errorf("error updating %q balance: %v", unbip(assetID), err)
			continue
		}

		if token := asset.TokenInfo(assetID); token != nil {
			if _, alreadyUpdating := assets[token.ParentID]; alreadyUpdating {
				continue
			}
			parentWallet, exists := c.wallet(token.ParentID)
			if !exists {
				c.log.Errorf("non-existent %d wallet should exist", token.ParentID)
				continue
			}
			_, err := c.updateWalletBalance(parentWallet)
			if err != nil {
				c.log.Errorf("error updating %q balance: %v", unbip(token.ParentID), err)
				continue
			}
		}
	}
}

// updateAssetBalance updates the balance for the specified asset. A
// notification is sent.
func (c *Core) updateAssetBalance(assetID uint32) {
	c.updateBalances(assetMap{assetID: struct{}{}})
}

// xcWallets creates a slice of the c.wallets xcWallets.
func (c *Core) xcWallets() []*xcWallet {
	c.walletMtx.RLock()
	defer c.walletMtx.RUnlock()
	wallets := make([]*xcWallet, 0, len(c.wallets))
	for _, wallet := range c.wallets {
		wallets = append(wallets, wallet)
	}
	return wallets
}

// Wallets creates a slice of WalletState for all known wallets.
func (c *Core) Wallets() []*WalletState {
	wallets := c.xcWallets()
	state := make([]*WalletState, 0, len(wallets))
	for _, wallet := range wallets {
		state = append(state, wallet.state())
	}
	return state
}

// ToggleWalletStatus changes a wallet's status to either disabled or enabled.
func (c *Core) ToggleWalletStatus(assetID uint32, disable bool) error {
	wallet, exists := c.wallet(assetID)
	if !exists {
		return newError(missingWalletErr, "no configured wallet found for %s (%d)",
			strings.ToUpper(unbip(assetID)), assetID)
	}

	// Return early if this wallet is already disabled or already enabled.
	if disable == wallet.isDisabled() {
		return nil
	}

	// If this wallet is a parent, disable/enable all token wallets.
	var affectedWallets []*xcWallet
	if disable {
		// Ensure wallet is not a parent of an enabled token wallet with active
		// orders.
		if assetInfo := asset.Asset(assetID); assetInfo != nil {
			for id := range assetInfo.Tokens {
				if wallet, exists := c.wallet(id); exists && !wallet.isDisabled() {
					affectedWallets = append(affectedWallets, wallet)
				}
			}
		}

		// If wallet is a parent wallet, it will be the last to be disconnected
		// and disabled.
		affectedWallets = append(affectedWallets, wallet)

		// Disconnect and disable all affected wallets.
		for _, wallet := range affectedWallets {
			if wallet.connected() {
				wallet.Disconnect() // before disable or it refuses
			}
			wallet.setDisabled(true)
		}
	} else {
		if wallet.parent != nil && wallet.parent.isDisabled() {
			// Ensure parent wallet starts first.
			affectedWallets = append(affectedWallets, wallet.parent)
		}

		affectedWallets = append(affectedWallets, wallet)

		for _, wallet := range affectedWallets {
			// Update wallet status before attempting to connect wallet because disabled
			// wallets cannot be connected to.
			wallet.setDisabled(false)

			// Attempt to connect wallet.
			err := c.connectAndUpdateWallet(wallet)
			if err != nil {
				c.log.Errorf("Error connecting to %s wallet: %v", unbip(assetID), err)
			}
		}
	}

	for _, wallet := range affectedWallets {
		// Update db with wallet status.
		err := c.db.UpdateWalletStatus(wallet.dbID, disable)
		if err != nil {
			return fmt.Errorf("db.UpdateWalletStatus error: %w", err)
		}

		c.notify(newWalletStateNote(wallet.state()))
	}

	return nil
}

// SupportedAssets returns a map of asset information for supported assets.
func (c *Core) SupportedAssets() map[uint32]*SupportedAsset {
	return c.assetMap()
}

// assetMap returns a map of asset information for supported assets.
func (c *Core) assetMap() map[uint32]*SupportedAsset {
	supported := asset.Assets()
	assets := make(map[uint32]*SupportedAsset, len(supported))
	c.walletMtx.RLock()
	defer c.walletMtx.RUnlock()
	for assetID, asset := range supported {
		var wallet *WalletState
		w, found := c.wallets[assetID]
		if found {
			wallet = w.state()
		}
		assets[assetID] = &SupportedAsset{
			ID:       assetID,
			Symbol:   asset.Symbol,
			Wallet:   wallet,
			Info:     asset.Info,
			Name:     asset.Info.Name,
			UnitInfo: asset.Info.UnitInfo,
		}
		for tokenID, token := range asset.Tokens {
			wallet = nil
			w, found := c.wallets[tokenID]
			if found {
				wallet = w.state()
			}
			assets[tokenID] = &SupportedAsset{
				ID:       tokenID,
				Symbol:   dex.BipIDSymbol(tokenID),
				Wallet:   wallet,
				Token:    token,
				Name:     token.Name,
				UnitInfo: token.UnitInfo,
			}
		}
	}
	return assets
}

// User is a thread-safe getter for the User.
func (c *Core) User() *User {
	return &User{
		Assets:             c.assetMap(),
		Exchanges:          c.Exchanges(),
		Initialized:        c.IsInitialized(),
		SeedGenerationTime: c.seedGenerationTime,
		FiatRates:          c.fiatConversions(),
		Net:                c.net,
		ExtensionConfig:    c.extensionModeConfig,
		Actions:            c.requestedActionsList(),
	}
}

func (c *Core) requestedActionsList() []*asset.ActionRequiredNote {
	c.requestedActionMtx.RLock()
	defer c.requestedActionMtx.RUnlock()
	actions := make([]*asset.ActionRequiredNote, 0, len(c.requestedActions))
	for _, a := range c.requestedActions {
		actions = append(actions, a)
	}
	return actions
}

// CreateWallet creates a new exchange wallet.
func (c *Core) CreateWallet(appPW, walletPW []byte, form *WalletForm) error {
	crypter, err := c.encryptionKey(appPW)
	if err != nil {
		return err
	}
	return c.createWallet(crypter, walletPW, form)
}

func (c *Core) createWallet(crypter encrypt.Crypter, walletPW []byte, form *WalletForm) (err error) {
	assetID := form.AssetID
	symbol := unbip(assetID)
	_, exists := c.wallet(assetID)
	if exists {
		return fmt.Errorf("%s wallet already exists", symbol)
	}

	token := asset.TokenInfo(assetID)
	var dbWallet *db.Wallet
	if token == nil {
		dbWallet, err = c.createDBWallet(crypter, form, walletPW)
	} else {
		dbWallet, err = c.createTokenDBWallet(assetID, token, form)
	}
	if err != nil {
		return err
	}

	wallet, err := c.loadXCWallet(dbWallet)
	if err != nil {
		return fmt.Errorf("error loading wallet for %d -> %s: %w", assetID, symbol, err)
	}
	// Block PeersChange until we know this wallet is ready.
	atomic.StoreUint32(wallet.broadcasting, 0)

	if err = wallet.OpenWithPW(c.ctx, crypter); err != nil {
		return err
	}

	dbWallet.Address, err = c.connectWallet(wallet)
	if err != nil {
		return err
	}

	if c.cfg.UnlockCoinsOnLogin {
		if err = wallet.ReturnCoins(nil); err != nil {
			c.log.Errorf("Failed to unlock all %s wallet coins: %v", unbip(wallet.AssetID), err)
		}
	}

	initErr := func(s string, a ...any) error {
		_ = wallet.Lock(2 * time.Second) // just try, but don't confuse the user with an error
		wallet.Disconnect()
		return fmt.Errorf(s, a...)
	}

	err = c.unlockWallet(crypter, wallet) // no-op if !wallet.Wallet.Locked() && len(encPW) == 0
	if err != nil {
		wallet.Disconnect()
		return fmt.Errorf("%s wallet authentication error: %w", symbol, err)
	}

	balances, err := c.walletBalance(wallet)
	if err != nil {
		return initErr("error getting wallet balance for %s: %w", symbol, err)
	}
	wallet.setBalance(balances)         // update xcWallet's WalletBalance
	dbWallet.Balance = balances.Balance // store the db.Balance

	// Store the wallet in the database.
	err = c.db.UpdateWallet(dbWallet)
	if err != nil {
		return initErr("error storing wallet credentials: %w", err)
	}

	c.log.Infof("Created %s wallet. Balance available = %d / "+
		"locked = %d / locked in contracts = %d, Deposit address = %s",
		symbol, balances.Available, balances.Locked, balances.ContractLocked,
		dbWallet.Address)

	// The wallet has been successfully created. Store it.
	c.updateWallet(assetID, wallet)

	atomic.StoreUint32(wallet.broadcasting, 1)
	c.notify(newWalletStateNote(wallet.state()))
	c.walletCheckAndNotify(wallet)

	// Create all token wallets
	if token == nil {
		for tokenID, tkn := range asset.Asset(assetID).Tokens {
			form := &WalletForm{
				AssetID: tokenID,
				Config:  make(map[string]string),
				Type:    tkn.Definition.Type,
			}
			if err := c.createWallet(crypter, nil, form); err != nil {
				c.log.Errorf("Error creating token %s wallet for parent %s", tkn.Name, symbol)
			}
		}
	}

	return nil
}

func (c *Core) createDBWallet(crypter encrypt.Crypter, form *WalletForm, walletPW []byte) (*db.Wallet, error) {
	walletDef, err := asset.WalletDef(form.AssetID, form.Type)
	if err != nil {
		return nil, newError(assetSupportErr, "asset.WalletDef error: %w", err)
	}

	// Sometimes core will insert data into the Settings map to communicate
	// information back to the wallet, so it cannot be nil.
	if form.Config == nil {
		form.Config = make(map[string]string)
	}

	// Remove unused key-values from parsed settings before saving to db.
	// Especially necessary if settings was parsed from a config file, b/c
	// config files usually define more key-values than we need.
	// Expected keys should be lowercase because config.Parse returns lowercase
	// keys.
	expectedKeys := make(map[string]bool, len(walletDef.ConfigOpts))
	for _, option := range walletDef.ConfigOpts {
		expectedKeys[strings.ToLower(option.Key)] = true
	}
	for key := range form.Config {
		if !expectedKeys[key] {
			delete(form.Config, key)
		}
	}

	if walletDef.Seeded {
		if len(walletPW) > 0 {
			return nil, errors.New("external password incompatible with seeded wallet")
		}
		walletPW, err = c.createSeededWallet(form.AssetID, crypter, form)
		if err != nil {
			return nil, err
		}
	}

	var encPW []byte
	if len(walletPW) > 0 {
		encPW, err = crypter.Encrypt(walletPW)
		if err != nil {
			return nil, fmt.Errorf("wallet password encryption error: %w", err)
		}
	}

	return &db.Wallet{
		Type:        walletDef.Type,
		AssetID:     form.AssetID,
		Settings:    form.Config,
		EncryptedPW: encPW,
		// Balance and Address are set after connect.
	}, nil
}

func (c *Core) createTokenDBWallet(tokenID uint32, token *asset.Token, form *WalletForm) (*db.Wallet, error) {
	wallet, found := c.wallet(token.ParentID)
	if !found {
		return nil, fmt.Errorf("no parent wallet %d for token %d (%s)", token.ParentID, tokenID, unbip(tokenID))
	}

	tokenMaster, is := wallet.Wallet.(asset.TokenMaster)
	if !is {
		return nil, fmt.Errorf("parent wallet %s is not a TokenMaster", unbip(token.ParentID))
	}

	// Sometimes core will insert data into the Settings map to communicate
	// information back to the wallet, so it cannot be nil.
	if form.Config == nil {
		form.Config = make(map[string]string)
	}

	if err := tokenMaster.CreateTokenWallet(tokenID, form.Config); err != nil {
		return nil, fmt.Errorf("CreateTokenWallet error: %w", err)
	}

	return &db.Wallet{
		Type:     form.Type,
		AssetID:  tokenID,
		Settings: form.Config,
		// EncryptedPW ignored because we assume throughout that token wallet
		// authorization is handled by the parent.
		// Balance and Address are set after connect.
	}, nil
}

// createSeededWallet initializes a seeded wallet with an asset-specific seed
// and password derived deterministically from the app seed. The password is
// returned for encrypting and storing.
func (c *Core) createSeededWallet(assetID uint32, crypter encrypt.Crypter, form *WalletForm) ([]byte, error) {
	seed, pw, err := c.assetSeedAndPass(assetID, crypter)
	if err != nil {
		return nil, err
	}
	defer encode.ClearBytes(seed)

	var bday uint64
	if creds := c.creds(); !creds.Birthday.IsZero() {
		bday = uint64(creds.Birthday.Unix())
	}

	c.log.Infof("Initializing a %s wallet", unbip(assetID))
	if err = asset.CreateWallet(assetID, &asset.CreateWalletParams{
		Type:     form.Type,
		Seed:     seed,
		Pass:     pw,
		Birthday: bday,
		Settings: form.Config,
		DataDir:  c.assetDataDirectory(assetID),
		Net:      c.net,
		Logger:   c.log.SubLogger(unbip(assetID)),
		TorProxy: c.cfg.TorProxy,
	}); err != nil {
		return nil, fmt.Errorf("Error creating wallet: %w", err)
	}

	return pw, nil
}

func (c *Core) assetSeedAndPass(assetID uint32, crypter encrypt.Crypter) (seed, pass []byte, err error) {
	creds := c.creds()
	if creds == nil {
		return nil, nil, errors.New("no v2 credentials stored")
	}

	if tkn := asset.TokenInfo(assetID); tkn != nil {
		return nil, nil, fmt.Errorf("%s is a token. assets seeds are for base chains onlyu. did you want %s",
			tkn.Name, asset.Asset(tkn.ParentID).Info.Name)
	}

	appSeed, err := crypter.Decrypt(creds.EncSeed)
	if err != nil {
		return nil, nil, fmt.Errorf("app seed decryption error: %w", err)
	}

	seed, pass = AssetSeedAndPass(assetID, appSeed)
	return seed, pass, nil
}

// AssetSeedAndPass derives the wallet seed and password that would be used to
// create a native wallet for a particular asset and application seed. Depending
// on external wallet software and their key derivation paths, this seed may be
// usable for accessing funds outside of DEX applications, e.g. btcwallet.
func AssetSeedAndPass(assetID uint32, appSeed []byte) ([]byte, []byte) {
	const accountBasedSeedAssetID = 60 // ETH
	seedAssetID := assetID
	if ai, _ := asset.Info(assetID); ai != nil && ai.BlockchainClass.IsEVM() {
		seedAssetID = accountBasedSeedAssetID
	}
	// Tokens asset IDs shouldn't be passed in, but if they are, return the seed
	// for the parent ID.
	if tkn := asset.TokenInfo(assetID); tkn != nil {
		if ai, _ := asset.Info(tkn.ParentID); ai != nil {
			if ai.BlockchainClass.IsEVM() {
				seedAssetID = accountBasedSeedAssetID
			}
		}
	}

	b := make([]byte, len(appSeed)+4)
	copy(b, appSeed)
	binary.BigEndian.PutUint32(b[len(appSeed):], seedAssetID)
	s := blake256.Sum256(b)
	p := blake256.Sum256(s[:])
	return s[:], p[:]
}

// assetDataDirectory is a directory for a wallet to use for local storage.
func (c *Core) assetDataDirectory(assetID uint32) string {
	return filepath.Join(filepath.Dir(c.cfg.DBPath), "assetdb", unbip(assetID))
}

// assetDataBackupDirectory is a directory for a wallet to use for backups of
// data. Wallet data is copied here instead of being deleted when recovering a
// wallet.
func (c *Core) assetDataBackupDirectory(assetID uint32) string {
	return filepath.Join(filepath.Dir(c.cfg.DBPath), "assetdb-backup", unbip(assetID))
}

// loadWallet uses the data from the database to construct a new exchange
// wallet. The returned wallet is running but not connected.
func (c *Core) loadXCWallet(dbWallet *db.Wallet) (*xcWallet, error) {
	var parent *xcWallet
	assetID := dbWallet.AssetID

	// Construct the unconnected xcWallet.
	symbol := unbip(assetID)
	wallet := &xcWallet{ // captured by the PeersChange closure
		AssetID: assetID,
		Symbol:  symbol,
		log:     c.log.SubLogger(symbol),
		balance: &WalletBalance{
			Balance: dbWallet.Balance,
		},
		encPass:      dbWallet.EncryptedPW,
		address:      dbWallet.Address,
		peerCount:    -1, // no count yet
		dbID:         dbWallet.ID(),
		walletType:   dbWallet.Type,
		broadcasting: new(uint32),
		disabled:     dbWallet.Disabled,
		syncStatus:   &asset.SyncStatus{},
		pendingTxs:   make(map[string]*asset.WalletTransaction),
	}

	token := asset.TokenInfo(assetID)

	peersChange := func(numPeers uint32, err error) {
		if c.ctx.Err() != nil {
			return
		}

		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.peerChange(wallet, numPeers, err)
		}()
	}

	// Ensure default settings are always supplied to the wallet as they
	// may not be saved yet.
	walletDef, err := asset.WalletDef(assetID, dbWallet.Type)
	if err != nil {
		return nil, newError(assetSupportErr, "asset.WalletDef error: %w", err)
	}
	defaultValues := make(map[string]string, len(walletDef.ConfigOpts))
	for _, option := range walletDef.ConfigOpts {
		defaultValues[strings.ToLower(option.Key)] = option.DefaultValue
	}
	settings := dbWallet.Settings
	for k, v := range defaultValues {
		if _, has := settings[k]; !has {
			settings[k] = v
		}
	}

	log := c.log.SubLogger(unbip(assetID))
	var w asset.Wallet
	if token == nil {
		walletCfg := &asset.WalletConfig{
			Type:        dbWallet.Type,
			Settings:    settings,
			Emit:        asset.NewWalletEmitter(c.notes, assetID, log),
			PeersChange: peersChange,
			DataDir:     c.assetDataDirectory(assetID),
			TorProxy:    c.cfg.TorProxy,
		}

		settings[asset.SpecialSettingActivelyUsed] = "false"
		defer delete(settings, asset.SpecialSettingActivelyUsed)

		w, err = asset.OpenWallet(assetID, walletCfg, log, c.net)
	} else {
		var found bool
		parent, found = c.wallet(token.ParentID)
		if !found {
			return nil, fmt.Errorf("cannot load %s wallet before %s wallet", unbip(assetID), unbip(token.ParentID))
		}

		tokenMaster, is := parent.Wallet.(asset.TokenMaster)
		if !is {
			return nil, fmt.Errorf("%s token's %s parent wallet is not a TokenMaster", unbip(assetID), unbip(token.ParentID))
		}

		w, err = tokenMaster.OpenTokenWallet(&asset.TokenConfig{
			AssetID:     assetID,
			Settings:    settings,
			Emit:        asset.NewWalletEmitter(c.notes, assetID, log),
			PeersChange: peersChange,
		})
	}
	if err != nil {
		if errors.Is(err, asset.ErrWalletTypeDisabled) {
			subject, details := c.formatDetails(TopicWalletTypeDeprecated, unbip(assetID))
			c.notify(newWalletConfigNote(TopicWalletTypeDeprecated, subject, details, db.WarningLevel, nil))
		}
		return nil, fmt.Errorf("error opening wallet: %w", err)
	}

	wallet.Wallet = w
	wallet.parent = parent
	wallet.supportedVersions = w.Info().SupportedVersions
	wallet.connector = dex.NewConnectionMaster(w)
	wallet.traits = asset.DetermineWalletTraits(w)
	atomic.StoreUint32(wallet.broadcasting, 1)
	return wallet, nil
}

// WalletState returns the *WalletState for the asset ID.
func (c *Core) WalletState(assetID uint32) *WalletState {
	c.walletMtx.Lock()
	defer c.walletMtx.Unlock()
	wallet, has := c.wallets[assetID]
	if !has {
		c.log.Tracef("wallet status requested for unknown asset %d -> %s", assetID, unbip(assetID))
		return nil
	}
	return wallet.state()
}

// WalletTraits gets the traits for the wallet.
func (c *Core) WalletTraits(assetID uint32) (asset.WalletTrait, error) {
	w, found := c.wallet(assetID)
	if !found {
		return 0, fmt.Errorf("no %d wallet found", assetID)
	}
	return w.traits, nil
}

// walletCheckAndNotify sets the xcWallet's synced and syncProgress fields from
// the wallet's SyncStatus result, emits a WalletStateNote, and returns the
// synced value. When synced is true, this also updates the wallet's balance,
// stores the balance in the DB, emits a BalanceNote, and updates the bond
// reserves (with balance checking).
func (c *Core) walletCheckAndNotify(w *xcWallet) bool {
	ss, err := w.SyncStatus()
	if err != nil {
		c.log.Errorf("Unable to get wallet/node sync status for %s: %v",
			unbip(w.AssetID), err)
		return false
	}

	w.mtx.Lock()
	wasSynced := w.syncStatus.Synced
	w.syncStatus = ss
	w.mtx.Unlock()

	if atomic.LoadUint32(w.broadcasting) == 1 {
		c.notify(newWalletSyncNote(w.AssetID, ss))
	}
	if ss.Synced && !wasSynced {
		c.updateWalletBalance(w)
		c.log.Debugf("Wallet synced for asset %s", unbip(w.AssetID))
		// bond reserves not applicable in this build
	}
	return ss.Synced
}

// startWalletSyncMonitor repeatedly calls walletCheckAndNotify on a ticker
// until it is synced. This launches the monitor goroutine, if not already
// running, and immediately returns.
func (c *Core) startWalletSyncMonitor(wallet *xcWallet) {
	// Prevent multiple sync monitors for this wallet.
	if !atomic.CompareAndSwapUint32(&wallet.monitored, 0, 1) {
		return // already monitoring
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer atomic.StoreUint32(&wallet.monitored, 0)
		ticker := time.NewTicker(syncTickerPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if c.walletCheckAndNotify(wallet) {
					return
				}
			case <-wallet.connector.Done():
				c.log.Warnf("%v wallet shut down before sync completed.", wallet.Info().Name)
				return
			case <-c.ctx.Done():
				return
			}
		}
	}()
}

// RescanWallet will issue a Rescan command to the wallet if supported by the
// wallet implementation. It is up to the underlying wallet backend if and how
// to implement this functionality. It may be asynchronous. Core will emit
// wallet state notifications until the rescan is complete. If force is false,
// this will check for active orders involving this asset before initiating a
// rescan. WARNING: It is ill-advised to initiate a wallet rescan with active
// orders unless as a last ditch effort to get the wallet to recognize a
// transaction needed to complete a swap.
func (c *Core) RescanWallet(assetID uint32, force bool) error {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return fmt.Errorf("OpenWallet: wallet not found for %d -> %s: %w",
			assetID, unbip(assetID), err)
	}

	walletDef, err := asset.WalletDef(assetID, wallet.walletType)
	if err != nil {
		return newError(assetSupportErr, "asset.WalletDef error: %w", err)
	}

	var bday uint64 // unix time seconds
	if walletDef.Seeded {
		creds := c.creds()
		if !creds.Birthday.IsZero() {
			bday = uint64(creds.Birthday.Unix())
		}
	}

	// Begin potentially asynchronous wallet rescan operation.
	if err = wallet.rescan(c.ctx, bday); err != nil {
		return err
	}

	if c.walletCheckAndNotify(wallet) {
		return nil // sync done, Rescan may have by synchronous or a no-op
	}

	// Synchronization still running. Launch a status update goroutine.
	c.startWalletSyncMonitor(wallet)

	return nil
}

// AbandonTransaction marks an unconfirmed transaction and all its descendants
// as abandoned. This allows the wallet to forget about the transaction and
// potentially spend its inputs in a different transaction. This is useful for
// transactions that are stuck due to low fees. Returns an error if the wallet
// does not support transaction abandonment, if the transaction is confirmed,
// or if the transaction does not exist.
func (c *Core) AbandonTransaction(assetID uint32, txID string) error {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return fmt.Errorf("wallet not found for %d -> %s: %w", assetID, unbip(assetID), err)
	}

	abandoner, ok := wallet.Wallet.(asset.TxAbandoner)
	if !ok {
		return fmt.Errorf("%s wallet does not support abandoning transactions", unbip(assetID))
	}

	return abandoner.AbandonTransaction(c.ctx, txID)
}

func (c *Core) removeWallet(assetID uint32) {
	c.walletMtx.Lock()
	defer c.walletMtx.Unlock()
	delete(c.wallets, assetID)
}

// updateWallet stores or updates an asset's wallet.
func (c *Core) updateWallet(assetID uint32, wallet *xcWallet) {
	c.walletMtx.Lock()
	defer c.walletMtx.Unlock()
	c.wallets[assetID] = wallet
}

// RecoverWallet will retrieve some recovery information from the wallet,
// which may not be possible if the wallet is too corrupted. Disconnect and
// destroy the old wallet, create a new one, and if the recovery information
// was retrieved from the old wallet, send this information to the new one.
// If force is false, this will check for active orders involving this
// asset before initiating a rescan. WARNING: It is ill-advised to initiate
// a wallet recovery with active orders unless the wallet db is definitely
// corrupted and even a rescan will not save it.
//
// DO NOT MAKE CONCURRENT CALLS TO THIS FUNCTION WITH THE SAME ASSET.
func (c *Core) RecoverWallet(assetID uint32, appPW []byte, force bool) error {
	crypter, err := c.encryptionKey(appPW)
	if err != nil {
		return newError(authErr, "RecoverWallet password error: %w", err)
	}
	defer crypter.Close()

	oldWallet, found := c.wallet(assetID)
	if !found {
		return fmt.Errorf("RecoverWallet: wallet not found for %d -> %s",
			assetID, unbip(assetID))
	}

	recoverer, isRecoverer := oldWallet.Wallet.(asset.Recoverer)
	if !isRecoverer {
		return errors.New("wallet is not a recoverer")
	}
	walletDef, err := asset.WalletDef(assetID, oldWallet.walletType)
	if err != nil {
		return newError(assetSupportErr, "asset.WalletDef error: %w", err)
	}
	// Unseeded wallets shouldn't implement the Recoverer interface. This
	// is just an additional check for safety.
	if !walletDef.Seeded {
		return fmt.Errorf("can only recover a seeded wallet")
	}

	dbWallet, err := c.db.Wallet(oldWallet.dbID)
	if err != nil {
		return fmt.Errorf("error retrieving DB wallet: %w", err)
	}

	seed, pw, err := c.assetSeedAndPass(assetID, crypter)
	if err != nil {
		return err
	}
	defer encode.ClearBytes(seed)
	defer encode.ClearBytes(pw)

	if oldWallet.connected() {
		if recoveryCfg, err := recoverer.GetRecoveryCfg(); err != nil {
			c.log.Errorf("RecoverWallet: unable to get recovery config: %v", err)
		} else {
			// merge recoveryCfg with dbWallet.Settings
			maps.Copy(dbWallet.Settings, recoveryCfg)
		}
		oldWallet.Disconnect() // wallet now shut down and w.hookedUp == false -> connected() returns false
	}
	// Before we pull the plug, remove the wallet from wallets map. Otherwise,
	// connectedWallet would try to connect it.
	c.removeWallet(assetID)

	if err = recoverer.Move(c.assetDataBackupDirectory(assetID)); err != nil {
		return fmt.Errorf("failed to move wallet data to backup folder: %w", err)
	}

	if err = asset.CreateWallet(assetID, &asset.CreateWalletParams{
		Type:     dbWallet.Type,
		Seed:     seed,
		Pass:     pw,
		Settings: dbWallet.Settings,
		DataDir:  c.assetDataDirectory(assetID),
		Net:      c.net,
		Logger:   c.log.SubLogger(unbip(assetID)),
		TorProxy: c.cfg.TorProxy,
	}); err != nil {
		return fmt.Errorf("error creating wallet: %w", err)
	}

	newWallet, err := c.loadXCWallet(dbWallet)
	if err != nil {
		return newError(walletErr, "error loading wallet for %d -> %s: %w",
			assetID, unbip(assetID), err)
	}

	// Ensure we are not trying to connect to a disabled wallet.
	if newWallet.isDisabled() {
		c.updateWallet(assetID, newWallet)
	} else {
		_, err = c.connectWallet(newWallet)
		if err != nil {
			return err
		}
		c.updateWalletBalance(newWallet)

		c.updateAssetWalletRefs(newWallet)

		err = c.unlockWallet(crypter, newWallet)
		if err != nil {
			return err
		}
	}

	c.notify(newWalletStateNote(newWallet.state()))

	return nil
}

// OpenWallet opens (unlocks) the wallet for use.
func (c *Core) OpenWallet(assetID uint32, appPW []byte) error {
	crypter, err := c.encryptionKey(appPW)
	if err != nil {
		return err
	}
	defer crypter.Close()
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return fmt.Errorf("OpenWallet: wallet not found for %d -> %s: %w", assetID, unbip(assetID), err)
	}
	err = c.unlockWallet(crypter, wallet)
	if err != nil {
		return newError(walletAuthErr, "failed to unlock %s wallet: %w", unbip(assetID), err)
	}

	state := wallet.state()
	balances, err := c.updateWalletBalance(wallet)
	if err != nil {
		return err
	}
	c.log.Infof("Connected to and unlocked %s wallet. Balance available "+
		"= %d / locked = %d / locked in contracts = %d, locked in bonds = %d, Deposit address = %s",
		state.Symbol, balances.Available, balances.Locked, balances.ContractLocked,
		balances.BondLocked, state.Address)

	c.notify(newWalletStateNote(state))
	return nil
}

// CloseWallet locks the wallet for the specified asset.
func (c *Core) CloseWallet(assetID uint32) error {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return fmt.Errorf("wallet not found for %d -> %s: %w", assetID, unbip(assetID), err)
	}
	err = wallet.Lock(walletLockTimeout)
	if err != nil {
		return err
	}
	c.notify(newWalletStateNote(wallet.state()))

	return nil
}

// ConnectWallet connects to the wallet without unlocking.
func (c *Core) ConnectWallet(assetID uint32) error {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return err
	}
	c.notify(newWalletStateNote(wallet.state()))
	return nil
}

// WalletSettings fetches the current wallet configuration details from the
// database.
func (c *Core) WalletSettings(assetID uint32) (map[string]string, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "%d -> %s wallet not found", assetID, unbip(assetID))
	}
	// Get the settings from the database.
	dbWallet, err := c.db.Wallet(wallet.dbID)
	if err != nil {
		return nil, codedError(dbErr, err)
	}
	return dbWallet.Settings, nil
}

// ChangeAppPass updates the application password to the provided new password
// after validating the current password.
func (c *Core) ChangeAppPass(appPW, newAppPW []byte) error {
	// Validate current password.
	if len(newAppPW) == 0 {
		return fmt.Errorf("application password cannot be empty")
	}
	creds := c.creds()
	if creds == nil {
		return fmt.Errorf("no primary credentials. Is the client initialized?")
	}

	outerCrypter, err := c.reCrypter(appPW, creds.OuterKeyParams)
	if err != nil {
		return newError(authErr, "old password error: %w", err)
	}
	defer outerCrypter.Close()
	innerKey, err := outerCrypter.Decrypt(creds.EncInnerKey)
	if err != nil {
		return fmt.Errorf("inner key decryption error: %w", err)
	}

	return c.changeAppPass(newAppPW, innerKey, creds)
}

// changeAppPass is a shared method to reset or change user password.
func (c *Core) changeAppPass(newAppPW, innerKey []byte, creds *db.PrimaryCredentials) error {
	newOuterCrypter := c.newCrypter(newAppPW)
	defer newOuterCrypter.Close()
	newEncInnerKey, err := newOuterCrypter.Encrypt(innerKey)
	if err != nil {
		return fmt.Errorf("encryption error: %v", err)
	}

	newCreds := &db.PrimaryCredentials{
		EncSeed:        creds.EncSeed,
		EncInnerKey:    newEncInnerKey,
		InnerKeyParams: creds.InnerKeyParams,
		Birthday:       creds.Birthday,
		OuterKeyParams: newOuterCrypter.Serialize(),
		Version:        creds.Version,
	}

	err = c.db.SetPrimaryCredentials(newCreds)
	if err != nil {
		return fmt.Errorf("SetPrimaryCredentials error: %w", err)
	}

	c.setCredentials(newCreds)

	return nil
}

// ResetAppPass resets the application password to the provided new password.
func (c *Core) ResetAppPass(newPass []byte, seedStr string) (err error) {
	if !c.IsInitialized() {
		return fmt.Errorf("cannot reset password before client is initialized")
	}

	if len(newPass) == 0 {
		return fmt.Errorf("application password cannot be empty")
	}

	seed, _, err := decodeSeedString(seedStr)
	if err != nil {
		return fmt.Errorf("error decoding seed: %w", err)
	}

	creds := c.creds()
	if creds == nil {
		return fmt.Errorf("no credentials stored")
	}

	innerKey := seedInnerKey(seed)
	_, err = c.reCrypter(innerKey[:], creds.InnerKeyParams)
	if err != nil {
		c.log.Errorf("Error reseting password with seed: %v", err)
		return errors.New("incorrect seed")
	}

	return c.changeAppPass(newPass, innerKey[:], creds)
}

// ReconfigureWallet updates the wallet configuration settings, it also updates
// the password if newWalletPW is non-nil. Do not make concurrent calls to
// ReconfigureWallet for the same asset.
func (c *Core) ReconfigureWallet(appPW, newWalletPW []byte, form *WalletForm) error {
	crypter, err := c.encryptionKey(appPW)
	if err != nil {
		return newError(authErr, "ReconfigureWallet password error: %w", err)
	}
	defer crypter.Close()

	assetID := form.AssetID

	walletDef, err := asset.WalletDef(assetID, form.Type)
	if err != nil {
		return newError(assetSupportErr, "asset.WalletDef error: %w", err)
	}
	if walletDef.Seeded && newWalletPW != nil {
		return newError(passwordErr, "cannot set a password on a built-in(seeded) wallet")
	}

	oldWallet, found := c.wallet(assetID)
	if !found {
		return newError(missingWalletErr, "%d -> %s wallet not found",
			assetID, unbip(assetID))
	}

	if oldWallet.isDisabled() { // disabled wallet cannot perform operation.
		return fmt.Errorf(walletDisabledErrStr, strings.ToUpper(unbip(assetID)))
	}

	oldDef, err := asset.WalletDef(assetID, oldWallet.walletType)
	if err != nil {
		return newError(assetSupportErr, "old wallet asset.WalletDef error: %w", err)
	}
	oldDepositAddr := oldWallet.currentDepositAddress()

	dbWallet := &db.Wallet{
		Type:        form.Type,
		AssetID:     oldWallet.AssetID,
		Settings:    form.Config,
		Balance:     &db.Balance{}, // in case retrieving new balance after connect fails
		EncryptedPW: oldWallet.encPW(),
		Address:     oldDepositAddr,
	}

	storeWithBalance := func(w *xcWallet, dbWallet *db.Wallet) error {
		balances, err := c.walletBalance(w)
		if err != nil {
			c.log.Warnf("Error getting balance for wallet %s: %v", unbip(assetID), err)
			// Do not fail in case this requires an unlocked wallet.
		} else {
			w.setBalance(balances)              // update xcWallet's WalletBalance
			dbWallet.Balance = balances.Balance // store the db.Balance
		}

		err = c.db.UpdateWallet(dbWallet)
		if err != nil {
			return newError(dbErr, "error saving wallet configuration: %w", err)
		}

		c.notify(newBalanceNote(assetID, balances)) // redundant with wallet config note?
		subject, details := c.formatDetails(TopicWalletConfigurationUpdated, unbip(assetID), w.address)
		c.notify(newWalletConfigNote(TopicWalletConfigurationUpdated, subject, details, db.Success, w.state()))

		return nil
	}

	// See if the wallet offers a quick path.
	if configurer, is := oldWallet.Wallet.(asset.LiveReconfigurer); is && oldWallet.walletType == walletDef.Type && oldWallet.connected() {
		form.Config[asset.SpecialSettingActivelyUsed] = "false"
		defer delete(form.Config, asset.SpecialSettingActivelyUsed)

		if restart, err := configurer.Reconfigure(c.ctx, &asset.WalletConfig{
			Type:     form.Type,
			Settings: form.Config,
			DataDir:  c.assetDataDirectory(assetID),
			TorProxy: c.cfg.TorProxy,
		}, oldWallet.currentDepositAddress()); err != nil {
			return fmt.Errorf("Reconfigure: %v", err)
		} else if !restart {
			// Config was updated without a need to restart.
			if owns, err := oldWallet.OwnsDepositAddress(oldWallet.currentDepositAddress()); err != nil {
				return newError(walletErr, "error checking deposit address after live config update: %w", err)
			} else if !owns {
				if dbWallet.Address, err = oldWallet.refreshDepositAddress(); err != nil {
					return newError(newAddrErr, "error refreshing deposit address after live config update: %w", err)
				}
			}
			if !oldDef.Seeded && newWalletPW != nil {
				if err = c.setWalletPassword(oldWallet, newWalletPW, crypter); err != nil {
					return newError(walletAuthErr, "failed to update password: %v", err)
				}
				dbWallet.EncryptedPW = oldWallet.encPW()

			}
			if err = storeWithBalance(oldWallet, dbWallet); err != nil {
				return err
			}
			c.log.Infof("%s wallet configuration updated without a restart 👍", unbip(assetID))
			return nil
		}
	}

	c.log.Infof("%s wallet configuration update will require a restart", unbip(assetID))

	var restartOnFail bool

	defer func() {
		if restartOnFail {
			if _, err := c.connectWallet(oldWallet); err != nil {
				c.log.Errorf("Failed to reconnect wallet after a failed reconfiguration attempt: %v", err)
			}
		}
	}()

	if walletDef.Seeded {
		exists, err := asset.WalletExists(assetID, form.Type, c.assetDataDirectory(assetID), form.Config, c.net)
		if err != nil {
			return newError(existenceCheckErr, "error checking wallet pre-existence: %w", err)
		}

		// The password on a seeded wallet is deterministic, based on the seed
		// itself, so if the seeded wallet of this Type for this asset already
		// exists, recompute the password from the app seed.
		var pw []byte
		if exists {
			_, pw, err = c.assetSeedAndPass(assetID, crypter)
			if err != nil {
				return newError(authErr, "error retrieving wallet password: %w", err)
			}
		} else {
			pw, err = c.createSeededWallet(assetID, crypter, form)
			if err != nil {
				return newError(createWalletErr, "error creating new %q-type %s wallet: %w", form.Type, unbip(assetID), err)
			}
		}
		dbWallet.EncryptedPW, err = crypter.Encrypt(pw)
		if err != nil {
			return fmt.Errorf("wallet password encryption error: %w", err)
		}

		if oldDef.Seeded && oldWallet.connected() {
			oldWallet.Disconnect()
			restartOnFail = true
		}
	} else if newWalletPW == nil && oldDef.Seeded {
		// If we're switching from a seeded wallet to a non-seeded wallet and no
		// password was provided, use empty string = wallet not encrypted.
		newWalletPW = []byte{}
	}

	// Reload the wallet with the new settings.
	wallet, err := c.loadXCWallet(dbWallet)
	if err != nil {
		return newError(walletErr, "error loading wallet for %d -> %s: %w",
			assetID, unbip(assetID), err)
	}

	// Block PeersChange until we know this wallet is ready.
	atomic.StoreUint32(wallet.broadcasting, 0)
	var success bool
	defer func() {
		if success {
			atomic.StoreUint32(wallet.broadcasting, 1)
			c.notify(newWalletStateNote(wallet.state()))
			c.walletCheckAndNotify(wallet)
		}
	}()

	// Helper function to make sure trades can be settled by the
	// keys held within the new wallet.
	sameWallet := func() error {
		return nil
	}

	reloadWallet := func(w *xcWallet, dbWallet *db.Wallet, checkSameness bool) error {
		// Must connect to ensure settings are good. This comes before
		// setWalletPassword since it would use connectAndUpdateWallet, which
		// performs additional deposit address validation and balance updates that
		// are redundant with the rest of this function.
		dbWallet.Address, err = c.connectWalletResumeTrades(w, false)
		if err != nil {
			return fmt.Errorf("connectWallet: %w", err)
		}

		if checkSameness {
			if err := sameWallet(); err != nil {
				wallet.Disconnect()
				return newError(walletErr, "new wallet cannot be used with current active trades: %w", err)
			}
			// If newWalletPW is non-nil, update the wallet's password.
			if newWalletPW != nil { // includes empty non-nil slice
				err = c.setWalletPassword(wallet, newWalletPW, crypter)
				if err != nil {
					wallet.Disconnect()
					return fmt.Errorf("setWalletPassword: %v", err)
				}
				// Update dbWallet so db.UpdateWallet below reflects the new password.
				dbWallet.EncryptedPW = wallet.encPW()
			} else if oldWallet.locallyUnlocked() {
				// If the password was not changed, carry over any cached password
				// regardless of backend lock state. loadWallet already copied encPW, so
				// this will decrypt pw rather than actually copying it, and it will
				// ensure the backend is also unlocked.
				err := wallet.Unlock(crypter) // decrypt encPW if set and unlock the backend
				if err != nil {
					wallet.Disconnect()
					return newError(walletAuthErr, "wallet successfully connected, but failed to unlock. "+
						"reconfiguration not saved: %w", err)
				}
			}
		}

		if err = storeWithBalance(w, dbWallet); err != nil {
			w.Disconnect()
			return err
		}

		c.updateAssetWalletRefs(w)
		return nil
	}

	// Reload the wallet
	if err := reloadWallet(wallet, dbWallet, true); err != nil {
		return err
	}

	restartOnFail = false
	success = true

	// If there are tokens, reload those wallets.
	for tokenID := range asset.Asset(assetID).Tokens {
		tokenWallet, found := c.wallet(tokenID)
		if found {
			tokenDBWallet, err := c.db.Wallet((&db.Wallet{AssetID: tokenID}).ID())
			if err != nil {
				c.log.Errorf("Error getting db wallet for token %s: %w", unbip(tokenID), err)
				continue
			}
			tokenWallet.Disconnect()
			tokenWallet, err = c.loadXCWallet(tokenDBWallet)
			if err != nil {
				c.log.Errorf("Error loading wallet for token %s: %w", unbip(tokenID), err)
				continue
			}
			if err := reloadWallet(tokenWallet, tokenDBWallet, false); err != nil {
				c.log.Errorf("Error reloading token wallet %s: %w", unbip(tokenID), err)
			}
		}
	}

	if oldWallet.connected() {
		// NOTE: Cannot lock the wallet backend because it may be the same as
		// the one just connected.
		go oldWallet.Disconnect()
	}


	return nil
}

// updateAssetWalletRefs sets all references of an asset's wallet to newWallet.
func (c *Core) updateAssetWalletRefs(newWallet *xcWallet) {
	c.updateWallet(newWallet.AssetID, newWallet)
}

// SetWalletPassword updates the (encrypted) password for the wallet. Returns
// passwordErr if provided newPW is nil. The wallet will be connected if it is
// not already.
func (c *Core) SetWalletPassword(appPW []byte, assetID uint32, newPW []byte) error {
	// Ensure newPW isn't nil.
	if newPW == nil {
		return newError(passwordErr, "SetWalletPassword password can't be nil")
	}

	// Check the app password and get the crypter.
	crypter, err := c.encryptionKey(appPW)
	if err != nil {
		return newError(authErr, "SetWalletPassword password error: %w", err)
	}
	defer crypter.Close()

	// Check that the specified wallet exists.
	c.walletMtx.Lock()
	defer c.walletMtx.Unlock()
	wallet, found := c.wallets[assetID]
	if !found {
		return newError(missingWalletErr, "wallet for %s (%d) is not known", unbip(assetID), assetID)
	}

	// Set new password, connecting to it if necessary to verify. It is left
	// connected since it is in the wallets map.
	return c.setWalletPassword(wallet, newPW, crypter)
}

// setWalletPassword updates the (encrypted) password for the wallet.
func (c *Core) setWalletPassword(wallet *xcWallet, newPW []byte, crypter encrypt.Crypter) error {
	authenticator, is := wallet.Wallet.(asset.Authenticator)
	if !is { // password setting is not supported by wallet.
		return newError(passwordErr, "wallet does not support password setting")
	}

	walletDef, err := asset.WalletDef(wallet.AssetID, wallet.walletType)
	if err != nil {
		return newError(assetSupportErr, "asset.WalletDef error: %w", err)
	}
	if walletDef.Seeded || asset.TokenInfo(wallet.AssetID) != nil {
		return newError(passwordErr, "cannot set a password on a seeded or token wallet")
	}

	// Connect if necessary.
	wasConnected := wallet.connected()
	if !wasConnected {
		if err := c.connectAndUpdateWallet(wallet); err != nil {
			return newError(connectionErr, "SetWalletPassword connection error: %w", err)
		}
	}

	wasUnlocked := wallet.unlocked()
	newPasswordSet := len(newPW) > 0 // excludes empty but non-nil

	// Check that the new password works.
	if newPasswordSet {
		// Encrypt password if it's not an empty string.
		encNewPW, err := crypter.Encrypt(newPW)
		if err != nil {
			return newError(encryptionErr, "encryption error: %w", err)
		}
		err = authenticator.Unlock(newPW)
		if err != nil {
			return newError(authErr,
				"setWalletPassword unlocking wallet error, is the new password correct?: %w", err)
		}
		wallet.setEncPW(encNewPW)
	} else {
		// Test that the wallet is actually good with no password. At present,
		// this means the backend either cannot be locked or unlocks with an
		// empty password. The following Lock->Unlock cycle but may be required
		// to detect a newly-unprotected wallet without reconnecting. We will
		// ignore errors in this process as we are discovering the true state.
		// check the backend directly, not using the xcWallet
		_ = authenticator.Lock()
		_ = authenticator.Unlock([]byte{})
		if authenticator.Locked() {
			if wasUnlocked { // try to re-unlock the wallet with previous encPW
				_ = c.unlockWallet(crypter, wallet)
			}
			return newError(authErr, "wallet appears to require a password")
		}
		wallet.setEncPW(nil)
	}

	err = c.db.SetWalletPassword(wallet.dbID, wallet.encPW())
	if err != nil {
		return codedError(dbErr, err)
	}

	// Re-lock the wallet if it was previously locked.
	if !wasUnlocked && newPasswordSet {
		if err = wallet.Lock(2 * time.Second); err != nil {
			c.log.Warnf("Unable to relock %s wallet: %v", unbip(wallet.AssetID), err)
		}
	}

	// Do not disconnect because the Wallet may not allow reconnection.

	subject, details := c.formatDetails(TopicWalletPasswordUpdated, unbip(wallet.AssetID))
	c.notify(newWalletConfigNote(TopicWalletPasswordUpdated, subject, details, db.Success, wallet.state()))

	return nil
}

// NewDepositAddress retrieves a new deposit address from the specified asset's
// wallet, saves it to the database, and emits a notification. If the wallet
// does not support generating new addresses, the current address will be
// returned.
func (c *Core) NewDepositAddress(assetID uint32) (string, error) {
	w, exists := c.wallet(assetID)
	if !exists {
		return "", newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	var addr string
	if _, ok := w.Wallet.(asset.NewAddresser); ok {
		// Retrieve a fresh deposit address.
		var err error
		addr, err = w.refreshDepositAddress()
		if err != nil {
			return "", err
		}
		if err = c.storeDepositAddress(w.dbID, addr); err != nil {
			return "", err
		}
		// Update wallet state in the User data struct and emit a WalletStateNote.
		c.notify(newWalletStateNote(w.state()))
	} else {
		addr = w.address
	}

	return addr, nil
}

// AddressUsed checks whether an address for a NewAddresser has been used.
func (c *Core) AddressUsed(assetID uint32, addr string) (bool, error) {
	w, exists := c.wallet(assetID)
	if !exists {
		return false, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	na, ok := w.Wallet.(asset.NewAddresser)
	if !ok {
		return false, errors.New("wallet is not a NewAddresser")
	}

	return na.AddressUsed(addr)
}

// AutoWalletConfig attempts to load setting from a wallet package's
// asset.WalletInfo.DefaultConfigPath. If settings are not found, an empty map
// is returned.
func (c *Core) AutoWalletConfig(assetID uint32, walletType string) (map[string]string, error) {
	walletDef, err := asset.WalletDef(assetID, walletType)
	if err != nil {
		return nil, newError(assetSupportErr, "asset.WalletDef error: %w", err)
	}

	if walletDef.DefaultConfigPath == "" {
		return nil, fmt.Errorf("no config path found for %s wallet, type %q", unbip(assetID), walletType)
	}

	settings, err := config.Parse(walletDef.DefaultConfigPath)
	c.log.Infof("%d %s configuration settings loaded from file at default location %s", len(settings), unbip(assetID), walletDef.DefaultConfigPath)
	if err != nil {
		c.log.Debugf("config.Parse could not load settings from default path: %v", err)
		return make(map[string]string), nil
	}
	return settings, nil
}

// AddDEX is not supported in this build (DEX connectivity removed).
func (c *Core) AddDEX(_ []byte, _ string, _ any) error {
	return fmt.Errorf("DEX connectivity not available in this build")
}

// IsInitialized checks if the app is already initialized.
func (c *Core) IsInitialized() bool {
	c.credMtx.RLock()
	defer c.credMtx.RUnlock()
	return c.credentials != nil
}

// InitializeClient sets the initial app-wide password and app seed for the
// client. The seed argument should be left nil unless restoring from seed.
func (c *Core) InitializeClient(pw []byte, restorationSeed *string) (string, error) {
	if c.IsInitialized() {
		return "", fmt.Errorf("already initialized, login instead")
	}

	_, creds, mnemonicSeed, err := c.generateCredentials(pw, restorationSeed)
	if err != nil {
		return "", err
	}

	err = c.db.SetPrimaryCredentials(creds)
	if err != nil {
		return "", fmt.Errorf("SetPrimaryCredentials error: %w", err)
	}

	seedGenTime := uint64(creds.Birthday.Unix())
	err = c.db.SetSeedGenerationTime(seedGenTime)
	if err != nil {
		return "", fmt.Errorf("SetSeedGenerationTime error: %w", err)
	}
	c.seedGenerationTime = seedGenTime

	freshSeed := restorationSeed == nil
	if freshSeed {
		subject, details := c.formatDetails(TopicSeedNeedsSaving)
		c.notify(newSecurityNote(TopicSeedNeedsSaving, subject, details, db.Success))
	}

	c.setCredentials(creds)
	return mnemonicSeed, nil
}

// ExportSeed exports the application seed.
func (c *Core) ExportSeed(pw []byte) (seedStr string, err error) {
	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return "", fmt.Errorf("ExportSeed password error: %w", err)
	}
	defer crypter.Close()

	creds := c.creds()
	if creds == nil {
		return "", fmt.Errorf("no v2 credentials stored")
	}

	seed, err := crypter.Decrypt(creds.EncSeed)
	if err != nil {
		return "", fmt.Errorf("app seed decryption error: %w", err)
	}

	if len(seed) == legacySeedLength {
		seedStr = hex.EncodeToString(seed)
	} else {
		seedStr, err = mnemonic.GenerateMnemonic(seed, creds.Birthday)
		if err != nil {
			return "", fmt.Errorf("error generating mnemonic: %w", err)
		}
	}

	return seedStr, nil
}

func decodeSeedString(seedStr string) (seed []byte, bday time.Time, err error) {
	// See if it decodes as a mnemonic seed first.
	seed, bday, err = mnemonic.DecodeMnemonic(seedStr)
	if err != nil {
		// Is it an old-school hex seed?
		bday = time.Time{}
		seed, err = hex.DecodeString(strings.Join(strings.Fields(seedStr), ""))
		if err != nil {
			return nil, time.Time{}, errors.New("unabled to decode provided seed")
		}
		if len(seed) != legacySeedLength {
			return nil, time.Time{}, errors.New("decoded seed is wrong length")
		}
	}
	return
}

// generateCredentials generates a new set of *PrimaryCredentials. The
// credentials are not stored to the database. A restoration seed can be
// provided, otherwise should be nil.
func (c *Core) generateCredentials(pw []byte, optionalSeed *string) (encrypt.Crypter, *db.PrimaryCredentials, string, error) {
	if len(pw) == 0 {
		return nil, nil, "", fmt.Errorf("empty password not allowed")
	}

	var seed []byte
	defer encode.ClearBytes(seed)
	var bday time.Time
	var mnemonicSeed string
	if optionalSeed == nil {
		bday = time.Now()
		seed, mnemonicSeed = mnemonic.New()
	} else {
		var err error
		// Is it a mnemonic seed?
		seed, bday, err = decodeSeedString(*optionalSeed)
		if err != nil {
			return nil, nil, "", err
		}
	}

	// Generate an inner key and it's Crypter.
	innerKey := seedInnerKey(seed)
	innerCrypter := c.newCrypter(innerKey[:])
	encSeed, err := innerCrypter.Encrypt(seed)
	if err != nil {
		return nil, nil, "", fmt.Errorf("client seed encryption error: %w", err)
	}

	// Generate the outer key.
	outerCrypter := c.newCrypter(pw)
	encInnerKey, err := outerCrypter.Encrypt(innerKey[:])
	if err != nil {
		return nil, nil, "", fmt.Errorf("inner key encryption error: %w", err)
	}

	creds := &db.PrimaryCredentials{
		EncSeed:        encSeed,
		EncInnerKey:    encInnerKey,
		InnerKeyParams: innerCrypter.Serialize(),
		OuterKeyParams: outerCrypter.Serialize(),
		Birthday:       bday,
		Version:        1,
	}

	return innerCrypter, creds, mnemonicSeed, nil
}

func seedInnerKey(seed []byte) []byte {
	// keyParam is a domain-specific value to ensure the resulting key is unique
	// for the specific use case of deriving an inner encryption key from the
	// seed. Any other uses of derivation from the seed should similarly create
	// their own domain-specific value to ensure uniqueness.
	//
	// It is equal to BLAKE-256([]byte("DCRDEX-InnerKey-v0")).
	keyParam := [32]byte{
		0x75, 0x25, 0xb1, 0xb6, 0x53, 0x33, 0x9e, 0x33,
		0xbe, 0x11, 0x61, 0x45, 0x1a, 0x88, 0x6f, 0x37,
		0xe7, 0x74, 0xdf, 0xca, 0xb4, 0x8a, 0xee, 0x0e,
		0x7c, 0x84, 0x60, 0x01, 0xed, 0xe5, 0xf6, 0x97,
	}
	key := make([]byte, len(seed)+len(keyParam))
	copy(key, seed)
	copy(key[len(seed):], keyParam[:])
	innerKey := blake256.Sum256(key)
	return innerKey[:]
}


// Login logs the user in. On the first login after startup or after a logout,
// this function will connect wallets, resolve active trades, and decrypt
// account keys for all known DEXes. Otherwise, it will only check whether or
// not the app pass is correct.
func (c *Core) Login(pw []byte) error {
	// Make sure the app has been initialized. This condition would error when
	// attempting to retrieve the encryption key below as well, but the
	// messaging may be confusing.
	c.credMtx.RLock()
	creds := c.credentials
	c.credMtx.RUnlock()

	if creds == nil {
		return fmt.Errorf("cannot log in because app has not been initialized")
	}

	c.notify(newLoginNote("Verifying credentials..."))
	if len(creds.EncInnerKey) == 0 {
		err := c.initializePrimaryCredentials(pw, creds.OuterKeyParams)
		if err != nil {
			// It's tempting to panic here, since Core and the db are probably
			// out of sync and the client shouldn't be doing anything else.
			c.log.Criticalf("v1 upgrade failed: %v", err)
			return err
		}
	}

	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return err
	}
	defer crypter.Close()

	switch creds.Version {
	case 0:
		if crypter, creds, err = c.upgradeV0CredsToV1(pw, *creds); err != nil {
			return fmt.Errorf("error upgrading primary credentials from version 0 to 1: %w", err)
		}
	}

	login := func() (needInit bool, err error) {
		c.loginMtx.Lock()
		defer c.loginMtx.Unlock()
		if !c.loggedIn {
			seed, err := crypter.Decrypt(creds.EncSeed)
			if err != nil {
				return false, fmt.Errorf("seed decryption error: %w", err)
			}
			defer encode.ClearBytes(seed)
			c.multisigXPriv, err = deriveMultisigXPriv(seed)
			if err != nil {
				return false, fmt.Errorf("error deriving multisig private key: %w", err)
			}

			c.loggedIn = true
			return true, nil
		}
		return false, nil
	}

	if needsInit, err := login(); err != nil {
		return err
	} else if needsInit {
		// It is not an error if we can't connect, unless we need the wallet
		// for active trades, but that condition is checked later in
		// resolveActiveTrades. We won't try to unlock here, but if the wallet
		// is needed for active trades, it will be unlocked in resolveActiveTrades
		// and the balance updated there.
		c.notify(newLoginNote("Connecting wallets..."))
		c.connectWallets(crypter) // initialize reserves
		c.notify(newLoginNote("Resuming active trades..."))
		c.resolveActiveTrades(crypter)
	}

	return nil
}

// upgradeV0CredsToV1 upgrades version 0 credentials to version 1. This update
// changes the inner key to be derived from the seed.
func (c *Core) upgradeV0CredsToV1(appPW []byte, creds db.PrimaryCredentials) (encrypt.Crypter, *db.PrimaryCredentials, error) {
	outerCrypter, err := c.reCrypter(appPW, creds.OuterKeyParams)
	if err != nil {
		return nil, nil, fmt.Errorf("app password error: %w", err)
	}
	innerKey, err := outerCrypter.Decrypt(creds.EncInnerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("inner key decryption error: %w", err)
	}
	innerCrypter, err := c.reCrypter(innerKey, creds.InnerKeyParams)
	if err != nil {
		return nil, nil, fmt.Errorf("inner key deserialization error: %w", err)
	}
	seed, err := innerCrypter.Decrypt(creds.EncSeed)
	if err != nil {
		return nil, nil, fmt.Errorf("app seed decryption error: %w", err)
	}

	// Update all the fields.
	newInnerKey := seedInnerKey(seed)
	newInnerCrypter := c.newCrypter(newInnerKey[:])
	creds.Version = 1
	creds.InnerKeyParams = newInnerCrypter.Serialize()
	if creds.EncSeed, err = newInnerCrypter.Encrypt(seed); err != nil {
		return nil, nil, fmt.Errorf("error encrypting version 1 seed: %w", err)
	}
	if creds.EncInnerKey, err = outerCrypter.Encrypt(newInnerKey[:]); err != nil {
		return nil, nil, fmt.Errorf("error encrypting version 1 inner key: %w", err)
	}
	if err := c.recrypt(&creds, innerCrypter, newInnerCrypter); err != nil {
		return nil, nil, fmt.Errorf("recrypt error during v0 -> v1 credentials upgrade: %w", err)
	}

	c.log.Infof("Upgraded to version 1 credentials")
	return newInnerCrypter, &creds, nil
}

// connectWallets attempts to connect to and retrieve balance from all known
// wallets. This should be done only ONCE on Login.
func (c *Core) connectWallets(crypter encrypt.Crypter) {
	var wg sync.WaitGroup
	var connectCount uint32
	connectWallet := func(wallet *xcWallet) {
		defer wg.Done()
		// Return early if wallet is disabled.
		if wallet.isDisabled() {
			return
		}
		// Return early if this is a token and the parent wallet is disabled.
		if token := asset.TokenInfo(wallet.AssetID); token != nil {
			if parentWallet, found := c.wallet(token.ParentID); found && parentWallet.isDisabled() {
				return
			}
		}
		if !wallet.connected() {
			err := wallet.OpenWithPW(c.ctx, crypter)
			if err != nil {
				c.log.Errorf("Unable to open %s wallet: %v", unbip(wallet.AssetID), err)
				// TODO: Make an open wallet specific topic.
				subject, _ := c.formatDetails(TopicWalletConnectionWarning)
				c.notify(newWalletConfigNote(TopicWalletConnectionWarning, subject, err.Error(),
					db.ErrorLevel, wallet.state()))
				return
			}
			err = c.connectAndUpdateWallet(wallet)
			if err != nil {
				c.log.Errorf("Unable to connect to %s wallet (start and sync wallets BEFORE starting dex!): %v",
					unbip(wallet.AssetID), err)
				// NOTE: Details for this topic is in the context of fee
				// payment, but the subject pertains to a failure to connect
				// to the wallet.
				subject, _ := c.formatDetails(TopicWalletConnectionWarning)
				c.notify(newWalletConfigNote(TopicWalletConnectionWarning, subject, err.Error(),
					db.ErrorLevel, wallet.state()))
				return
			}
			if mw, is := wallet.Wallet.(asset.FundsMixer); is {
				startMixing := func() error {
					stats, err := mw.FundsMixingStats()
					if err != nil {
						return fmt.Errorf("error checking %s wallet mixing stats: %v", unbip(wallet.AssetID), err)
					}
					// If the wallet has no funds to transfer to the default account
					// and mixing is not enabled unlocking is not required.
					if !stats.Enabled && stats.MixedFunds == 0 && stats.TradingFunds == 0 {
						return nil
					}
					// Unlocking is required for mixing or to move funds if mixing
					// was recently turned off without funds being moved yet.
					if err := c.connectAndUnlock(crypter, wallet); err != nil {
						return fmt.Errorf("error unlocking %s wallet for mixing: %v", unbip(wallet.AssetID), err)
					}
					if err := mw.ConfigureFundsMixer(stats.Enabled); err != nil {
						return fmt.Errorf("error starting %s wallet mixing: %v", unbip(wallet.AssetID), err)
					}
					return nil
				}
				if err := startMixing(); err != nil {
					c.log.Errorf("Failed to start or stop mixing: %v", err)
				}
			}
			if c.cfg.UnlockCoinsOnLogin {
				if err = wallet.ReturnCoins(nil); err != nil {
					c.log.Errorf("Failed to unlock all %s wallet coins: %v", unbip(wallet.AssetID), err)
				}
			}
		}
		atomic.AddUint32(&connectCount, 1)
	}
	wallets := c.xcWallets()
	walletCount := len(wallets)
	var tokenWallets []*xcWallet

	for _, wallet := range wallets {
		if asset.TokenInfo(wallet.AssetID) != nil {
			tokenWallets = append(tokenWallets, wallet)
			continue
		}
		wg.Add(1)
		go connectWallet(wallet)
	}
	wg.Wait()

	for _, wallet := range tokenWallets {
		wg.Add(1)
		go connectWallet(wallet)
	}
	wg.Wait()

	if walletCount > 0 {
		c.log.Infof("Connected to %d of %d wallets.", connectCount, walletCount)
	}
}

// Notifications loads the latest notifications from the db.
func (c *Core) Notifications(n int) (notes, pokes []*db.Notification, _ error) {
	notes, err := c.db.NotificationsN(n)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting notifications: %w", err)
	}
	return notes, c.pokes(), nil
}

// pokes returns an empty slice; pokes were DEX server push notifications.
func (c *Core) pokes() []*db.Notification {
	return nil
}

func (c *Core) recrypt(creds *db.PrimaryCredentials, oldCrypter, newCrypter encrypt.Crypter) error {
	walletUpdates, acctUpdates, err := c.db.Recrypt(creds, oldCrypter, newCrypter)
	if err != nil {
		return err
	}

	c.setCredentials(creds)

	for assetID, newEncPW := range walletUpdates {
		w, found := c.wallet(assetID)
		if !found {
			c.log.Errorf("no wallet found for v1 upgrade asset ID %d", assetID)
			continue
		}
		w.setEncPW(newEncPW)
	}

	_ = acctUpdates // DEX connections removed; in-memory updates skipped

	return nil
}

// initializePrimaryCredentials sets the PrimaryCredential fields after the DB
// upgrade.
func (c *Core) initializePrimaryCredentials(pw []byte, oldKeyParams []byte) error {
	oldCrypter, err := c.reCrypter(pw, oldKeyParams)
	if err != nil {
		return fmt.Errorf("legacy encryption key deserialization error: %w", err)
	}

	newCrypter, creds, _, err := c.generateCredentials(pw, nil)
	if err != nil {
		return err
	}

	if err := c.recrypt(creds, oldCrypter, newCrypter); err != nil {
		return err
	}

	subject, details := c.formatDetails(TopicUpgradedToSeed)
	c.notify(newSecurityNote(TopicUpgradedToSeed, subject, details, db.WarningLevel))
	return nil
}

// Active indicates if there are any active orders. Always false in this build.
func (c *Core) Active() bool {
	return false
}

// Logout logs the user out
func (c *Core) Logout() error {
	c.loginMtx.Lock()
	defer c.loginMtx.Unlock()

	if !c.loggedIn {
		return nil
	}

	// Check active orders
	if c.Active() {
		return codedError(activeOrdersErr, ActiveOrdersLogoutErr)
	}

	// Lock wallets
	if !c.cfg.NoAutoWalletLock {
		// Ensure wallet lock in c.Run waits for c.Logout if this is called
		// before shutdown.
		c.wg.Add(1)
		for _, w := range c.xcWallets() {
			if w.connected() && w.unlocked() {
				if mw, is := w.Wallet.(asset.FundsMixer); is {
					if stats, err := mw.FundsMixingStats(); err == nil && stats.Enabled {
						c.log.Infof("Skipping lock for %s wallet (mixing active)", strings.ToUpper(unbip(w.AssetID)))
						continue
					}
				}
				symb := strings.ToUpper(unbip(w.AssetID))
				c.log.Infof("Locking %s wallet", symb)
				if err := w.Lock(walletLockTimeout); err != nil {
					// A failure to lock the wallet need not block the ability to
					// lock the DEX accounts or shutdown Core gracefully.
					c.log.Warnf("Unable to lock %v wallet: %v", unbip(w.AssetID), err)
				}
			}
		}
		c.wg.Done()
	}

	c.multisigXPriv.Zero()
	c.multisigXPriv = nil

	c.loggedIn = false

	return nil
}

// coreOrderFromMetaOrder creates an *Order from a *db.MetaOrder, including
// loading matches from the database. The order is presumed to be inactive, so
// swap coin confirmations will not be set. For active orders, get the
// *trackedTrade and use the coreOrder method.
func (c *Core) coreOrderFromMetaOrder(mOrd *db.MetaOrder) (*Order, error) {
	corder := coreOrderFromTrade(mOrd.Order, mOrd.MetaData)
	oid := mOrd.Order.ID()
	excludeCancels := false // maybe don't include cancel order matches?
	matches, err := c.db.MatchesForOrder(oid, excludeCancels)
	if err != nil {
		return nil, fmt.Errorf("MatchesForOrder error loading matches for %s: %w", oid, err)
	}
	corder.Matches = make([]*Match, 0, len(matches))
	for _, match := range matches {
		corder.Matches = append(corder.Matches, matchFromMetaMatch(mOrd.Order, match))
	}
	return corder, nil
}

// marketWallets is not available in this build (DEX connectivity removed).
func (c *Core) marketWallets(_ string, _, _ uint32) (ba, qa *dex.Asset, bw, qw *xcWallet, err error) {
	return nil, nil, nil, nil, fmt.Errorf("DEX connectivity not available in this build")
}

// resolveActiveTrades is a no-op in this build (DEX trading layer removed).
func (c *Core) resolveActiveTrades(_ encrypt.Crypter) {}

func (c *Core) wait(coinID []byte, assetID uint32, trigger func() (bool, error), action func(error)) {
	c.waiterMtx.Lock()
	defer c.waiterMtx.Unlock()
	c.blockWaiters[coinIDString(assetID, coinID)] = &blockWaiter{
		assetID: assetID,
		trigger: trigger,
		action:  action,
	}
}

func (c *Core) waiting(coinID []byte, assetID uint32) bool {
	c.waiterMtx.RLock()
	defer c.waiterMtx.RUnlock()
	_, found := c.blockWaiters[coinIDString(assetID, coinID)]
	return found
}

// removeWaiter removes a blockWaiter from the map.
func (c *Core) removeWaiter(id string) {
	c.waiterMtx.Lock()
	delete(c.blockWaiters, id)
	c.waiterMtx.Unlock()
}

// Send initiates either send or withdraw from an exchange wallet. if subtract
// is true, fees are subtracted from the value else fees are taken from the
// exchange wallet.
func (c *Core) Send(pw []byte, assetID uint32, value uint64, address string, subtract bool) (asset.Coin, error) {
	var crypter encrypt.Crypter
	// Empty password can be provided if wallet is already unlocked. Webserver
	// and RPCServer should not allow empty password, but this is used for
	// bots.
	if len(pw) > 0 {
		var err error
		crypter, err = c.encryptionKey(pw)
		if err != nil {
			return nil, fmt.Errorf("Trade password error: %w", err)
		}
		defer crypter.Close()
	}

	if value == 0 {
		return nil, fmt.Errorf("cannot send/withdraw zero %s", unbip(assetID))
	}
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}
	err := c.connectAndUnlock(crypter, wallet)
	if err != nil {
		return nil, err
	}

	if err = wallet.checkPeersAndSyncStatus(); err != nil {
		return nil, err
	}

	var feeSuggestion uint64
	if fr, is := wallet.Wallet.(asset.FeeRater); is {
		feeSuggestion = fr.FeeRate()
	}
	var coin asset.Coin
	if !subtract {
		coin, err = wallet.Wallet.Send(address, value, feeSuggestion)
	} else {
		if withdrawer, isWithdrawer := wallet.Wallet.(asset.Withdrawer); isWithdrawer {
			coin, err = withdrawer.Withdraw(address, value, feeSuggestion)
		} else {
			return nil, fmt.Errorf("wallet does not support subtracting network fee from withdraw amount")
		}
	}
	if err != nil {
		subject, details := c.formatDetails(TopicSendError, unbip(assetID), err)
		c.notify(newSendNote(TopicSendError, subject, details, db.ErrorLevel))
		return nil, err
	}

	sentValue := wallet.Info().UnitInfo.ConventionalString(coin.Value())
	subject, details := c.formatDetails(TopicSendSuccess, sentValue, unbip(assetID), address, coin)
	c.notify(newSendNote(TopicSendSuccess, subject, details, db.Success))

	c.updateAssetBalance(assetID)

	return coin, nil
}

// ValidateAddress checks that the provided address is valid.
func (c *Core) ValidateAddress(address string, assetID uint32) (bool, error) {
	if address == "" {
		return false, nil
	}
	wallet, found := c.wallet(assetID)
	if !found {
		return false, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}
	return wallet.Wallet.ValidateAddress(address), nil
}

// ApproveToken calls a wallet's ApproveToken method. It approves the version
// of the token used by the dex at the specified address.
func (c *Core) ApproveToken(appPW []byte, assetID uint32, dexAddr string, onConfirm func()) (string, error) {
	crypter, err := c.encryptionKey(appPW)
	if err != nil {
		return "", err
	}

	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return "", err
	}

	err = wallet.Unlock(crypter)
	if err != nil {
		return "", err
	}

	err = wallet.checkPeersAndSyncStatus()
	if err != nil {
		return "", err
	}

	walletOnConfirm := func() {
		go onConfirm()
		go c.notify(newTokenApprovalNote(wallet.state()))
	}

	txID, err := wallet.ApproveToken(0, walletOnConfirm)
	if err != nil {
		return "", err
	}

	c.notify(newTokenApprovalNote(wallet.state()))
	return txID, nil
}

// UnapproveToken calls a wallet's UnapproveToken method for a specified
// version of the token.
func (c *Core) UnapproveToken(appPW []byte, assetID uint32, version uint32) (string, error) {
	crypter, err := c.encryptionKey(appPW)
	if err != nil {
		return "", err
	}

	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return "", err
	}

	err = wallet.Unlock(crypter)
	if err != nil {
		return "", err
	}

	err = wallet.checkPeersAndSyncStatus()
	if err != nil {
		return "", err
	}

	onConfirm := func() {
		go c.notify(newTokenApprovalNote(wallet.state()))
	}

	txID, err := wallet.UnapproveToken(version, onConfirm)
	if err != nil {
		return "", err
	}

	c.notify(newTokenApprovalNote(wallet.state()))
	return txID, nil
}

// ApproveTokenFee returns the fee for a token approval/unapproval.
func (c *Core) ApproveTokenFee(assetID uint32, version uint32, approval bool) (uint64, error) {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return 0, err
	}

	return wallet.ApprovalFee(version, approval)
}

// BridgeContractApprovalStatus returns the approval status of the bridge
// contract for the specified asset.
func (c *Core) BridgeContractApprovalStatus(assetID uint32, bridgeName string) (asset.ApprovalStatus, error) {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return 0, err
	}

	return wallet.BridgeContractApprovalStatus(c.ctx, bridgeName)
}

// ApproveBridgeContract approves the bridge contract for the specified asset.
func (c *Core) ApproveBridgeContract(assetID uint32, bridgeName string) (string, error) {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return "", err
	}

	if !wallet.locallyUnlocked() {
		return "", fmt.Errorf("wallet %s must be unlocked", unbip(assetID))
	}

	err = wallet.checkPeersAndSyncStatus()
	if err != nil {
		return "", err
	}

	// Send notification when approval confirms
	onConfirm := func() {
		go c.notify(newBridgeApprovalNote(wallet.state()))
	}

	txID, err := wallet.ApproveBridgeContract(c.ctx, bridgeName, onConfirm)
	if err != nil {
		return "", err
	}

	// Send immediate notification (approval pending)
	c.notify(newBridgeApprovalNote(wallet.state()))
	return txID, nil
}

// UnapproveBridgeContract unapproves the bridge contract for the specified
// asset.
func (c *Core) UnapproveBridgeContract(assetID uint32, bridgeName string) (string, error) {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return "", err
	}

	if !wallet.locallyUnlocked() {
		return "", fmt.Errorf("wallet %s must be unlocked", unbip(assetID))
	}

	err = wallet.checkPeersAndSyncStatus()
	if err != nil {
		return "", err
	}

	// Send notification when unapproval confirms
	onConfirm := func() {
		go c.notify(newBridgeApprovalNote(wallet.state()))
	}

	txID, err := wallet.UnapproveBridgeContract(c.ctx, bridgeName, onConfirm)
	if err != nil {
		return "", err
	}

	// Send immediate notification (unapproval pending)
	c.notify(newBridgeApprovalNote(wallet.state()))
	return txID, nil
}

// DeployContract deploys a smart contract to one or more EVM chains.
func (c *Core) DeployContract(appPW []byte, assetIDs []uint32, txData []byte, contractVer *uint32, tokenAddress string) ([]*DeployContractResult, error) {
	_, err := c.encryptionKey(appPW)
	if err != nil {
		return nil, newError(authErr, "DeployContract password error: %w", err)
	}
	results := make([]*DeployContractResult, 0, len(assetIDs))
	for _, assetID := range assetIDs {
		res := &DeployContractResult{AssetID: assetID, Symbol: unbip(assetID)}
		wallet, err := c.connectedWallet(assetID)
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		deployer, ok := wallet.Wallet.(asset.ContractDeployer)
		if !ok {
			res.Error = "wallet does not support contract deployment"
			results = append(results, res)
			continue
		}
		deployData := txData
		if deployData == nil && contractVer != nil {
			deployData, err = deployer.BuildDeployTxData(*contractVer, tokenAddress)
			if err != nil {
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
		}
		contractAddr, txID, err := deployer.DeployContract(deployData)
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		res.ContractAddr = contractAddr
		res.TxID = txID
		results = append(results, res)
	}
	return results, nil
}

// TestContractGas exercises all v1 swap contract functions on the specified
// chains and tokens, measuring actual gas consumption.
func (c *Core) TestContractGas(appPW []byte, assetIDs []uint32, tokenAssetIDs []uint32, maxSwaps int) ([]*ContractGasTestResult, error) {
	_, err := c.encryptionKey(appPW)
	if err != nil {
		return nil, newError(authErr, "TestContractGas password error: %w", err)
	}

	// Group token asset IDs by their parent chain.
	tokensByParent := make(map[uint32][]uint32)
	for _, tokenID := range tokenAssetIDs {
		ti := asset.TokenInfo(tokenID)
		if ti == nil {
			return nil, newError(assetSupportErr, "unknown token asset ID %d", tokenID)
		}
		tokensByParent[ti.ParentID] = append(tokensByParent[ti.ParentID], tokenID)
	}

	// Build the set of chains to test. Explicitly listed chains run both
	// base and token tests. Chains inferred from tokens run tokens only.
	explicitChains := make(map[uint32]bool, len(assetIDs))
	for _, id := range assetIDs {
		explicitChains[id] = true
	}
	// Auto-add parent chains for tokens not already covered.
	for parentID := range tokensByParent {
		if !explicitChains[parentID] {
			assetIDs = append(assetIDs, parentID)
		}
	}

	var (
		results []*ContractGasTestResult
		mu      sync.Mutex
		wg      sync.WaitGroup
	)
	for _, assetID := range assetIDs {
		wg.Add(1)
		go func(assetID uint32) {
			defer wg.Done()
			var res []*ContractGasTestResult
			wallet, err := c.connectedWallet(assetID)
			if err != nil {
				res = []*ContractGasTestResult{{
					AssetID: assetID,
					Symbol:  unbip(assetID),
					Error:   err.Error(),
				}}
			} else if tester, ok := wallet.Wallet.(asset.ContractGasTester); !ok {
				res = []*ContractGasTestResult{{
					AssetID: assetID,
					Symbol:  unbip(assetID),
					Error:   "wallet does not support contract gas testing",
				}}
			} else {
				tokensForChain := tokensByParent[assetID]
				tokensOnly := !explicitChains[assetID]
				gasResults, err := tester.TestContractGas(1, maxSwaps, tokensForChain, tokensOnly)
				if err != nil {
					res = []*ContractGasTestResult{{
						AssetID: assetID,
						Symbol:  unbip(assetID),
						Error:   err.Error(),
					}}
				} else {
					res = gasResults
				}
			}
			mu.Lock()
			results = append(results, res...)
			mu.Unlock()
		}(assetID)
	}
	wg.Wait()
	return results, nil
}

// Bridge initiates a bridge.
func (c *Core) Bridge(fromAssetID, toAssetID uint32, amt uint64, bridgeName string) (txID string, err error) {
	// Connect and unlock the source wallet.
	sourceWallet, err := c.connectedWallet(fromAssetID)
	if err != nil {
		return "", err
	}
	if !sourceWallet.locallyUnlocked() {
		return "", fmt.Errorf("wallet %s must be unlocked", unbip(fromAssetID))
	}
	err = sourceWallet.checkPeersAndSyncStatus()
	if err != nil {
		return "", err
	}

	// Connect and unlock the destination wallet.
	destWallet, err := c.connectedWallet(toAssetID)
	if err != nil {
		return "", err
	}
	if !destWallet.locallyUnlocked() {
		return "", fmt.Errorf("wallet %s must be unlocked", unbip(toAssetID))
	}
	err = destWallet.checkPeersAndSyncStatus()
	if err != nil {
		return "", err
	}

	// Check destination wallet has enough for completion fees
	destBridger, ok := destWallet.Wallet.(asset.Bridger)
	if !ok {
		return "", fmt.Errorf("wallet %s is not a Bridger", unbip(toAssetID))
	}
	_, hasSufficientBalance, err := destBridger.BridgeCompletionFees(bridgeName)
	if err != nil {
		return "", fmt.Errorf("error getting completion fees: %w", err)
	}
	if !hasSufficientBalance {
		return "", fmt.Errorf("insufficient destination wallet balance for bridge completion fees")
	}

	return sourceWallet.InitiateBridge(c.ctx, amt, toAssetID, bridgeName)
}

// PendingBridges returns the pending bridges originating on the chain of the
// given asset ID.
func (c *Core) PendingBridges(assetID uint32) ([]*asset.WalletTransaction, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	return wallet.PendingBridges()
}

// BridgeHistory returns the bridge history for the given asset ID.
func (c *Core) BridgeHistory(assetID uint32, n int, refID *string, past bool) ([]*asset.WalletTransaction, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	return wallet.BridgeHistory(n, refID, past)
}

// SupportedBridgeDestinations returns the list of asset IDs that are supported
// as bridge destinations for the specified asset and the names of the bridges
// that can be used for each destination.
func (c *Core) SupportedBridgeDestinations(assetID uint32) (map[uint32][]string, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	bridger, ok := wallet.Wallet.(asset.Bridger)
	if !ok {
		return nil, fmt.Errorf("wallet for asset %s does not support bridging", unbip(assetID))
	}

	return bridger.SupportedDestinations(), nil
}

// BridgeFeesAndLimits returns the estimated fees and limits for bridging between two assets.
func (c *Core) BridgeFeesAndLimits(fromAssetID, toAssetID uint32, bridgeName string) (*BridgeFeesAndLimits, error) {
	sourceWallet, err := c.connectedWallet(fromAssetID)
	if err != nil {
		return nil, err
	}
	bridger, ok := sourceWallet.Wallet.(asset.Bridger)
	if !ok {
		return nil, fmt.Errorf("wallet for asset %s does not support bridging", unbip(fromAssetID))
	}

	destWallet, err := c.connectedWallet(toAssetID)
	if err != nil {
		return nil, err
	}
	destBridger, ok := destWallet.Wallet.(asset.Bridger)
	if !ok {
		return nil, fmt.Errorf("wallet for asset %s does not support bridging", unbip(toAssetID))
	}

	initiationFee, limits, hasLimits, err := bridger.BridgeInitiationFeesAndLimits(bridgeName, toAssetID)
	if err != nil {
		return nil, err
	}

	completionFee, _, err := destBridger.BridgeCompletionFees(bridgeName)
	if err != nil {
		return nil, err
	}

	fees := make(map[uint32]uint64, 2)

	if initiationFee > 0 {
		fromFeeAssetID := fromAssetID
		if token := asset.TokenInfo(fromAssetID); token != nil {
			fromFeeAssetID = token.ParentID
		}
		fees[fromFeeAssetID] += initiationFee
	}

	if completionFee > 0 {
		toFeeAssetID := toAssetID
		if token := asset.TokenInfo(toAssetID); token != nil {
			toFeeAssetID = token.ParentID
		}
		fees[toFeeAssetID] += completionFee
	}

	return &BridgeFeesAndLimits{
		Fees:      fees,
		MinLimit:  limits[0],
		MaxLimit:  limits[1],
		HasLimits: hasLimits,
	}, nil
}

// AllBridgePaths returns a map of all supported bridge paths.
// The return value maps source asset -> destination asset -> bridge names.
func (c *Core) AllBridgePaths() (map[uint32]map[uint32][]string, error) {
	paths := make(map[uint32]map[uint32][]string)

	c.walletMtx.RLock()
	defer c.walletMtx.RUnlock()

	for assetID, wallet := range c.wallets {
		bridger, ok := wallet.Wallet.(asset.Bridger)
		if !ok {
			continue
		}

		paths[assetID] = bridger.SupportedDestinations()
	}

	return paths, nil
}

// EstimateSendTxFee returns an estimate of the tx fee needed to send or
// withdraw the specified amount.
func (c *Core) EstimateSendTxFee(address string, assetID uint32, amount uint64, subtract, maxWithdraw bool) (fee uint64, isValidAddress bool, err error) {
	if amount == 0 {
		return 0, false, fmt.Errorf("cannot check fee for zero %s", unbip(assetID))
	}

	wallet, found := c.wallet(assetID)
	if !found {
		return 0, false, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	if !wallet.traits.IsTxFeeEstimator() {
		return 0, false, fmt.Errorf("wallet does not support fee estimation")
	}

	if subtract && !wallet.traits.IsWithdrawer() {
		return 0, false, fmt.Errorf("wallet does not support checking network fee for withdrawal")
	}
	estimator, is := wallet.Wallet.(asset.TxFeeEstimator)
	if !is {
		return 0, false, fmt.Errorf("wallet does not support fee estimation")
	}

	return estimator.EstimateSendTxFee(address, amount, 0, subtract, maxWithdraw)
}

// MultiTradeResult is returned from MultiTrade. Some orders may be placed
// successfully, while others may fail.
type MultiTradeResult struct {
	Order *Order
	Error error
}

// TxHistory returns all the transactions a wallet has made. If refID
// is nil, then transactions starting from the most recent are returned
// (past is ignored). If past is true, the transactions prior to the
// refID are returned, otherwise the transactions after the refID are
// returned. n is the number of transactions to return. If n is <= 0,
// all the transactions will be returned
func (c *Core) TxHistory(assetID uint32, req *asset.TxHistoryRequest) (*asset.TxHistoryResponse, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	return wallet.TxHistory(req)
}

// WalletTransaction returns information about a transaction that the wallet
// has made or one in which that wallet received funds. This function supports
// both transaction ID and coin ID.
func (c *Core) WalletTransaction(assetID uint32, txID string) (*asset.WalletTransaction, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	return wallet.WalletTransaction(c.ctx, txID)
}

// assetSet bundles a server's asset "config" for a pair of assets.
type assetSet struct {
	baseAsset  *dex.Asset
	quoteAsset *dex.Asset
	fromAsset  *dex.Asset
	toAsset    *dex.Asset
}

// AssetBalance retrieves and updates the current wallet balance.
func (c *Core) AssetBalance(assetID uint32) (*WalletBalance, error) {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return nil, fmt.Errorf("%d -> %s wallet error: %w", assetID, unbip(assetID), err)
	}
	return c.walletBalance(wallet)
}

func pluralize(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// initialize loads wallet configurations from the database.
func (c *Core) initialize() error {
	// Load wallet configurations. Actual connections are established on Login.
	dbWallets, err := c.db.Wallets()
	if err != nil {
		c.log.Errorf("error loading wallets from database: %v", err)
	}

	existingTokenWallets := make(map[uint32]bool)
	for _, dbWallet := range dbWallets {
		tkn := asset.TokenInfo(dbWallet.AssetID)
		if asset.Asset(dbWallet.AssetID) == nil && tkn == nil {
			c.log.Infof("Wallet for asset %s no longer supported", dex.BipIDSymbol(dbWallet.AssetID))
			continue
		}
		assetID := dbWallet.AssetID
		wallet, err := c.loadXCWallet(dbWallet)
		if err != nil {
			c.log.Errorf("error loading %d -> %s wallet: %v", assetID, unbip(assetID), err)
			continue
		}
		// Wallet is loaded from the DB, but not yet connected.
		c.log.Tracef("Loaded %s wallet configuration.", unbip(assetID))
		c.updateWallet(assetID, wallet)

		if tkn != nil {
			existingTokenWallets[dbWallet.AssetID] = true
		}
	}

	// Check for missing token wallets
	for _, dbWallet := range dbWallets {
		a := asset.Asset(dbWallet.AssetID)
		if a == nil {
			continue
		}
		for tokenID, tkn := range a.Tokens {
			if existingTokenWallets[tokenID] {
				continue
			}
			// Let's create the missing token wallet
			if err := c.initializeTokenWallet(tokenID, tkn); err != nil {
				c.log.Errorf("Couldn't create missing token wallet: %v", err)
			}
		}
	}

	return nil
}

// peerChange is called by a wallet backend when the peer count changes or
// cannot be determined. A wallet state note is always emitted. In addition to
// recording the number of peers, if the number of peers is 0, the wallet is
// flagged as not synced. If the number of peers has just dropped to zero, a
// notification that includes wallet state is emitted with the topic
// TopicWalletPeersWarning. If the number of peers is >0 and was previously
// zero, a resync monitor goroutine is launched to poll SyncStatus until the
// wallet has caught up with its network. The monitor goroutine will regularly
// emit wallet state notes, and once sync has been restored, a wallet balance
// note will be emitted. If peerChangeErr is non-nil, numPeers should be zero.
func (c *Core) peerChange(w *xcWallet, numPeers uint32, peerChangeErr error) {
	if peerChangeErr != nil {
		c.log.Warnf("%s wallet communication issue: %q", unbip(w.AssetID), peerChangeErr.Error())
	} else if numPeers == 0 {
		c.log.Warnf("Wallet for asset %s has zero network peers!", unbip(w.AssetID))
	} else {
		c.log.Tracef("New peer count for asset %s: %v", unbip(w.AssetID), numPeers)
	}

	ss, err := w.SyncStatus()
	if err != nil {
		c.log.Errorf("error getting sync status after peer change: %v", err)
		return
	}

	w.mtx.Lock()
	wasDisconnected := w.peerCount == 0 // excludes no count (-1)
	w.peerCount = int32(numPeers)
	w.syncStatus = ss
	w.mtx.Unlock()

	c.notify(newWalletConfigNote(TopicWalletPeersUpdate, "", "", db.Data, w.state()))

	// When we get peers after having none, start waiting for re-sync, otherwise
	// leave synced alone. This excludes the unknown state (-1) prior to the
	// initial peer count report.
	if wasDisconnected && numPeers > 0 {
		subject, details := c.formatDetails(TopicWalletPeersRestored, w.Info().Name)
		c.notify(newWalletConfigNote(TopicWalletPeersRestored, subject, details,
			db.Success, w.state()))
		c.startWalletSyncMonitor(w)
	} else if !ss.Synced {
		c.startWalletSyncMonitor(w)
	}

	// Send a WalletStateNote in case Synced or anything else has changed.
	if atomic.LoadUint32(w.broadcasting) == 1 {
		if (numPeers == 0 || peerChangeErr != nil) && !wasDisconnected { // was connected or initial report
			if peerChangeErr != nil {
				subject, details := c.formatDetails(TopicWalletCommsWarning,
					w.Info().Name, peerChangeErr.Error())
				c.notify(newWalletConfigNote(TopicWalletCommsWarning, subject, details,
					db.ErrorLevel, w.state()))
			} else {
				subject, details := c.formatDetails(TopicWalletPeersWarning, w.Info().Name)
				c.notify(newWalletConfigNote(TopicWalletPeersWarning, subject, details,
					db.WarningLevel, w.state()))
			}
		}
		c.notify(newWalletStateNote(w.state()))
	}
}

func (c *Core) handleBridgeReadyToComplete(n *asset.BridgeReadyToCompleteNote) {
	destWallet, err := c.connectedWallet(n.DestAssetID)
	if err != nil {
		c.log.Errorf("Bridge %s funds are ready to complete, but wallet unable to connect.", unbip(n.DestAssetID))
		return
	}

	bridgeTx := &asset.BridgeCounterpartTx{
		AssetID: n.AssetID,
		IDs:     []string{n.InitiateBridgeTxID},
	}

	err = destWallet.CompleteBridge(c.ctx, bridgeTx, n.Amount, n.Data, n.BridgeName)
	if err != nil {
		c.log.Errorf("Error completing bridge: %v", err)
	}
}

func (c *Core) handleBridgeCompleted(n *asset.BridgeCompletedNote) {
	sourceWallet, err := c.connectedWallet(n.SourceAssetID)
	if err != nil {
		c.log.Errorf("Bridge %s funds are completed, but wallet unable to connect.", unbip(n.SourceAssetID))
		return
	}

	sourceWallet.MarkBridgeComplete(n.InitiationTxID, n.CompletionTxIDs, n.AmtReceived, n.Fees, n.Complete)

	c.notify(newBridgeNote(n))
}

// handleWalletNotification processes an asynchronous wallet notification.
func (c *Core) handleWalletNotification(ni asset.WalletNotification) {
	switch n := ni.(type) {
	case *asset.TipChangeNote:
		c.queueTipChange(n.AssetID, n.Tip)
	case *asset.BalanceChangeNote:
		c.queueBalanceChange(n.AssetID, n.Balance)
		return // Notification sent by runBalanceChange.
	case *asset.ActionRequiredNote:
		c.requestedActionMtx.Lock()
		c.requestedActions[n.UniqueID] = n
		c.requestedActionMtx.Unlock()
	case *asset.ActionResolvedNote:
		c.deleteRequestedAction(n.UniqueID)
	case *asset.BridgeReadyToCompleteNote:
		c.handleBridgeReadyToComplete(n)
	case *asset.BridgeCompletedNote:
		c.handleBridgeCompleted(n)
	case *asset.TransactionNote:
		w, ok := c.wallet(n.AssetID)
		if !ok {
			return
		}
		w.processWalletTransactions([]*asset.WalletTransaction{n.Transaction})
	}
	c.notify(newWalletNote(ni))
}

// queueTipChange records the latest tip for the asset and ensures a tipChange
// goroutine is running. If one is already active, the pending tip is stored and
// picked up when the current run finishes, coalescing rapid-fire notifications.
func (c *Core) queueTipChange(assetID uint32, tip uint64) {
	c.tipMtx.Lock()
	c.tipPending[assetID] = tip
	if c.tipActive[assetID] {
		c.tipMtx.Unlock()
		return
	}
	c.tipActive[assetID] = true
	c.tipMtx.Unlock()
	c.wg.Add(1)
	go c.runTipChange(assetID)
}

// runTipChange drains pending tips for the asset, calling tipChange for each.
// When no more pending tips exist, marks the asset as inactive and returns.
func (c *Core) runTipChange(assetID uint32) {
	defer c.wg.Done()
	for {
		c.tipMtx.Lock()
		tip, ok := c.tipPending[assetID]
		if !ok {
			c.tipActive[assetID] = false
			c.tipMtx.Unlock()
			return
		}
		delete(c.tipPending, assetID)
		c.tipMtx.Unlock()
		c.tipChange(assetID, tip)
	}
}

// queueBalanceChange records the latest balance for the asset and ensures a
// balance processing goroutine is running. If one is already active, the
// pending balance is stored and picked up when the current run finishes,
// coalescing rapid-fire balance notifications.
func (c *Core) queueBalanceChange(assetID uint32, bal *asset.Balance) {
	c.balMtx.Lock()
	c.balPending[assetID] = bal
	if c.balActive[assetID] {
		c.balMtx.Unlock()
		return
	}
	c.balActive[assetID] = true
	c.balMtx.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.runBalanceChange(assetID)
	}()
}

// runBalanceChange drains pending balance changes for the asset. When no more
// pending balances exist, marks the asset as inactive and returns.
func (c *Core) runBalanceChange(assetID uint32) {
	for {
		c.balMtx.Lock()
		bal, ok := c.balPending[assetID]
		if !ok {
			c.balActive[assetID] = false
			c.balMtx.Unlock()
			return
		}
		delete(c.balPending, assetID)
		c.balMtx.Unlock()

		w, ok := c.wallet(assetID)
		if !ok {
			continue
		}
		walBal := &WalletBalance{
			Balance: &db.Balance{
				Balance: *bal,
				Stamp:   time.Now(),
			},
		}
		if err := c.storeAndSendWalletBalance(w, walBal); err != nil {
			c.log.Errorf("Error storing and sending emitted balance: %v", err)
		}
	}
}

// tipChange is called by a wallet backend when the tip block changes, or when
// a connection error is encountered such that tip change reporting may be
// adversely affected.
func (c *Core) tipChange(assetID uint32, tip uint64) {
	c.log.Tracef("Processing tip change for %s", unbip(assetID))
	c.waiterMtx.RLock()
	for id, waiter := range c.blockWaiters {
		if waiter.assetID != assetID {
			continue
		}
		go func(id string, waiter *blockWaiter) {
			ok, err := waiter.trigger()
			if err != nil {
				waiter.action(err)
				c.removeWaiter(id)
				return
			}
			if ok {
				waiter.action(nil)
				c.removeWaiter(id)
			}
		}(id, waiter)
	}
	c.waiterMtx.RUnlock()

	w, found := c.wallet(assetID)
	if found {
		w.mtx.Lock()
		ss := *w.syncStatus
		ss.Blocks = tip
		w.syncStatus = &ss
		w.mtx.Unlock()
	}

	if _, exists := c.wallet(assetID); exists {
		c.updateBalances(assetMap{assetID: {}})
	}
}

// WalletLogFilePath returns the path to the wallet's log file.
func (c *Core) WalletLogFilePath(assetID uint32) (string, error) {
	wallet, exists := c.wallet(assetID)
	if !exists {
		return "", newError(missingWalletErr, "no configured wallet found for %s (%d)",
			strings.ToUpper(unbip(assetID)), assetID)
	}

	return wallet.logFilePath()
}

// WalletRestorationInfo returns information about how to restore the currently
// loaded wallet for assetID in various external wallet software. This function
// will return an error if the currently loaded wallet for assetID does not
// implement the WalletRestorer interface.
func (c *Core) WalletRestorationInfo(pw []byte, assetID uint32) ([]*asset.WalletRestoration, error) {
	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return nil, fmt.Errorf("WalletRestorationInfo password error: %w", err)
	}
	defer crypter.Close()

	seed, _, err := c.assetSeedAndPass(assetID, crypter)
	if err != nil {
		return nil, fmt.Errorf("assetSeedAndPass error: %w", err)
	}
	defer encode.ClearBytes(seed)

	wallet, found := c.wallet(assetID)
	if !found {
		return nil, fmt.Errorf("no wallet configured for asset %d", assetID)
	}

	restorer, ok := wallet.Wallet.(asset.WalletRestorer)
	if !ok {
		return nil, fmt.Errorf("wallet for asset %d doesn't support exporting functionality", assetID)
	}

	restorationInfo, err := restorer.RestorationInfo(seed)
	if err != nil {
		return nil, fmt.Errorf("failed to get restoration info for wallet %w", err)
	}

	return restorationInfo, nil
}

// createFile creates a new file and will create the file directory if it does
// not exist.
func createFile(fileName string) (*os.File, error) {
	if fileName == "" {
		return nil, errors.New("no file path specified for creating")
	}
	fileDir := filepath.Dir(fileName)
	if !dex.FileExists(fileDir) {
		err := os.MkdirAll(fileDir, 0755)
		if err != nil {
			return nil, fmt.Errorf("os.MkdirAll error: %w", err)
		}
	}
	fileName = dex.CleanAndExpandPath(fileName)
	// Errors if file exists.
	f, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// WalletPeers returns a list of peers that a wallet is connected to. It also
// returns the user added peers that the wallet is not connected to.
func (c *Core) WalletPeers(assetID uint32) ([]*asset.WalletPeer, error) {
	w, err := c.connectedWallet(assetID)
	if err != nil {
		return nil, err
	}

	peerManager, is := w.Wallet.(asset.PeerManager)
	if !is {
		return nil, fmt.Errorf("%s wallet is not a peer manager", unbip(assetID))
	}

	return peerManager.Peers()
}

// AddWalletPeer connects the wallet to a new peer, and also persists this peer
// to be connected to on future startups.
func (c *Core) AddWalletPeer(assetID uint32, address string) error {
	w, err := c.connectedWallet(assetID)
	if err != nil {
		return err
	}

	peerManager, is := w.Wallet.(asset.PeerManager)
	if !is {
		return fmt.Errorf("%s wallet is not a peer manager", unbip(assetID))
	}

	return peerManager.AddPeer(address)
}

// RemoveWalletPeer disconnects from a peer that the user previously added. It
// will no longer be guaranteed to connect to this peer in the future.
func (c *Core) RemoveWalletPeer(assetID uint32, address string) error {
	w, err := c.connectedWallet(assetID)
	if err != nil {
		return err
	}

	peerManager, is := w.Wallet.(asset.PeerManager)
	if !is {
		return fmt.Errorf("%s wallet is not a peer manager", unbip(assetID))
	}

	return peerManager.RemovePeer(address)
}

// fetchFiatExchangeRates starts the fiat rate fetcher goroutine and schedules
// refresh cycles. Use under ratesMtx lock.
func (c *Core) fetchFiatExchangeRates(ctx context.Context) {
	c.log.Debug("starting fiat rate fetching")

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		tick := time.NewTicker(fiatRateRequestInterval)
		defer tick.Stop()
		for {
			c.refreshFiatRates(ctx)

			select {
			case <-tick.C:
			case <-c.reFiat:
			case <-ctx.Done():
				return

			}
		}
	}()
}

func (c *Core) fiatSources() []*commonRateSource {
	c.ratesMtx.RLock()
	defer c.ratesMtx.RUnlock()
	sources := make([]*commonRateSource, 0, len(c.fiatRateSources))
	for _, s := range c.fiatRateSources {
		sources = append(sources, s)
	}
	return sources
}

// refreshFiatRates refreshes the fiat rates for rate sources whose values have
// not been updated since fiatRateRequestInterval. It also checks if fiat rates
// are expired and does some clean-up.
func (c *Core) refreshFiatRates(ctx context.Context) {
	var wg sync.WaitGroup
	supportedAssets := c.SupportedAssets()
	for _, source := range c.fiatSources() {
		wg.Add(1)
		go func(source *commonRateSource) {
			defer wg.Done()
			source.refreshRates(ctx, c.log, supportedAssets)
		}(source)
	}
	wg.Wait()

	// Remove expired rate source if any.
	c.removeExpiredRateSources()

	fiatRatesMap := c.fiatConversions()
	if len(fiatRatesMap) != 0 {
		c.notify(newFiatRatesUpdate(fiatRatesMap))
	}
}

// FiatRateSources returns a list of fiat rate sources and their individual
// status.
func (c *Core) FiatRateSources() map[string]bool {
	c.ratesMtx.RLock()
	defer c.ratesMtx.RUnlock()
	rateSources := make(map[string]bool, len(fiatRateFetchers))
	for token := range fiatRateFetchers {
		rateSources[token] = c.fiatRateSources[token] != nil
	}
	return rateSources
}

// FiatConversionRates are the currently cached fiat conversion rates. Must have
// 1 or more fiat rate sources enabled.
func (c *Core) FiatConversionRates() map[uint32]float64 {
	return c.fiatConversions()
}

// fiatConversions returns fiat rate for all supported assets that have a
// wallet.
func (c *Core) fiatConversions() map[uint32]float64 {
	assetIDs := make(map[uint32]struct{})
	supportedAssets := asset.Assets()
	for assetID, asset := range supportedAssets {
		assetIDs[assetID] = struct{}{}
		for tokenID := range asset.Tokens {
			assetIDs[tokenID] = struct{}{}
		}
	}

	fiatRatesMap := make(map[uint32]float64, len(supportedAssets))
	for assetID := range assetIDs {
		var rateSum float64
		var sources int
		for _, source := range c.fiatSources() {
			// Adjust a couple custom bip ids to get the eth equivalent.
			getAssetID := assetID
			switch assetID {
			case 8453:
				// base -> eth
				getAssetID = 60
			}
			rateInfo := source.assetRate(getAssetID)
			if rateInfo != nil && time.Since(rateInfo.lastUpdate) < fiatRateDataExpiry && rateInfo.rate > 0 {
				sources++
				rateSum += rateInfo.rate
			}
		}
		if rateSum != 0 {
			fiatRatesMap[assetID] = rateSum / float64(sources) // get average rate.
		}
	}
	return fiatRatesMap
}

// ToggleRateSourceStatus toggles a fiat rate source status. If disable is true,
// the fiat rate source is disabled, otherwise the rate source is enabled.
func (c *Core) ToggleRateSourceStatus(source string, disable bool) error {
	if disable {
		return c.disableRateSource(source)
	}
	return c.enableRateSource(source)
}

// enableRateSource enables a fiat rate source.
func (c *Core) enableRateSource(source string) error {
	// Check if it's an invalid rate source or it is already enabled.
	rateFetcher, found := fiatRateFetchers[source]
	if !found {
		return errors.New("cannot enable unknown fiat rate source")
	}

	c.ratesMtx.Lock()
	defer c.ratesMtx.Unlock()
	if c.fiatRateSources[source] != nil {
		return nil // already enabled.
	}

	// Build fiat rate source.
	rateSource := newCommonRateSource(rateFetcher)
	c.fiatRateSources[source] = rateSource

	select {
	case c.reFiat <- struct{}{}:
	default:
	}

	// Update disabled fiat rate source.
	c.saveDisabledRateSources()

	c.log.Infof("Enabled %s to fetch fiat rates.", source)
	return nil
}

// disableRateSource disables a fiat rate source.
func (c *Core) disableRateSource(source string) error {
	// Check if it's an invalid fiat rate source or it is already
	// disabled.
	_, found := fiatRateFetchers[source]
	if !found {
		return errors.New("cannot disable unknown fiat rate source")
	}

	c.ratesMtx.Lock()
	defer c.ratesMtx.Unlock()

	if c.fiatRateSources[source] == nil {
		return nil // already disabled.
	}

	// Remove fiat rate source.
	delete(c.fiatRateSources, source)

	// Save disabled fiat rate sources to database.
	c.saveDisabledRateSources()

	c.log.Infof("Disabled %s from fetching fiat rates.", source)
	return nil
}

// removeExpiredRateSources disables expired fiat rate source.
func (c *Core) removeExpiredRateSources() {
	c.ratesMtx.Lock()
	defer c.ratesMtx.Unlock()

	// Remove fiat rate source with expired exchange rate data.
	var disabledSources []string
	for token, source := range c.fiatRateSources {
		if source.isExpired(fiatRateDataExpiry) {
			delete(c.fiatRateSources, token)
			disabledSources = append(disabledSources, token)
		}
	}

	// Ensure disabled fiat rate fetchers are saved to database.
	if len(disabledSources) > 0 {
		c.saveDisabledRateSources()
		c.log.Warnf("Expired rate source(s) has been disabled: %v", strings.Join(disabledSources, ", "))
	}
}

// saveDisabledRateSources saves disabled fiat rate sources to database and
// shuts down rate fetching if there are no exchange rate source. Use under
// ratesMtx lock.
func (c *Core) saveDisabledRateSources() {
	var disabled []string
	for token := range fiatRateFetchers {
		if c.fiatRateSources[token] == nil {
			disabled = append(disabled, token)
		}
	}

	err := c.db.SaveDisabledRateSources(disabled)
	if err != nil {
		c.log.Errorf("Unable to save disabled fiat rate source to database: %v", err)
	}
}

// stakingWallet fetches the staking wallet and returns its asset.TicketBuyer
// interface. Errors if no wallet is currently loaded. Used for ticket
// purchasing.
func (c *Core) stakingWallet(assetID uint32) (*xcWallet, asset.TicketBuyer, error) {
	wallet, exists := c.wallet(assetID)
	if !exists {
		return nil, nil, newError(missingWalletErr, "no configured wallet found for %s", unbip(assetID))
	}
	ticketBuyer, is := wallet.Wallet.(asset.TicketBuyer)
	if !is {
		return nil, nil, fmt.Errorf("%s wallet is not a TicketBuyer", unbip(assetID))
	}
	return wallet, ticketBuyer, nil
}

// politeiaVoter returns the PoliteiaVoter interface for the given asset ID.
// Defaults to DCR (asset ID 42) if no asset ID is provided.
func (c *Core) politeiaVoter(assetID ...uint32) (asset.PoliteiaVoter, error) {
	id := uint32(42) // DCR BipID
	if len(assetID) > 0 {
		id = assetID[0]
	}
	wallet, exists := c.wallet(id)
	if !exists {
		return nil, fmt.Errorf("no wallet for %s", unbip(id))
	}
	if !wallet.traits.IsPoliteiaVoter() {
		return nil, fmt.Errorf("%s wallet is not a PoliteiaVoter", unbip(id))
	}
	pv, ok := wallet.Wallet.(asset.PoliteiaVoter)
	if !ok {
		return nil, fmt.Errorf("%s wallet is not a PoliteiaVoter", unbip(id))
	}
	return pv, nil
}

// StakeStatus returns current staking statuses such as currently owned
// tickets, ticket price, and current voting preferences. Used for
// ticket purchasing.
func (c *Core) StakeStatus(assetID uint32) (*asset.TicketStakingStatus, error) {
	_, tb, err := c.stakingWallet(assetID)
	if err != nil {
		return nil, err
	}
	return tb.StakeStatus()
}

// SetVSP sets the VSP provider. Used for ticket purchasing.
func (c *Core) SetVSP(assetID uint32, addr string) error {
	_, tb, err := c.stakingWallet(assetID)
	if err != nil {
		return err
	}
	return tb.SetVSP(addr)
}

// PurchaseTickets purchases n tickets. Returns the purchased ticket hashes if
// successful. Used for ticket purchasing.
func (c *Core) PurchaseTickets(assetID uint32, pw []byte, n int) error {
	wallet, tb, err := c.stakingWallet(assetID)
	if err != nil {
		return err
	}
	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return fmt.Errorf("password error: %w", err)
	}
	defer crypter.Close()

	if err = c.connectAndUnlock(crypter, wallet); err != nil {
		return err
	}

	if err = tb.PurchaseTickets(n, 0); err != nil {
		return err
	}
	c.updateAssetBalance(assetID)
	// TODO: Send tickets bought notification.
	//subject, details := c.formatDetails(TopicSendSuccess, sentValue, unbip(assetID), address, coin)
	//c.notify(newSendNote(TopicSendSuccess, subject, details, db.Success))
	return nil
}

// SetVotingPreferences sets default voting settings for all active tickets and
// future tickets. Nil maps can be provided for no change. Used for ticket
// purchasing.
func (c *Core) SetVotingPreferences(assetID uint32, choices, tSpendPolicy,
	treasuryPolicy map[string]string) error {
	_, tb, err := c.stakingWallet(assetID)
	if err != nil {
		return err
	}
	return tb.SetVotingPreferences(choices, tSpendPolicy, treasuryPolicy)
}

// ListVSPs lists known available voting service providers.
func (c *Core) ListVSPs(assetID uint32) ([]*asset.VotingServiceProvider, error) {
	_, tb, err := c.stakingWallet(assetID)
	if err != nil {
		return nil, err
	}
	return tb.ListVSPs()
}

// TicketPage fetches a page of TicketBuyer tickets within a range of block
// numbers with a target page size and optional offset. scanStart it the block
// in which to start the scan. The scan progresses in reverse block number
// order, starting at scanStart and going to progressively lower blocks.
// scanStart can be set to -1 to indicate the current chain tip.
func (c *Core) TicketPage(assetID uint32, scanStart int32, n, skipN int) ([]*asset.Ticket, error) {
	_, tb, err := c.stakingWallet(assetID)
	if err != nil {
		return nil, err
	}
	return tb.TicketPage(scanStart, n, skipN)
}

func (c *Core) mixingWallet(assetID uint32) (*xcWallet, asset.FundsMixer, error) {
	w, known := c.wallet(assetID)
	if !known {
		return nil, nil, fmt.Errorf("unknown wallet %d", assetID)
	}
	mw, is := w.Wallet.(asset.FundsMixer)
	if !is {
		return nil, nil, fmt.Errorf("%s wallet is not a FundsMixer", w.Info().Name)
	}
	return w, mw, nil
}

// FundsMixingStats returns the current state of the wallet's funds mixer.
func (c *Core) FundsMixingStats(assetID uint32) (*asset.FundsMixingStats, error) {
	_, mw, err := c.mixingWallet(assetID)
	if err != nil {
		return nil, err
	}
	return mw.FundsMixingStats()
}

// ConfigureFundsMixer configures the wallet for funds mixing.
func (c *Core) ConfigureFundsMixer(pw []byte, assetID uint32, isMixerEnabled bool) error {
	wallet, mw, err := c.mixingWallet(assetID)
	if err != nil {
		return err
	}
	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return fmt.Errorf("mixing password error: %w", err)
	}
	defer crypter.Close()
	if err := c.connectAndUnlock(crypter, wallet); err != nil {
		return err
	}
	return mw.ConfigureFundsMixer(isMixerEnabled)
}

// NetworkFeeRate returns the network fee rate for the specified asset.
// Falls back to the wallet's fee rate estimate if available, otherwise 0.
func (c *Core) NetworkFeeRate(assetID uint32) uint64 {
	w, found := c.wallet(assetID)
	if !found {
		return 0
	}
	if fr, is := w.Wallet.(asset.FeeRater); is {
		return fr.FeeRate()
	}
	return 0
}

func (c *Core) deleteRequestedAction(uniqueID string) {
	c.requestedActionMtx.Lock()
	delete(c.requestedActions, uniqueID)
	c.requestedActionMtx.Unlock()
}

// handleCoreAction checks if the actionID is a known core action, and if so
// attempts to take the action requested.
func (c *Core) handleCoreAction(_ string, _ json.RawMessage) ( /* handled */ bool, error) {
	return false, nil
}

// TakeAction is called in response to a ActionRequiredNote. The note may have
// come from core or from a wallet.
func (c *Core) TakeAction(assetID uint32, actionID string, actionB json.RawMessage) (err error) {
	defer func() {
		if err != nil {
			c.log.Errorf("Error while attempting user action %q with parameters %q, asset ID %d: %v",
				actionID, string(actionB), assetID, err)
		} else {
			c.log.Infof("User completed action %q with parameters %q, asset ID %d",
				actionID, string(actionB), assetID)
		}
	}()
	if handled, err := c.handleCoreAction(actionID, actionB); handled {
		return err
	}
	w, err := c.connectedWallet(assetID)
	if err != nil {
		return err
	}
	goGetter, is := w.Wallet.(asset.ActionTaker)
	if !is {
		return fmt.Errorf("wallet for %s cannot handle user actions", w.Symbol)
	}
	return goGetter.TakeAction(actionID, actionB)
}

// GenerateBCHRecoveryTransaction generates a tx that spends all inputs from the
// deprecated BCH wallet to the given recipient.
func (c *Core) GenerateBCHRecoveryTransaction(appPW []byte, recipient string) ([]byte, error) {
	const bipID = 145
	crypter, err := c.encryptionKey(appPW)
	if err != nil {
		return nil, err
	}
	_, walletPW, err := c.assetSeedAndPass(bipID, crypter)
	if err != nil {
		return nil, err
	}
	return asset.SPVWithdrawTx(c.ctx, bipID, walletPW, recipient, c.assetDataDirectory(bipID), c.net, c.log.SubLogger("BCH"))
}

// ExtensionModeConfig is the configuration parsed from the extension-mode file.
func (c *Core) ExtensionModeConfig() *ExtensionModeConfig {
	return c.extensionModeConfig
}

