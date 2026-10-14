// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"context"
	"encoding/binary"
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

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/config"
	"github.com/bisoncraft/caribou/util/dexnet"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/encrypt"
	"github.com/bisoncraft/caribou/wallet/asset"
	"github.com/bisoncraft/caribou/wallet/db"
	"github.com/bisoncraft/caribou/wallet/mnemonic"
	"github.com/bisoncraft/caribou/wallet/txdb"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/hdkeychain/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const (
	// walletLockTimeout is the default timeout used when locking wallets.
	walletLockTimeout = 5 * time.Second
)

var (
	unbip = util.BipIDSymbol
	// When waiting for a wallet to sync, a SyncStatus check will be performed
	// every syncTickerPeriod. var instead of const for testing purposes.
	syncTickerPeriod = 3 * time.Second
	// walletDisabledErrStr is the error message returned when trying to use a
	// disabled wallet.
	walletDisabledErrStr = "%s wallet is disabled"

	errTimeout = errors.New("timeout")
)

// Config is the configuration for the Core.
type Config struct {
	// DBPath is a filepath to use for the client database. If the database does
	// not already exist, it will be created.
	DBPath string
	// Net is the current network.
	Net util.Network
	// Logger is the Core's logger and is also used to create the sub-loggers
	// for the asset backends.
	Logger util.Logger
	// Onion is the address (host:port) of a Tor proxy for use with .onion
	// addresses. To use Tor with regular addresses as well, set TorProxy.
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
	// ExtensionModeFile is the path to a file that specifies configuration
	// for running core in extension mode, which gives the caller options for
	// e.g. limiting the ability to configure wallets.
	ExtensionModeFile string
	// IntentsBaseURL overrides the NEAR Intents 1Click API base URL, for
	// dev servers such as the simnet mock (util/testing/near-intents).
	// Ignored on mainnet: an injected base URL would bypass the pinned
	// quote-signing key and redirect deposits.
	IntentsBaseURL string
	// IntentsSigningKey overrides the pinned 1Click quote-signing public
	// key (base58, optional "ed25519:" prefix). Ignored on mainnet.
	IntentsSigningKey string
}

// locale is data associated with the currently selected language.
type locale struct {
	lang    language.Tag
	m       map[Topic]*translation
	printer *message.Printer
}

// Core is the core client application. Core manages wallets,
// database access, atomic swap settlement and more.
type Core struct {
	ctx   context.Context
	wg    sync.WaitGroup
	ready chan struct{}
	cfg   *Config
	log   util.Logger
	db    db.DB
	txDB  *txdb.TxDB
	net   util.Network
	intl  atomic.Value // *locale

	extensionModeConfig *ExtensionModeConfig

	// construction or init sets credentials
	credMtx     sync.RWMutex
	credentials *db.PrimaryCredentials

	loginMtx      sync.Mutex
	loggedIn      bool
	multisigXPriv *hdkeychain.ExtendedKey // derived from creds.EncSeed on login

	// suspendMtx guards the mobile pause/resume state. Global lock order is
	// suspendMtx before loginMtx. See Suspend/Resume.
	suspendMtx       sync.Mutex
	suspended        bool
	suspendedWallets []uint32
	// suspendGen counts session-lifecycle transitions that invalidate an
	// in-flight Resume: a Suspend or a Logout. Resume snapshots it, drops
	// suspendMtx for the reconnects (which are network-bound and must not
	// stall a concurrent Logout or Stop), and re-checks it after each one. A
	// bump means the wallet it just reconnected is no longer wanted.
	suspendGen uint64
	// loginConnecting spans a fresh Login from the moment it publishes loggedIn
	// to the end of its connectWallets. Every wallet is legitimately down for
	// that whole window, so connection state cannot be read as "a newer session
	// never reached this one" - see rearmUnreached, which bows out while it is
	// set. Login claims it, connectWallets clears it.
	loginConnecting bool

	seedGenerationTime uint64

	newCrypter func([]byte) encrypt.Crypter
	reCrypter  func([]byte, []byte) (encrypt.Crypter, error)

	// taskMtx guards shuttingDown, which gates registration of ad-hoc work on
	// c.wg. An unsynchronized check of c.ctx before c.wg.Add races the Wait in
	// Run: the counter can legitimately reach zero and Wait return between the
	// check and the Add, which panics with "sync: WaitGroup misuse". Publishing
	// the flag under taskMtx before Run's notification goroutine drops the last
	// count makes the check and the Add atomic with respect to that Wait. See
	// beginTask.
	taskMtx      sync.Mutex
	shuttingDown bool

	walletMtx sync.RWMutex
	wallets   map[uint32]*xcWallet
	// creating holds the assets with a wallet creation in flight. See
	// claimWalletCreation.
	creating map[uint32]bool

	tipMtx     sync.Mutex
	tipPending map[uint32]uint64 // assetID -> latest pending tip height
	tipActive  map[uint32]bool   // assetID -> whether tipChange is running

	balMtx     sync.Mutex
	balPending map[uint32]*asset.Balance // assetID -> latest pending balance
	balActive  map[uint32]bool           // assetID -> whether balance processing is running

	noteMtx   sync.RWMutex
	noteChans map[uint64]chan Notification

	ratesMtx        sync.RWMutex
	fiatRateSources map[string]*commonRateSource
	// autoDisabledRateSources maps a rate source to the time it was removed for
	// serving only expired data. Removal is a reaction to an outage rather than
	// a user preference, so these entries are never persisted and are retried
	// after fiatRateSourceRetryInterval. Guarded by ratesMtx.
	autoDisabledRateSources map[string]time.Time

	reFiat chan struct{}

	notes chan asset.WalletNotification

	requestedActionMtx sync.RWMutex
	requestedActions   map[string]*asset.ActionRequiredNote

	// intentsEng holds the *intentsEngine for cross-chain swaps. All
	// engine logic lives in near-intents.go.
	intentsEng atomic.Value
}

// New is the constructor for a new Core.
func New(cfg *Config) (*Core, error) {
	if cfg.Logger == nil {
		return nil, fmt.Errorf("Core.Config must specify a Logger")
	}
	coreDB, err := db.New(cfg.DBPath, cfg.Logger.SubLogger("DB"))
	if err != nil {
		return nil, fmt.Errorf("database initialization error: %w", err)
	}

	// Both stores hold an exclusive engine lock from the moment they open, so
	// every error return below must close whatever has opened by that point.
	var masterTxDB *txdb.TxDB
	constructed := false
	defer func() {
		if constructed {
			return
		}
		if cerr := coreDB.Close(); cerr != nil {
			cfg.Logger.Errorf("Error closing database after failed construction: %v", cerr)
		}
		if masterTxDB == nil {
			return
		}
		if cerr := masterTxDB.Close(); cerr != nil {
			cfg.Logger.Errorf("Error closing transaction database after failed construction: %v", cerr)
		}
	}()

	// The master transaction database lives beside the main database. It is
	// shared by all wallets, each of which manages its own transactions through
	// a network-scoped NetworkTxDB handle.
	txDBPath := filepath.Join(filepath.Dir(cfg.DBPath), "tx.db")
	txDBLog := cfg.Logger.SubLogger("TXDB")
	masterTxDB, err = txdb.New(txDBPath, txDBLog)
	if err != nil {
		return nil, fmt.Errorf("transaction database initialization error: %w", err)
	}
	// The txdb is asset-agnostic and cannot group transactions by ticker on its
	// own; supply the mapping from asset metadata. Must be set before any wallet
	// connects and begins storing transactions.
	masterTxDB.SetTickerResolver(func(assetID uint32) string {
		return assetTickerGroup(assetID, txDBLog)
	})
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
	if langStr, err := coreDB.Language(); err != nil {
		cfg.Logger.Errorf("Error loading language from database: %v", err)
	} else if len(langStr) > 0 {
		if lang, err = parseLanguage(langStr); err != nil {
			cfg.Logger.Errorf("Error parsing language retrieved from database %q: %v", langStr, err)
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
	creds, err := coreDB.PrimaryCredentials()
	if err != nil && !errors.Is(err, db.ErrNoCredentials) {
		return nil, err
	}

	// The seed generation time is the credentials' Birthday; there is no
	// separate database record.
	var seedGenerationTime uint64
	if creds != nil && !creds.Birthday.IsZero() {
		seedGenerationTime = uint64(creds.Birthday.Unix())
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
		cfg:         cfg,
		credentials: creds,
		ready:       make(chan struct{}),
		log:         cfg.Logger,
		db:          coreDB,
		txDB:        masterTxDB,
		wallets:     make(map[uint32]*xcWallet),
		creating:    make(map[uint32]bool),
		net:         cfg.Net,
		tipPending:  make(map[uint32]uint64),
		tipActive:   make(map[uint32]bool),
		balPending:  make(map[uint32]*asset.Balance),
		balActive:   make(map[uint32]bool),
		newCrypter:  encrypt.NewCrypter,
		reCrypter:   encrypt.Deserialize,
		noteChans:   make(map[uint64]chan Notification),

		extensionModeConfig: xCfg,
		seedGenerationTime:  seedGenerationTime,

		fiatRateSources: make(map[string]*commonRateSource),

		autoDisabledRateSources: make(map[string]time.Time),

		reFiat: make(chan struct{}, 1),

		notes:            make(chan asset.WalletNotification, 128),
		requestedActions: make(map[string]*asset.ActionRequiredNote),
	}

	c.intl.Store(&locale{
		lang:    lang,
		m:       translations,
		printer: message.NewPrinter(lang),
	})

	// Populate the initial user data.
	c.log.Debugf("new client core created")
	constructed = true
	return c, nil
}

// beginTask registers a unit of work on c.wg, reporting false if shutdown has
// already begun and no new work may be registered. The Add happens under
// taskMtx, the same mutex Run's notification goroutine takes to publish
// shuttingDown before it drops its own count, so a beginTask that wins the
// mutex is guaranteed to have incremented the counter before Run's Wait can
// observe zero. Pair every true return with endTask.
func (c *Core) beginTask() bool {
	c.taskMtx.Lock()
	defer c.taskMtx.Unlock()
	if c.shuttingDown {
		return false
	}
	c.wg.Add(1)
	return true
}

// endTask retires work registered by a beginTask that returned true.
func (c *Core) endTask() {
	c.wg.Done()
}

// Run runs the core. Satisfies the runner.Runner interface.
func (c *Core) Run(ctx context.Context) {
	c.log.Infof("Starting Caribou core")
	// Store the context as a field for use by goroutines spawned during
	// initialization.
	c.ctx = ctx
	err := c.initialize()
	if err != nil {
		c.log.Critical(err)
		close(c.ready) // unblock <-Ready()
		// Connect was never called on either database, so close the stores
		// opened by New here; nothing else will.
		if closeErr := c.db.Close(); closeErr != nil {
			c.log.Errorf("Error closing database: %v", closeErr)
		}
		if closeErr := c.txDB.Close(); closeErr != nil {
			c.log.Errorf("Error closing transaction database: %v", closeErr)
		}
		return
	}
	close(c.ready)

	// The DB starts first and stops last. Both databases are bound to their
	// own contexts rather than the application ctx so their shutdown
	// (storage-engine close, GC drain, etc.) can be deferred until after the
	// wallets are locked and disconnected below. This prevents an in-flight
	// write during wind-down — a wallet transaction or a shutdown
	// notification — from hitting a closed DB.
	ctxDB, stopDB := context.WithCancel(context.Background())
	dbWG, err := c.db.Connect(ctxDB)
	if err != nil {
		c.log.Critical(fmt.Errorf("error connecting database: %w", err))
		// Connect did not take ownership of the DBs opened by New(), so close
		// them here; nothing else will. dbWG is nil on this path, and the tx
		// DB is not yet connected.
		if closeErr := c.db.Close(); closeErr != nil {
			c.log.Errorf("Error closing database: %v", closeErr)
		}
		if closeErr := c.txDB.Close(); closeErr != nil {
			c.log.Errorf("Error closing transaction database: %v", closeErr)
		}
		stopDB()
		return
	}

	ctxTxDB, stopTxDB := context.WithCancel(context.Background())
	txDBWG, err := c.txDB.Connect(ctxTxDB)
	if err != nil {
		// The transaction database is essential: wallets are wired to it
		// before Run is called, so continuing here would leave them writing
		// to a closed handle. Treat a connect failure as fatal, mirroring the
		// initialize() path above.
		c.log.Critical(fmt.Errorf("error connecting transaction database: %w", err))
		// Connect did not take ownership of the DB opened by New(), so close
		// it here; nothing else will. txDBWG is nil on this path.
		if closeErr := c.txDB.Close(); closeErr != nil {
			c.log.Errorf("Error closing transaction database: %v", closeErr)
		}
		// Release the main DB context, which closes the connected main DB.
		stopTxDB()
		stopDB()
		dbWG.Wait()
		return
	}

	// Retrieve disabled fiat rate sources from database.
	disabledSources, err := c.db.DisabledRateSources()
	if err != nil {
		c.log.Errorf("Unable to retrieve disabled fiat rate source: %v", err)
	}

	c.initFiatRateSources(disabledSources)
	c.fetchFiatExchangeRates(ctx)

	// Handle wallet notifications.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			select {
			case n := <-c.notes:
				c.handleWalletNotification(n)
			case <-ctx.Done():
				// Publish the flag before the deferred Done below drops this
				// goroutine's count, so no beginTask can Add after Wait has
				// seen zero.
				c.taskMtx.Lock()
				c.shuttingDown = true
				c.taskMtx.Unlock()
				return
			}
		}
	}()

	// Cross-chain swap engine (near-intents.go).
	c.startIntentsEngine(ctx)

	c.wg.Wait() // block here until all goroutines except DB complete

	if err := c.db.SavePokes(c.pokes()); err != nil {
		c.log.Errorf("Error saving pokes: %v", err)
	}

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

	// Now that the wallets are disconnected, stop the databases and wait for
	// them to finish shutting down. Closing them only here ensures no
	// in-flight write — a wallet transaction, or a notification fired from
	// the lock/disconnect path above — can race the engine close.
	stopDB()
	dbWG.Wait()
	stopTxDB()
	if txDBWG != nil {
		txDBWG.Wait()
	}

	c.log.Infof("Caribou core off")
}

// Ready returns a channel that is closed when Run completes its initialization
// tasks and Core becomes ready for use.
func (c *Core) Ready() <-chan struct{} {
	return c.ready
}

func (c *Core) locale() *locale {
	intlI := c.intl.Load()
	if intlI == nil {
		return nil
	}
	loc, ok := intlI.(*locale)
	if !ok {
		c.log.Warnf("unexpected type for locale: %T", intlI)
		return nil
	}
	return loc
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

// SetAPIToken stores the local API token hash in the database.
func (c *Core) SetAPIToken(token string) error {
	return c.db.SetAPIToken(token)
}

// APIToken retrieves the local API token hash from the database.
func (c *Core) APIToken() (string, error) {
	return c.db.APIToken()
}

// SetSendRequiresPassword stores whether sending funds requires the app
// password.
func (c *Core) SetSendRequiresPassword(require bool) error {
	return c.db.SetSendRequiresPassword(require)
}

// SendRequiresPassword reports whether sending funds requires the app
// password. If no value has been stored, false is returned without an error.
func (c *Core) SendRequiresPassword() (bool, error) {
	return c.db.SendRequiresPassword()
}

const defaultPort = "7232"

// addrHost returns the host or url:port pair for an address.
func addrHost(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	const defaultHost = "localhost"
	const missingPort = "missing port in address"
	// Empty addresses are localhost.
	if addr == "" {
		return defaultHost + ":" + defaultPort, nil
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
			return net.JoinHostPort(host, defaultPort), nil
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
			return net.JoinHostPort(host, defaultPort), nil
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
	return c.credentials
}

// setCredentials stores the *PrimaryCredentials.
func (c *Core) setCredentials(creds *db.PrimaryCredentials) {
	c.credMtx.Lock()
	c.credentials = creds
	c.credMtx.Unlock()
}

// Network returns the current wallet network.
func (c *Core) Network() util.Network {
	return c.net
}

// TorProxy returns the configured Tor proxy address, or "" if none.
func (c *Core) TorProxy() string {
	return c.cfg.TorProxy
}

// wallet gets the wallet for the specified asset ID in a thread-safe way.
func (c *Core) wallet(assetID uint32) (*xcWallet, bool) {
	c.walletMtx.RLock()
	w, found := c.wallets[assetID]
	c.walletMtx.RUnlock()
	return w, found
}

func (c *Core) initializeTokenWallet(tokenID uint32, tkn *asset.Token) error {
	if _, found := c.wallet(tkn.NetworkID); !found {
		return fmt.Errorf("no network wallet %d for token %s", tkn.NetworkID, tkn.Name)
	}
	dbWallet, err := c.createTokenDBWallet(tokenID, tkn, &WalletForm{
		AssetID: tokenID,
		Config:  make(map[string]string),
		Type:    tkn.Definition.Type,
	})
	if err != nil {
		return fmt.Errorf("error creating %s token wallet with existing %s network wallet: %w",
			util.BipIDSymbol(tokenID), util.BipIDSymbol(tkn.NetworkID), err)
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
	c.log.Infof("Token wallet for %s automatically created", util.BipIDSymbol(tokenID))
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

// VerifyPassword reports whether pw is the correct application password. It
// derives the encryption key and discards it, touching no session state, so it
// is safe to call while logged in (e.g. the mobile biometric opt-in confirms
// the password before storing it in the OS keystore). A nil return means the
// password is correct.
func (c *Core) VerifyPassword(pw []byte) error {
	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return err
	}
	crypter.Close()
	return nil
}

func (c *Core) storeDepositAddress(assetID uint32, addr string) error {
	return c.db.SetWalletAddress(assetID, addr)
}

// connectAndUpdateWallet creates a connection to a wallet and updates the
// balance.
func (c *Core) connectAndUpdateWallet(w *xcWallet) error {
	assetID := w.AssetID

	token := asset.TokenInfo(assetID)
	if token != nil {
		networkWallet, found := c.wallet(token.NetworkID)
		if !found {
			return fmt.Errorf("token %s wallet has no %s network wallet", unbip(assetID), unbip(token.NetworkID))
		}
		if !networkWallet.connected() {
			if err := c.connectAndUpdateWallet(networkWallet); err != nil {
				return fmt.Errorf("failed to connect %s network wallet for %s token: %v",
					unbip(token.NetworkID), unbip(assetID), err)
			}
		}
	}

	c.log.Debugf("Connecting wallet for %s", unbip(assetID))
	addr := w.currentDepositAddress()
	newAddr, err := c.connectWallet(w)
	if err != nil {
		return fmt.Errorf("connectWallet: %w", err) // core.Error with code connectWalletErr
	}
	if newAddr != addr {
		c.log.Infof("New deposit address for %v wallet: %v", unbip(assetID), newAddr)
		if err = c.storeDepositAddress(w.AssetID, newAddr); err != nil {
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

// connectWallet connects to the wallet and returns the deposit address
// validated by the xcWallet after connecting. If the wallet backend is still
// syncing, this also starts a goroutine to monitor sync status, emitting
// WalletStateNotes on each progress update.
func (c *Core) connectWallet(w *xcWallet) (depositAddr string, err error) {
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

	if !synced {
		c.startWalletSyncMonitor(w)
	}

	return
}

// unlockWallet will unlock a wallet if it is not yet unlocked.
func (c *Core) unlockWallet(crypter encrypt.Crypter, wallet *xcWallet) error {
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
		// Notify new wallet state, but only for a registered wallet. During
		// wallet creation this runs before the wallet is added to the wallet
		// map, and creation can still fail after this point — broadcasting
		// here would tell clients about a wallet core may never have.
		if _, found := c.wallet(wallet.AssetID); found {
			c.notify(newWalletStateNote(wallet.state()))
		}
	}

	return nil
}

// connectAndUnlock will connect to the wallet if not already connected,
// and unlock the wallet if not already unlocked. If the wallet backend
// is still syncing, this also starts a goroutine to monitor sync status,
// emitting WalletStateNotes on each progress update.
func (c *Core) connectAndUnlock(crypter encrypt.Crypter, wallet *xcWallet) error {
	if !wallet.connected() {
		if err := c.connectAndUpdateWallet(wallet); err != nil {
			return err
		}
	}
	return c.unlockWallet(crypter, wallet)
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
	err := c.db.UpdateBalance(wallet.AssetID, walletBal.Balance)
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
			if _, alreadyUpdating := assets[token.NetworkID]; alreadyUpdating {
				continue
			}
			networkWallet, exists := c.wallet(token.NetworkID)
			if !exists {
				c.log.Errorf("non-existent %d wallet should exist", token.NetworkID)
				continue
			}
			_, err := c.updateWalletBalance(networkWallet)
			if err != nil {
				c.log.Errorf("error updating %q balance: %v", unbip(token.NetworkID), err)
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

	// If this wallet is a network wallet, disable/enable all token wallets.
	var affectedWallets []*xcWallet
	if disable {
		// Ensure wallet is not the network wallet of an enabled token wallet
		// with active orders.
		if assetInfo := asset.Asset(assetID); assetInfo != nil {
			for id := range assetInfo.Tokens {
				if wallet, exists := c.wallet(id); exists && !wallet.isDisabled() {
					affectedWallets = append(affectedWallets, wallet)
				}
			}
		}

		// If wallet is a network wallet, it will be the last to be disconnected
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
		if wallet.network != nil && wallet.network.isDisabled() {
			// Ensure network wallet starts first.
			affectedWallets = append(affectedWallets, wallet.network)
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
		err := c.db.UpdateWalletStatus(wallet.AssetID, disable)
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
			ID:          assetID,
			Symbol:      asset.Symbol,
			Wallet:      wallet,
			Info:        asset.Info,
			Name:        asset.Info.Name,
			UnitInfo:    asset.Info.UnitInfo,
			TickerGroup: normalizeTickerUnit(asset.Info.UnitInfo.Conventional.Unit),
		}
		for tokenID, token := range asset.Tokens {
			wallet = nil
			w, found := c.wallets[tokenID]
			if found {
				wallet = w.state()
			}
			assets[tokenID] = &SupportedAsset{
				ID:          tokenID,
				Symbol:      util.BipIDSymbol(tokenID),
				Wallet:      wallet,
				Token:       token,
				Name:        token.Name,
				UnitInfo:    token.UnitInfo,
				TickerGroup: normalizeTickerUnit(token.UnitInfo.Conventional.Unit),
			}
		}
	}
	return assets
}

// FiatRates is a thread-safe getter for the cached USD exchange rates,
// keyed by asset ID. Assets whose sources have no fresh data are absent.
func (c *Core) FiatRates() map[uint32]float64 {
	return c.fiatConversions()
}

// User is a thread-safe getter for the User.
func (c *Core) User() *User {
	fiatRates := c.fiatConversions()
	return &User{
		Assets:             c.assetMap(),
		Initialized:        c.IsInitialized(),
		SeedGenerationTime: c.seedGenerationTime,
		FiatRates:          fiatRates,
		TickerRates:        c.tickerConversions(fiatRates),
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
	release, err := c.claimWalletCreation(assetID)
	if err != nil {
		return err
	}
	// Released after the deferred discard below has run, so no other creation
	// can touch the asset's data directory until this one is fully unwound.
	defer release()

	token := asset.TokenInfo(assetID)
	var dbWallet *db.Wallet
	if token == nil {
		walletDef, defErr := asset.WalletDef(assetID, form.Type)
		if defErr != nil {
			return newError(assetSupportErr, "asset.WalletDef error: %w", defErr)
		}
		var fresh bool
		dbWallet, fresh, err = c.createDBWallet(crypter, walletDef, form, walletPW)
		if fresh {
			// This call created the asset's data directory, so everything in
			// it was written by this call, and a seeded wallet is rebuilt in
			// full from the app seed. A failure at any step (on-disk creation,
			// open, connect, unlock, balance, db store) must remove the
			// directory again, so the next attempt starts from nothing instead
			// of tripping over this attempt's remains. A directory that
			// predates the call is never discarded, whatever it holds: it may
			// belong to a wallet that failed to load at startup, and it is not
			// this call's to remove. Every failure path after loadXCWallet
			// disconnects the wallet before returning, which also closes an
			// asset.Opener, so nothing holds the directory open when this
			// runs, and the creation claim is still held, so no other creation
			// can be using it.
			defer func() {
				if err != nil {
					c.discardAssetData(assetID)
				}
			}()
		}
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

	// Every failure path from here on disconnects the wallet before
	// returning. Disconnect is what closes a wallet that OpenWithPW opened,
	// and the deferred discard must not run under an open wallet.
	if err = wallet.OpenWithPW(c.ctx, crypter); err != nil {
		wallet.Disconnect()
		return err
	}

	dbWallet.Address, err = c.connectWallet(wallet)
	if err != nil {
		wallet.Disconnect()
		return err
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

	// Token wallet creation is purely local. A token that needs an on-chain
	// opt-in before it can be received (asset.TrustlineEstablisher) gets it
	// later, through EstablishTrustline, when the user asks to receive it:
	// the opt-in commits funds and needs a funded account, and a freshly
	// created wallet usually has neither.
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
				c.log.Errorf("Error creating token %s wallet for network %s", tkn.Name, symbol)
			}
		}
	}

	return nil
}

// createDBWallet builds the db.Wallet record for a new non-token wallet,
// creating the seeded wallet on disk first when the definition calls for one.
// The returned bool reports whether this call created the asset's data
// directory; see createSeededWallet.
func (c *Core) createDBWallet(crypter encrypt.Crypter, walletDef *asset.WalletDefinition, form *WalletForm, walletPW []byte) (*db.Wallet, bool, error) {
	var err error
	var fresh bool

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
			return nil, false, errors.New("external password incompatible with seeded wallet")
		}
		walletPW, fresh, err = c.createSeededWallet(form.AssetID, crypter, form)
		if err != nil {
			return nil, fresh, err
		}
	}

	var encPW []byte
	if len(walletPW) > 0 {
		encPW, err = crypter.Encrypt(walletPW)
		if err != nil {
			return nil, fresh, fmt.Errorf("wallet password encryption error: %w", err)
		}
	}

	return &db.Wallet{
		Type:        walletDef.Type,
		AssetID:     form.AssetID,
		Settings:    form.Config,
		EncryptedPW: encPW,
		// Balance and Address are set after connect.
	}, fresh, nil
}

func (c *Core) createTokenDBWallet(tokenID uint32, token *asset.Token, form *WalletForm) (*db.Wallet, error) {
	wallet, found := c.wallet(token.NetworkID)
	if !found {
		return nil, fmt.Errorf("no network wallet %d for token %d (%s)", token.NetworkID, tokenID, unbip(tokenID))
	}

	tokenMaster, is := wallet.Wallet.(asset.TokenMaster)
	if !is {
		return nil, fmt.Errorf("network wallet %s is not a TokenMaster", unbip(token.NetworkID))
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
		// authorization is handled by the network wallet.
		// Balance and Address are set after connect.
	}, nil
}

// createSeededWallet makes sure the seeded wallet for an asset exists on disk
// and returns the password that opens it, for encrypting and storing. Both the
// wallet seed and the password are derived deterministically from the app
// seed, so wallet data already in the asset's data directory is used as-is
// rather than recreated. A creation interrupted after the on-disk wallet was
// made but before core stored the wallet leaves exactly that, and it would
// otherwise block every later creation with an already-exists error. The
// returned bool reports whether the asset's data directory did not exist
// before this call, so that everything in it afterwards, complete or not, was
// written by this call. The driver's existence check is narrower, scoped to
// one wallet type, and cannot say whether the directory holds anything else.
func (c *Core) createSeededWallet(assetID uint32, crypter encrypt.Crypter, form *WalletForm) ([]byte, bool, error) {
	seed, pw, err := c.assetSeedAndPass(assetID, crypter)
	if err != nil {
		return nil, false, err
	}
	defer encode.ClearBytes(seed)

	dataDir := c.assetDataDirectory(assetID)
	_, statErr := os.Stat(dataDir)
	fresh := os.IsNotExist(statErr)
	exists, err := asset.WalletExists(assetID, form.Type, dataDir, form.Config, c.net)
	if err != nil {
		return nil, false, fmt.Errorf("error checking for existing wallet data: %w", err)
	}
	if exists {
		c.log.Infof("Using the existing %s wallet data in %q", unbip(assetID), dataDir)
		return pw, false, nil
	}

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
		DataDir:  dataDir,
		Net:      c.net,
		Logger:   c.log.SubLogger(unbip(assetID)),
		TorProxy: c.cfg.TorProxy,
	}); err != nil {
		return nil, fresh, fmt.Errorf("Error creating wallet: %w", err)
	}

	return pw, fresh, nil
}

func (c *Core) assetSeedAndPass(assetID uint32, crypter encrypt.Crypter) (seed, pass []byte, err error) {
	creds := c.creds()
	if creds == nil {
		return nil, nil, errors.New("no credentials stored")
	}

	if tkn := asset.TokenInfo(assetID); tkn != nil {
		return nil, nil, fmt.Errorf("%s is a token. assets seeds are for base chains only. did you want %s",
			tkn.Name, asset.Asset(tkn.NetworkID).Info.Name)
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
// usable for accessing funds with external wallet software, e.g. btcwallet.
func AssetSeedAndPass(assetID uint32, appSeed []byte) ([]byte, []byte) {
	const accountBasedSeedAssetID = 60 // ETH
	seedAssetID := assetID
	if ai, _ := asset.Info(assetID); ai != nil && ai.BlockchainClass.IsEVM() {
		seedAssetID = accountBasedSeedAssetID
	}
	// Tokens asset IDs shouldn't be passed in, but if they are, return the seed
	// for the network ID.
	if tkn := asset.TokenInfo(assetID); tkn != nil {
		if ai, _ := asset.Info(tkn.NetworkID); ai != nil {
			if ai.BlockchainClass.IsEVM() {
				seedAssetID = accountBasedSeedAssetID
			}
		}
	}
	// A wallet that must share another asset's address (e.g. HyperEVM sharing
	// HyperCore's) overrides both rules above.
	if ai, _ := asset.Info(assetID); ai != nil && ai.SeedAssetID != 0 {
		seedAssetID = ai.SeedAssetID
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

// discardAssetData removes an asset's data directory. Only for a directory
// created by a wallet creation that then failed and released the wallet, with
// the creation claim still held: nothing in the directory predates the attempt
// or is in use, and the next creation rebuilds the wallet in full from the app
// seed.
func (c *Core) discardAssetData(assetID uint32) {
	dir := c.assetDataDirectory(assetID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		c.log.Errorf("Error removing %s wallet data directory %q after failed wallet creation: %v",
			unbip(assetID), dir, err)
		return
	}
	c.log.Infof("Removed %s wallet data directory %q after failed wallet creation", unbip(assetID), dir)
}

// loadWallet uses the data from the database to construct a new exchange
// wallet. The returned wallet is running but not connected.
func (c *Core) loadXCWallet(dbWallet *db.Wallet) (*xcWallet, error) {
	var network *xcWallet
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
		walletType:   dbWallet.Type,
		broadcasting: new(uint32),
		disabled:     dbWallet.Disabled,
		syncStatus:   &asset.SyncStatus{},
		pendingTxs:   make(map[string]*txdb.WalletTransaction),
	}

	token := asset.TokenInfo(assetID)

	peersChange := func(numPeers uint32, err error) {
		if !c.beginTask() {
			return
		}

		go func() {
			defer c.endTask()
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
			NetworkTxDB: c.txDB.NetworkTxDB(assetID),
		}

		settings[asset.SpecialSettingActivelyUsed] = "false"
		defer delete(settings, asset.SpecialSettingActivelyUsed)

		w, err = asset.OpenWallet(assetID, walletCfg, log, c.net)
	} else {
		var found bool
		network, found = c.wallet(token.NetworkID)
		if !found {
			return nil, fmt.Errorf("cannot load %s wallet before %s wallet", unbip(assetID), unbip(token.NetworkID))
		}

		tokenMaster, is := network.Wallet.(asset.TokenMaster)
		if !is {
			return nil, fmt.Errorf("%s token's %s network wallet is not a TokenMaster", unbip(assetID), unbip(token.NetworkID))
		}

		w, err = tokenMaster.OpenTokenWallet(&asset.TokenConfig{
			AssetID:     assetID,
			Settings:    settings,
			Emit:        asset.NewWalletEmitter(c.notes, assetID, log),
			PeersChange: peersChange,
			NetworkTxDB: c.txDB.NetworkTxDB(token.NetworkID),
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
	wallet.network = network
	wallet.connector = util.NewConnectionMaster(w)
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
// stores the balance in the DB, and emits a BalanceNote.
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

	if !c.beginTask() {
		atomic.StoreUint32(&wallet.monitored, 0) // don't strand the CAS
		return
	}
	go func() {
		defer c.endTask()
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
				// A mobile background pause disconnects every wallet, so an
				// unsynced wallet lands here on every pause. That is routine,
				// and warning about it once per pause per wallet buries the
				// genuinely unexpected shutdown this warning is for.
				if atomic.LoadUint32(&wallet.suspended) != 0 {
					c.log.Debugf("%v wallet suspended before sync completed.", wallet.Info().Name)
				} else {
					c.log.Warnf("%v wallet shut down before sync completed.", wallet.Info().Name)
				}
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
// this will check for active swaps involving this asset before initiating a
// rescan. WARNING: It is ill-advised to initiate a wallet rescan with active
// swaps unless as a last ditch effort to get the wallet to recognize a
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

// claimWalletCreation reserves an asset for one in-flight wallet creation and
// returns the func that releases the claim. The wallet lookup and the claim
// share one critical section, so two creations for the same asset cannot both
// find no wallet and go on to fight over the asset's data directory, where the
// loser would discard what the winner is using.
func (c *Core) claimWalletCreation(assetID uint32) (release func(), err error) {
	c.walletMtx.Lock()
	defer c.walletMtx.Unlock()
	if _, exists := c.wallets[assetID]; exists {
		return nil, fmt.Errorf("%s wallet already exists", unbip(assetID))
	}
	if c.creating[assetID] {
		return nil, fmt.Errorf("%s wallet creation already in progress", unbip(assetID))
	}
	c.creating[assetID] = true
	return func() {
		c.walletMtx.Lock()
		delete(c.creating, assetID)
		c.walletMtx.Unlock()
	}, nil
}

// RecoverWallet will retrieve some recovery information from the wallet,
// which may not be possible if the wallet is too corrupted. Disconnect and
// destroy the old wallet, create a new one, and if the recovery information
// was retrieved from the old wallet, send this information to the new one.
// If force is false, this will check for active swaps involving this
// asset before initiating a rescan. WARNING: It is ill-advised to initiate
// a wallet recovery with active swaps unless the wallet db is definitely
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

	dbWallet, err := c.db.Wallet(oldWallet.AssetID)
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

	// Carry the seed's birthday into the rebuilt wallet. Without this the
	// wallet defaults to DefaultWalletBirthday (2021) and re-scans the whole
	// chain from there even for a wallet created today.
	var bday uint64
	if creds := c.creds(); !creds.Birthday.IsZero() {
		bday = uint64(creds.Birthday.Unix())
	}

	if err = asset.CreateWallet(assetID, &asset.CreateWalletParams{
		Type:     dbWallet.Type,
		Seed:     seed,
		Pass:     pw,
		Birthday: bday,
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
		"= %d / locked = %d / locked in contracts = %d, Deposit address = %s",
		state.Symbol, balances.Available, balances.Locked, balances.ContractLocked,
		state.Address)

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
	dbWallet, err := c.db.Wallet(wallet.AssetID)
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
			return fmt.Errorf("reconfigure: %v", err)
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
				if err = c.setWalletPassword(oldWallet, newWalletPW, crypter, false); err != nil {
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
			pw, _, err = c.createSeededWallet(assetID, crypter, form)
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
		dbWallet.Address, err = c.connectWallet(w)
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
				err = c.setWalletPassword(wallet, newWalletPW, crypter, false)
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
			tokenDBWallet, err := c.db.Wallet(tokenID)
			if err != nil {
				c.log.Errorf("Error getting db wallet for token %s: %v", unbip(tokenID), err)
				continue
			}
			tokenWallet.Disconnect()
			tokenWallet, err = c.loadXCWallet(tokenDBWallet)
			if err != nil {
				c.log.Errorf("Error loading wallet for token %s: %v", unbip(tokenID), err)
				continue
			}
			if err := reloadWallet(tokenWallet, tokenDBWallet, false); err != nil {
				c.log.Errorf("Error reloading token wallet %s: %v", unbip(tokenID), err)
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

	// Check that the specified wallet exists. Only the lookup takes walletMtx:
	// setWalletPassword connects the wallet, runs backend Unlock/Lock RPCs and
	// writes to the DB, and holding the process-wide write lock across that
	// would stall every other asset's c.wallet lookup on an unreachable node.
	wallet, found := c.wallet(assetID)
	if !found {
		return newError(missingWalletErr, "wallet for %s (%d) is not known", unbip(assetID), assetID)
	}

	// Set new password, connecting to it if necessary to verify. It is left
	// connected since it is in the wallets map.
	return c.setWalletPassword(wallet, newPW, crypter, true)
}

// setWalletPassword updates the (encrypted) password for the wallet. mustBeLive
// requires that wallet still be the live entry in c.wallets for its asset when
// the update is persisted; only SetWalletPassword holds that invariant.
// ReconfigureWallet's restart path installs the new wallet into c.wallets only
// after this call returns, so it passes false and relies instead on its
// documented no-concurrent-calls-per-asset contract.
func (c *Core) setWalletPassword(wallet *xcWallet, newPW []byte, crypter encrypt.Crypter, mustBeLive bool) error {
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

	// Refuse to persist against a wallet swapped out of c.wallets while the
	// RPCs above ran unlocked from walletMtx: the wallet this func was given
	// may no longer be the live one for assetID. Only applies to callers
	// (SetWalletPassword) whose invariant is that wallet is already the live
	// entry; ReconfigureWallet's restart path calls this before the new
	// wallet is installed into c.wallets, so it opts out.
	if mustBeLive {
		if current, ok := c.wallet(wallet.AssetID); !ok || current != wallet {
			return newError(missingWalletErr, "%s wallet was reconfigured during password update",
				unbip(wallet.AssetID))
		}
	}

	err = c.db.SetWalletPassword(wallet.AssetID, wallet.encPW())
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
		if err = c.storeDepositAddress(w.AssetID, addr); err != nil {
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

	// The seed generation time is the credentials' Birthday, already stored
	// with the credentials.
	c.seedGenerationTime = uint64(creds.Birthday.Unix())

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
		return "", fmt.Errorf("no credentials stored")
	}

	seed, err := crypter.Decrypt(creds.EncSeed)
	if err != nil {
		return "", fmt.Errorf("app seed decryption error: %w", err)
	}

	seedStr, err = mnemonic.GenerateMnemonic(seed, creds.Birthday)
	if err != nil {
		return "", fmt.Errorf("error generating mnemonic: %w", err)
	}

	return seedStr, nil
}

func decodeSeedString(seedStr string) (seed []byte, bday time.Time, err error) {
	return mnemonic.DecodeMnemonic(seedStr)
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
	}

	return innerCrypter, creds, mnemonicSeed, nil
}

func seedInnerKey(seed []byte) []byte {
	// keyParam is a domain-specific value to ensure the resulting key is unique
	// for the specific use case of deriving an inner encryption key from the
	// seed. Any other uses of derivation from the seed should similarly create
	// their own domain-specific value to ensure uniqueness.
	//
	// It is equal to BLAKE-256([]byte("Caribou-InnerKey-v0")).
	keyParam := [32]byte{
		0x26, 0x85, 0x02, 0x2d, 0x65, 0x70, 0x98, 0x43,
		0x24, 0x6f, 0x99, 0xbc, 0xa2, 0x36, 0x07, 0xfe,
		0xb9, 0x98, 0x73, 0x46, 0x91, 0x25, 0xe2, 0xab,
		0xa9, 0xa1, 0xce, 0xbf, 0x50, 0x8c, 0xfb, 0x41,
	}
	key := make([]byte, len(seed)+len(keyParam))
	copy(key, seed)
	copy(key[len(seed):], keyParam[:])
	innerKey := blake256.Sum256(key)
	return innerKey[:]
}

// Login logs the user in. On the first login after startup or after a logout,
// this function will connect wallets and decrypt stored credentials. Otherwise,
// it will only check whether or not the app pass is correct.
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

	crypter, err := c.encryptionKey(pw)
	if err != nil {
		return err
	}
	defer crypter.Close()

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

	// loginConnecting is claimed in the same suspendMtx hold that publishes
	// loggedIn, because the gap between them is reachable: Login releases
	// loginMtx and only then calls connectWallets. A stale Resume landing in
	// that gap would see a logged-in session with every wallet still down and
	// read it as wallets a newer session stranded. Taking suspendMtx around
	// login() rather than inside it keeps the global suspendMtx-before-loginMtx
	// order; the work it covers is a seed decrypt and a key derivation, not the
	// app pass KDF, which encryptionKey already did above. connectWallets
	// clears the flag at the end of its connect.
	c.suspendMtx.Lock()
	needsInit, err := login()
	if err == nil && needsInit {
		c.loginConnecting = true
	}
	c.suspendMtx.Unlock()
	if err != nil {
		return err
	}
	if needsInit {
		c.notify(newLoginNote("Connecting wallets..."))
		c.connectWallets(crypter) // initialize reserves
	}

	return nil
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
		// Return early if this is a token and its network wallet is disabled.
		if token := asset.TokenInfo(wallet.AssetID); token != nil {
			if networkWallet, found := c.wallet(token.NetworkID); found && networkWallet.isDisabled() {
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

	// A mobile pause can land in the middle of this, or anywhere earlier in
	// Login. Login sets loggedIn and releases loginMtx before calling us, so a
	// Suspend can see the session as logged in and walk a wallet set that is
	// still coming up: it records nothing, because suspend() reports false for a
	// wallet that is not connected yet, and latches the pause anyway. A Suspend
	// landing before that, during the app pass KDF or the seed decrypt, finds no
	// session and no connected wallets, and latches the pause for the same
	// reason. Either way everything we just brought up would poll on through a
	// background the app believes is quiet. Sweep once here rather than per
	// wallet, because the token phase reconnects network wallets and would undo
	// an earlier sweep.
	c.suspendMtx.Lock()
	// The login window closes here, not before the sweep: a stale Resume that
	// lands between the last dial and this lock is still one rearmUnreached
	// must leave alone, and the sweep below records anything a real pause
	// caught.
	c.loginConnecting = false
	if c.suspended {
		c.log.Infof("Pause landed during login; suspending the wallets it connected")
		c.suspendConnected()
	}
	c.suspendMtx.Unlock()
}

// Notifications loads the latest notifications from the db.
func (c *Core) Notifications(n int) (notes, pokes []*db.Notification, _ error) {
	notes, err := c.db.NotificationsN(n)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting notifications: %w", err)
	}
	return notes, c.pokes(), nil
}

// pokes returns an empty slice.
func (c *Core) pokes() []*db.Notification {
	return nil
}

// Active indicates if there are any active swaps. Always false in this build.
func (c *Core) Active() bool {
	return false
}

// Logout logs the user out
func (c *Core) Logout() error {
	// Lock order: suspendMtx before loginMtx (see Suspend/Resume), so that
	// clearing the pause state below cannot deadlock against a concurrent
	// Suspend/Resume.
	c.suspendMtx.Lock()
	defer c.suspendMtx.Unlock()
	c.loginMtx.Lock()
	defer c.loginMtx.Unlock()

	if !c.loggedIn {
		return nil
	}

	// Lock wallets
	if !c.cfg.NoAutoWalletLock {
		// Ensure wallet lock in c.Run waits for c.Logout if this is called
		// before shutdown. If shutdown has already begun the registration is
		// moot: Run locks and disconnects the wallets itself right after Wait
		// returns, and the loop below is best-effort either way, so run it
		// unregistered rather than skipping it. Unregistered here means this
		// loop can run concurrently with Run's own post-Wait lock loop: this
		// loop snapshots via xcWallets() under the read lock while Run's loop
		// holds the write lock, so map access is safe, but the two loops can
		// still call wallet.Lock on the same wallet at the same time, which is
		// the same Lock/Unlock inversion documented on xcWallet.Lock. Still
		// strictly better than the previous panic on this path.
		registered := c.beginTask()
		for _, w := range c.xcWallets() {
			if !w.connected() {
				// A wallet suspended by a mobile background pause is
				// disconnected, so it cannot be locked: xcWallet.Lock needs the
				// connection to reach the backend. Every eviction is preceded by
				// a pause, though, so this is the mainline mobile path, not a
				// corner case. Shed the cached decrypted password directly,
				// which is the key material the lock tier exists to evict.
				w.clearCachedPW()
				continue
			}
			if !w.unlocked() {
				continue
			}
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
				// lock wallets or shutdown Core gracefully.
				c.log.Warnf("Unable to lock %v wallet: %v", unbip(w.AssetID), err)
			}
		}
		if registered {
			c.endTask()
		}
	}

	// Disconnect wallets so a locked, backgrounded app stops polling and its
	// sockets close. Logout previously locked but left every wallet connected,
	// so the radio never quieted on the escalation-tier lock. Disconnect is
	// called unconditionally because it is idempotent and because a wallet
	// suspended by a prior pause still needs it: Disconnect is the only caller
	// of asset.Opener.Close, and suspend deliberately leaves xmr's wallet2
	// instance open. Closing it here is correct since the session is ending.
	for _, w := range c.xcWallets() {
		w.Disconnect()
	}
	c.suspended = false
	c.suspendedWallets = nil
	c.suspendGen++

	c.multisigXPriv.Zero()
	c.multisigXPriv = nil

	c.loggedIn = false

	return nil
}

// trackSuspended records assetID as a wallet the next Resume should reconnect,
// keeping the set ordered tokens before network wallets. That order is what
// suspendConnected's teardown loop produces and what every later reader of the
// set relies on - Resume walks it forward to reconnect, unwindStaleResume walks
// it forward to tear back down - so a plain append would quietly break it for
// the paths that hand wallets back into the set. Deduplicates, since more than
// one of those paths can name the same wallet. Caller holds suspendMtx.
func (c *Core) trackSuspended(assetID uint32) {
	for _, id := range c.suspendedWallets {
		if id == assetID {
			return
		}
	}
	if asset.TokenInfo(assetID) != nil {
		// Insert ahead of the first network wallet, if there is one.
		for i, id := range c.suspendedWallets {
			if asset.TokenInfo(id) == nil {
				c.suspendedWallets = append(c.suspendedWallets[:i],
					append([]uint32{assetID}, c.suspendedWallets[i:]...)...)
				return
			}
		}
	}
	c.suspendedWallets = append(c.suspendedWallets, assetID)
}

// suspendConnected disconnects every connected wallet and records it for the
// next Resume. Token wallets go before their network wallets: a token's
// connection rides on its network wallet's context, so tear the leaves down
// first. Caller holds suspendMtx.
func (c *Core) suspendConnected() {
	var tokens, networks []*xcWallet
	for _, w := range c.xcWallets() {
		if asset.TokenInfo(w.AssetID) != nil {
			tokens = append(tokens, w)
		} else {
			networks = append(networks, w)
		}
	}

	for _, w := range append(tokens, networks...) {
		if w.suspend() {
			c.trackSuspended(w.AssetID)
			c.log.Infof("Suspended %s wallet", unbip(w.AssetID))
		}
	}
}

// Suspend drops every connected wallet's network connection without logging out
// or locking, so a backgrounded mobile app stops polling and lets the radio
// sleep while keys and the session stay resident. Resume reconnects the same
// set. The native shell calls it (via mobile.App.Pause) after a short background
// grace, not on a quick app switch.
//
// The pause latches whether or not a session exists yet. A logged-out core has
// no connected wallets for the teardown loop to find, but Login is not atomic:
// it runs the app pass KDF and the seed decrypt before it sets loggedIn, and
// only then calls connectWallets. Returning early on a logged-out core would let
// a pause landing in that window record nothing and latch nothing, so the
// wallets connectWallets is about to bring up would poll through a background
// the app believes is quiet, with the next Resume a no-op on a clear flag.
// Latching instead hands that case to connectWallets' own sweep.
//
// Already being suspended is not a shortcut either: the teardown loop runs every
// time and merges what it finds into the existing set. A wallet can be connected
// under a pause that already latched - it was still coming up when Suspend
// walked past it during login, or a failed Resume re-armed the pause with the
// wallets that did come back up still live - and an early return would leave it
// polling with nothing to notice.
func (c *Core) Suspend() error {
	c.suspendMtx.Lock()
	defer c.suspendMtx.Unlock()

	c.suspendConnected()
	c.suspended = true
	c.suspendGen++
	return nil
}

// Resume reconnects the wallets that Suspend disconnected, restoring polling and
// sync when the mobile app returns to the foreground. connectAndUpdateWallet
// reconnects a token's network wallet first, refreshes balances, restarts the
// sync monitor, and emits a WalletState note, so the foregrounded UI repaints.
// No app password is needed: the wallets were never closed.
//
// A Lock/Logout while suspended clears the pause state itself, so Resume is a
// no-op afterwards and the shell's Unlock is what reconnects. Resume can still
// find the pause latched with no session, because Suspend latches one that lands
// before Login finishes; that is not an error, just a foreground with nothing
// to reconnect.
//
// suspendMtx is held only to claim the suspended set, not across the reconnects
// themselves: those are network-bound and unbounded, and Logout takes suspendMtx
// first thing, so holding it here would let a wedged reconnect stall a lock or a
// shutdown indefinitely. Dropping it means a Suspend or Logout can interleave, so
// every reconnect attempt is followed by unwindStaleResume, which undoes the
// wallets the session no longer wants. See suspendGen.
func (c *Core) Resume() error {
	c.suspendMtx.Lock()
	if !c.suspended {
		c.suspendMtx.Unlock()
		return nil
	}

	c.loginMtx.Lock()
	loggedIn := c.loggedIn
	c.loginMtx.Unlock()

	suspended := c.suspendedWallets
	c.suspended = false
	c.suspendedWallets = nil
	// Claim this generation. Resume itself does not bump: it is restoring the
	// state Suspend recorded, not starting a new one.
	gen := c.suspendGen
	c.suspendMtx.Unlock()

	if !loggedIn {
		// Clearing the flag above is the work: a pause that latched before Login
		// set loggedIn would otherwise make that login's connectWallets sweep
		// suspend every wallet it just brought up, in the foreground. There is
		// nothing to reconnect here - a pre-login pause records no wallets, and
		// Logout clears the set - so a non-empty one means some caller dropped
		// the session without going through Logout.
		if len(suspended) > 0 {
			c.log.Warnf("Resume found %d suspended wallet(s) with no session; "+
				"leaving them to the next login", len(suspended))
		}
		return nil
	}

	var failed []uint32
	for _, assetID := range suspended {
		w, resumable := c.resumableWallet(assetID)
		if !resumable {
			continue
		}
		err := c.connectAndUpdateWallet(w)
		// Re-check before acting on err, not after: a reconnect that fails does
		// so by burning a dial timeout, and Stop drains in-flight Resumes before
		// cancelling the core, so the error path is exactly where continuing
		// past a bumped generation is most expensive - one full timeout per
		// wallet left in the set, with a termination waiting behind it.
		if c.unwindStaleResume(suspended, gen) {
			return nil
		}
		if err != nil {
			c.log.Errorf("Unable to resume %s wallet: %v", unbip(assetID), err)
			failed = append(failed, assetID)
			continue
		}
		c.log.Infof("Resumed %s wallet", unbip(assetID))
	}

	c.rearmFailedResumes(failed, gen)
	return nil
}

// resumableWallet returns the wallet a Resume should reconnect for assetID, or
// false if there is nothing to reconnect: an unknown or disabled wallet, or a
// token whose network wallet was disabled while suspended, since that network
// wallet won't come back and the token rides on its connection.
func (c *Core) resumableWallet(assetID uint32) (*xcWallet, bool) {
	w, found := c.wallet(assetID)
	if !found || w.isDisabled() {
		return nil, false
	}
	if token := asset.TokenInfo(assetID); token != nil {
		if nw, ok := c.wallet(token.NetworkID); ok && nw.isDisabled() {
			return nil, false
		}
	}
	return w, true
}

// rearmFailedResumes puts the wallets whose reconnect failed back into the
// suspended set, so the next Resume retries them. Nothing else would: a
// disconnected wallet is invisible to the next Suspend's teardown loop,
// connectWallets runs only on Login, and a wallet's sync monitor exits with the
// context that reconnect was supposed to replace. Without this, one transient
// failure - likeliest of all on a foreground, when the radio is still coming
// back up - leaves the wallet dark for the rest of the session, with an error
// log as the only signal.
//
// The generation check is the one the reconnect loop uses: if the session moved
// on, unwindStaleResume owns these wallets and re-arming would fight it.
//
// Re-arming means the core stays marked suspended while the app is in the
// foreground, which is what makes the retry reachable at all, since Resume
// returns early when it is not. The wallets that did come back up are not
// stranded by that: Suspend runs its teardown loop whether or not the flag is
// already set.
func (c *Core) rearmFailedResumes(failed []uint32, gen uint64) {
	if len(failed) == 0 {
		return
	}

	c.suspendMtx.Lock()
	defer c.suspendMtx.Unlock()

	if c.suspendGen != gen {
		return
	}
	for _, assetID := range failed {
		c.trackSuspended(assetID)
	}
	c.suspended = true
	c.log.Warnf("%d wallet(s) failed to resume; retrying on the next foreground", len(failed))
}

// unwindStaleResume reports whether the session moved on since Resume claimed
// generation gen and, if it did, undoes the whole claimed set so Resume can
// stop. A Suspend or Logout that interleaved already ran its own teardown loop
// past these wallets, so anything Resume brought up is stranded: connected under
// a Suspend that meant to quiet the radio, or alive past a Logout.
//
// The whole set rather than the wallet that just finished, because a single
// connectAndUpdateWallet can bring up two: reconnecting a token connects its
// network wallet first. Undoing only the token would leave that network wallet
// connected and polling past the very lock meant to quiet it. The wallets Resume
// never reached are already down and both teardowns are idempotent, so sweeping
// the set costs nothing. Key material is not a concern in the Logout case, which
// cleared the cached passwords before we reconnected and which reconnecting
// cannot restore.
//
// What to do depends on what the session wants now, not merely on the fact that
// something moved. A Logout wants Disconnect, which closes an asset.Opener's
// wallet handle. A Suspend does not: leaving xmr's wallet2 instance open is the
// whole reason xcWallet.suspend exists, so use it and hand the wallet back to
// the interleaving Suspend's set, or the next Resume would have nothing to
// reconnect it from. Teardown is bounded, unlike the reconnects, so it is safe
// to do under suspendMtx.
//
// A newer Resume or a fresh Login that already finished is the third case, and
// it wants these wallets up. c.suspended cannot distinguish it from a logout on
// its own - both leave the flag clear - so ask whether the session is still
// logged in. Getting that wrong is not a missed cleanup but active damage: the
// wallets a foreground just brought back get disconnected, and nothing restores
// them, since the logout branch records nothing, the next Suspend's teardown
// loop skips a wallet that is already down, the next Resume returns early on a
// clear flag, and connectWallets runs only on Login. For xmr the Disconnect also
// closes the wallet handle that suspend() exists to keep open, so recovery would
// need an app password Resume does not have. Bowing out of that case is not
// quite the whole answer, though: see rearmUnreached for the tail of the set the
// newer session cannot know about.
func (c *Core) unwindStaleResume(claimed []uint32, gen uint64) bool {
	c.suspendMtx.Lock()
	defer c.suspendMtx.Unlock()

	if c.suspendGen == gen {
		return false
	}

	transition := "logout"
	if c.suspended {
		transition = "suspend"
	} else {
		// Lock order: suspendMtx before loginMtx, which is the order Suspend,
		// Resume and Logout all take them in.
		c.loginMtx.Lock()
		loggedIn := c.loggedIn
		c.loginMtx.Unlock()
		if loggedIn {
			c.rearmUnreached(claimed)
			return true
		}
	}
	c.log.Infof("Session changed (%s) while resuming; unwinding %d wallets", transition, len(claimed))

	// claimed is ordered tokens before network wallets (see suspendConnected),
	// which is the order teardown wants too. trackSuspended keeps the set we
	// hand back in that same order rather than appending our tokens behind the
	// interleaving Suspend's network wallets.
	for _, assetID := range claimed {
		w, found := c.wallet(assetID)
		if !found {
			continue
		}
		if !c.suspended {
			w.Disconnect()
			continue
		}
		w.suspend()
		c.trackSuspended(assetID)
	}
	return true
}

// rearmUnreached hands the wallets a stale Resume never reached back to the
// suspended set, so the next foreground retries them. The session it lost the
// race to wants them up, but it has no way to bring them up itself: they were
// already down when the interleaving Suspend swept, so its teardown loop had
// nothing to record, so the newer Resume never claimed them. Leaving them is the
// same stranding the whole staleness check exists to prevent - down with the app
// in the foreground, no polling and no sync monitor, until the next Login. The
// wallets that were reached are left alone; those are up and belong to the newer
// session now.
//
// Marking the core suspended in the foreground is what makes the retry reachable
// at all, since Resume returns early when it is not - the same trade
// rearmFailedResumes makes, and safe for the same reasons: a Logout clears the
// flag, and the wallets that are up are swept by the next Suspend whether or not
// it is already set. The generation is deliberately not bumped: the newer
// session still owns it, and bumping would strand the very Resume we are handing
// these wallets to. Caller holds suspendMtx.
//
// A fresh Login reaches the same branch of unwindStaleResume, and there the
// premise does not hold: Login sets loggedIn and releases loginMtx before
// calling connectWallets, so for the length of that connect every wallet is
// down because it has not been brought up yet, not because a session change
// stranded it. Re-arming there would latch the pause under a login, and
// connectWallets' end-of-connect sweep would then tear down everything it just
// connected - the stranding this function exists to prevent, from the login
// side. So while that window is open we leave the whole set to the login, which
// is bringing all of it up anyway.
func (c *Core) rearmUnreached(claimed []uint32) {
	if c.loginConnecting {
		c.log.Infof("Session changed (login) while resuming; "+
			"leaving %d wallets to the login bringing them up", len(claimed))
		return
	}

	var unreached int
	for _, assetID := range claimed {
		w, resumable := c.resumableWallet(assetID)
		if !resumable || w.connected() {
			continue
		}
		c.trackSuspended(assetID)
		unreached++
	}
	if unreached == 0 {
		c.log.Infof("Session changed (newer resume) while resuming; "+
			"leaving %d wallets to it", len(claimed))
		return
	}
	c.suspended = true
	c.log.Infof("Session changed (newer resume) while resuming; leaving %d wallets to it "+
		"and re-arming %d it could not have claimed", len(claimed)-unreached, unreached)
}

// maxSendFeeUSD caps the estimated network fee of a mainnet send when a fiat
// exchange rate is available. A caller-approved fee rate that prices the
// transaction beyond this is more likely a client bug or a units mixup than
// intent. Sweeps that consume the whole balance are exempt: spending a large
// UTXO set is legitimately expensive, and the alternative is a balance that
// cannot be fully withdrawn during a fee spike.
const maxSendFeeUSD = 100.0

// validateSendFeeRate sanity-checks the caller-approved fee rate for a
// UTXO-wallet send, where the submitted rate is used exactly as given. Three
// checks apply, each guarding a different failure with the reference data it
// has: an absolute per-asset rate ceiling (WalletInfo.MaxFeeRate) that needs no
// live data at all; a factor-of-two band around the wallet's own current
// estimate, when one is available (fee markets move between estimate and
// approval, but not that much); and the maxSendFeeUSD cap on the priced fee,
// when a fiat exchange rate is available.
func (c *Core) validateSendFeeRate(w *xcWallet, address string, value, feeRate uint64, subtract bool) error {
	// The ceiling is the only check that doesn't depend on live reference
	// data, so it is what stands between a units mixup and a burned balance
	// when the fee and fiat oracles are all unavailable. It also can't be
	// satisfied trivially the way the band check can when a poisoned fee
	// source is feeding both the estimate and the validation.
	if maxRate := w.Info().MaxFeeRate; maxRate > 0 && feeRate > maxRate {
		return fmt.Errorf("submitted fee rate %d exceeds the %s sanity ceiling of %d",
			feeRate, unbip(w.AssetID), maxRate)
	}

	if liveRate := w.usableFeeRate(); liveRate > 0 && (feeRate > liveRate*2 || feeRate*2 < liveRate) {
		return fmt.Errorf("submitted fee rate %d is not within a factor of 2 of the current estimate %d",
			feeRate, liveRate)
	}

	estimator, ok := w.Wallet.(asset.TxFeeEstimator)
	if !ok {
		return nil
	}
	fee, _, err := estimator.EstimateSendTxFee(address, value, feeRate, subtract, false)
	if err != nil {
		return fmt.Errorf("fee pre-check failed: %w", err)
	}

	// The USD cap only means anything where the coins have fiat value.
	if c.net != util.Mainnet {
		return nil
	}
	fiatRate := c.fiatConversions()[w.AssetID]
	if fiatRate == 0 {
		return nil
	}
	// value plus fee spanning the entire available balance is the max-withdraw
	// shape — a sweep, exempt from the cap.
	w.mtx.RLock()
	bal := w.balance
	w.mtx.RUnlock()
	if bal != nil && value <= bal.Available && bal.Available-value <= fee {
		return nil
	}
	ui := w.Info().UnitInfo
	feeUSD := float64(fee) / float64(ui.Conventional.ConversionFactor) * fiatRate
	if feeUSD > maxSendFeeUSD {
		return fmt.Errorf("estimated fee %s %s (%.2f USD) exceeds the sanity limit of %.0f USD",
			ui.ConventionalString(fee), ui.Conventional.Unit, feeUSD, maxSendFeeUSD)
	}
	return nil
}

// Send initiates either send or withdraw from an exchange wallet. if subtract
// is true, fees are subtracted from the value else fees are taken from the
// exchange wallet.
// destTag, when non-nil, is an XRPL-style destination tag to include with the
// send. It is a pointer because 0 is a valid tag, distinct from "no tag."
func (c *Core) Send(pw []byte, assetID uint32, value uint64, address string, subtract bool, feeRate uint64, destTag *uint32) (asset.Coin, error) {
	var crypter encrypt.Crypter
	// Empty password can be provided if wallet is already unlocked. Webserver
	// and RPCServer should not allow empty password, but this is used for
	// bots.
	if len(pw) > 0 {
		var err error
		crypter, err = c.encryptionKey(pw)
		if err != nil {
			return nil, fmt.Errorf("trade password error: %w", err)
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

	// feeRate is the caller-approved rate. Only UTXO wallets price a
	// transaction to a specific rate, and they treat it as authoritative, so
	// the fee the user saw is the fee that is paid — sanity-check it first.
	// Account and EVM wallets compute their own fees; the rate is zeroed
	// rather than forwarded so a stray value can't be misinterpreted.
	if wallet.Info().BlockchainClass == asset.BlockchainClassUTXO {
		if err := c.validateSendFeeRate(wallet, address, value, feeRate, subtract); err != nil {
			return nil, err
		}
	} else {
		feeRate = 0
	}

	var coin asset.Coin
	switch {
	case destTag != nil && subtract:
		// Silently dropping the tag here would be a funds-attribution loss on
		// whatever asset first combines a destination tag with a Withdrawer:
		// a tagged exchange deposit that arrives untagged is unattributable.
		return nil, fmt.Errorf("cannot combine a destination tag with subtracting the network fee")
	case destTag != nil:
		tagger, isTagger := wallet.Wallet.(asset.DestinationTagger)
		if !isTagger {
			return nil, fmt.Errorf("%s wallet does not support a destination tag", unbip(assetID))
		}
		coin, err = tagger.SendTagged(address, value, feeRate, *destTag)
	case !subtract:
		coin, err = wallet.Wallet.Send(address, value, feeRate)
	default:
		if withdrawer, isWithdrawer := wallet.Wallet.(asset.Withdrawer); isWithdrawer {
			coin, err = withdrawer.Withdraw(address, value, feeRate)
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

	// Send notification when approval confirms. Runs synchronously on the wallet
	// goroutine that observed the confirmation so the note is not stranded on a
	// detached goroutine at shutdown. This note is Data severity, so notify
	// delivers it to subscribers only and never persists it.
	onConfirm := func() {
		c.notify(newBridgeApprovalNote(wallet.state()))
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

	// Send notification when unapproval confirms. Synchronous for the same
	// reason as in ApproveBridgeContract above.
	onConfirm := func() {
		c.notify(newBridgeApprovalNote(wallet.state()))
	}

	txID, err := wallet.UnapproveBridgeContract(c.ctx, bridgeName, onConfirm)
	if err != nil {
		return "", err
	}

	// Send immediate notification (unapproval pending)
	c.notify(newBridgeApprovalNote(wallet.state()))
	return txID, nil
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
func (c *Core) PendingBridges(assetID uint32) ([]*txdb.WalletTransaction, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	return wallet.PendingBridges()
}

// BridgeHistory returns the bridge history for the given asset ID.
func (c *Core) BridgeHistory(assetID uint32, n int, refID *string, past bool) ([]*txdb.WalletTransaction, error) {
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
			fromFeeAssetID = token.NetworkID
		}
		fees[fromFeeAssetID] += initiationFee
	}

	if completionFee > 0 {
		toFeeAssetID := toAssetID
		if token := asset.TokenInfo(toAssetID); token != nil {
			toFeeAssetID = token.NetworkID
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

// EstimateSendTxFee estimates the network fee for sending or withdrawing the
// specified amount, and reports the fee rate used. The front end submits the
// returned FeeRate to Send so that the fee the user approved is the fee that is
// charged.
func (c *Core) EstimateSendTxFee(address string, assetID uint32, amount uint64, subtract, maxWithdraw bool) (*SendFeeEstimate, error) {
	if amount == 0 {
		return nil, fmt.Errorf("cannot check fee for zero %s", unbip(assetID))
	}

	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}
	if !wallet.connected() {
		return nil, fmt.Errorf("%s wallet is not connected", unbip(assetID))
	}

	if !wallet.traits.IsTxFeeEstimator() {
		return nil, fmt.Errorf("wallet does not support fee estimation")
	}

	if subtract && !wallet.traits.IsWithdrawer() {
		return nil, fmt.Errorf("wallet does not support checking network fee for withdrawal")
	}
	estimator, is := wallet.Wallet.(asset.TxFeeEstimator)
	if !is {
		return nil, fmt.Errorf("wallet does not support fee estimation")
	}

	// Determine the rate here and compute the fee against it, so the estimate
	// and the rate the front end submits to Send are consistent. UTXO wallets
	// price at exactly this rate, so a zero rate must error here rather than
	// let the wallet quietly price at a rate other than the one reported.
	// Account and EVM wallets ignore the rate and compute their own.
	feeRate := wallet.usableFeeRate()
	if feeRate == 0 && wallet.Info().BlockchainClass == asset.BlockchainClassUTXO {
		return nil, fmt.Errorf("no %s fee rate is currently available", unbip(assetID))
	}
	fee, isValidAddress, err := estimator.EstimateSendTxFee(address, amount, feeRate, subtract, maxWithdraw)
	if err != nil {
		return nil, err
	}
	return &SendFeeEstimate{
		Fee:          fee,
		FeeRate:      feeRate,
		ValidAddress: isValidAddress,
	}, nil
}

// assetTickerGroup returns the ticker group an asset belongs to: its
// conventional unit, with wrapped natives folded onto their base asset
// (WETH->ETH, WBTC->BTC) so that, for example, WETH on Polygon groups with
// ETH. The front end receives the same value via SupportedAsset.TickerGroup,
// so the stored ticker index and the UI's grouping key agree. An asset with
// no resolvable unit info maps to "", which excludes its transactions from
// the ticker index.
func assetTickerGroup(assetID uint32, log util.Logger) string {
	ui, err := asset.UnitInfo(assetID)
	if err != nil {
		log.Warnf("No unit info for asset ID %d: %v. Transactions for this "+
			"asset will be omitted from ticker-grouped history.", assetID, err)
		return ""
	}
	return normalizeTickerUnit(ui.Conventional.Unit)
}

// normalizeTickerUnit folds wrapped-native conventional units onto their base.
// This is the single source of the mapping: the front end consumes it through
// SupportedAsset.TickerGroup rather than maintaining its own copy.
func normalizeTickerUnit(unit string) string {
	switch unit {
	case "WETH":
		return "ETH"
	case "WBTC":
		return "BTC"
	}
	return unit
}

// TxHistoryByTicker returns merged, time-ordered transaction history for a
// ticker group across every network it settles on (e.g. USDC on Ethereum and
// Polygon), served directly from the master txdb's ticker index.
//
// A ticker group with no wallet is an error rather than an empty history, so a
// caller naming an unknown or unactivated ticker is told so instead of reading
// the empty result as "no transactions yet". This mirrors TxHistory's
// missingWalletErr.
func (c *Core) TxHistoryByTicker(ticker string, req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	ticker = normalizeTickerUnit(ticker)
	if !c.tickerHasWallet(ticker) {
		return nil, newError(missingWalletErr, "no wallet found for ticker %s", ticker)
	}
	return c.txDB.GetTxsByTicker(ticker, req)
}

// tickerHasWallet reports whether any existing wallet's asset belongs to the
// ticker group.
func (c *Core) tickerHasWallet(ticker string) bool {
	c.walletMtx.RLock()
	defer c.walletMtx.RUnlock()
	for assetID := range c.wallets {
		if assetTickerGroup(assetID, c.log) == ticker {
			return true
		}
	}
	return false
}

// TxHistory returns all the transactions a wallet has made. If refID is nil,
// then transactions starting from the most recent are returned (past is
// ignored). If past is true, the transactions prior to the refID are
// returned, otherwise the transactions after the refID are returned. n is the
// number of transactions to return. If n is <= 0, all the transactions will
// be returned.
func (c *Core) TxHistory(assetID uint32, req *txdb.TxHistoryRequest) (*txdb.TxHistoryResponse, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	return wallet.TxHistory(req)
}

// WalletTransaction returns information about a transaction that the wallet
// has made or one in which that wallet received funds. This function supports
// both transaction ID and coin ID.
func (c *Core) WalletTransaction(assetID uint32, txID string) (*txdb.WalletTransaction, error) {
	wallet, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}

	return wallet.WalletTransaction(c.ctx, txID)
}

// AssetBalance retrieves and updates the current wallet balance.
func (c *Core) AssetBalance(assetID uint32) (*WalletBalance, error) {
	wallet, err := c.connectedWallet(assetID)
	if err != nil {
		return nil, fmt.Errorf("%d -> %s wallet error: %w", assetID, unbip(assetID), err)
	}
	return c.walletBalance(wallet)
}

// splitDBWalletsByTokenStatus separates dbWallets into network wallets and
// their token wallets, preserving relative order within each group. Callers
// that need a token's network wallet already registered should load the
// network group first.
func splitDBWalletsByTokenStatus(dbWallets []*db.Wallet) (networkWallets, tokenWallets []*db.Wallet) {
	for _, dbWallet := range dbWallets {
		if asset.TokenInfo(dbWallet.AssetID) != nil {
			tokenWallets = append(tokenWallets, dbWallet)
		} else {
			networkWallets = append(networkWallets, dbWallet)
		}
	}
	return networkWallets, tokenWallets
}

// initialize loads wallet configurations from the database.
func (c *Core) initialize() error {
	// Load wallet configurations. Actual connections are established on Login.
	dbWallets, err := c.db.Wallets()
	if err != nil {
		c.log.Errorf("error loading wallets from database: %v", err)
	}

	// Network wallets must load before their token wallets: loadXCWallet
	// looks up the network wallet via c.wallet(token.NetworkID) and fails if
	// it isn't registered yet. db.Wallets() has no ordering guarantee to rely
	// on for that (its underlying map iteration is randomized), so split and
	// load network wallets first regardless of the order returned.
	networkDBWallets, tokenDBWallets := splitDBWalletsByTokenStatus(dbWallets)

	existingTokenWallets := make(map[uint32]bool)
	loadDBWallets := func(ws []*db.Wallet) {
		for _, dbWallet := range ws {
			tkn := asset.TokenInfo(dbWallet.AssetID)
			if asset.Asset(dbWallet.AssetID) == nil && tkn == nil {
				c.log.Warnf("Wallet for asset %d -> %s no longer supported", dbWallet.AssetID, unbip(dbWallet.AssetID))
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
	}
	loadDBWallets(networkDBWallets)
	loadDBWallets(tokenDBWallets)

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

	bridgeTx := &txdb.BridgeCounterpartTx{
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
		w.processWalletTransactions([]*txdb.WalletTransaction{n.Transaction})
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
	w, found := c.wallet(assetID)
	if found {
		w.mtx.Lock()
		ss := *w.syncStatus
		ss.Blocks = tip
		w.syncStatus = &ss
		w.mtx.Unlock()
		c.updateBalances(assetMap{assetID: {}})
	}
}

// EstablishTrustline submits the opt-in transaction that lets a token
// wallet's account receive its token, for tokens that need one
// (asset.TrustlineEstablisher). It commits native funds, so it is gated like
// Send: pw may be empty when the wallet is already unlocked. The returned ID
// is empty when the line already exists.
func (c *Core) EstablishTrustline(pw []byte, assetID uint32) (string, error) {
	var crypter encrypt.Crypter
	if len(pw) > 0 {
		var err error
		crypter, err = c.encryptionKey(pw)
		if err != nil {
			return "", fmt.Errorf("password error: %w", err)
		}
		defer crypter.Close()
	}
	wallet, found := c.wallet(assetID)
	if !found {
		return "", newError(missingWalletErr, "no wallet found for %s", unbip(assetID))
	}
	establisher, ok := wallet.Wallet.(asset.TrustlineEstablisher)
	if !ok {
		return "", fmt.Errorf("%s does not require a trust line", unbip(assetID))
	}
	if err := c.connectAndUnlock(crypter, wallet); err != nil {
		return "", err
	}
	if err := wallet.checkPeersAndSyncStatus(); err != nil {
		return "", err
	}
	txID, err := establisher.EstablishTrustline()
	if err != nil {
		return "", err
	}
	// The wallet announces the new trust-line state itself; refresh the
	// native balance so the reserve change shows up alongside it.
	if token := asset.TokenInfo(assetID); token != nil {
		c.updateAssetBalance(token.NetworkID)
	}
	return txID, nil
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
	c.retryAutoDisabledRateSources()

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
		c.notify(newFiatRatesUpdate(fiatRatesMap, c.tickerConversions(fiatRatesMap)))
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

// tickerConversions collapses per-asset fiat rates into one rate per ticker
// group, keyed by the normalized ticker (see assetTickerGroup). A group's
// variants are the same asset economically (USDC on Ethereum and Polygon, ETH
// and WETH), so their rates are averaged into a single entry.
func (c *Core) tickerConversions(fiatRates map[uint32]float64) map[string]float64 {
	type acc struct {
		sum float64
		n   int
	}
	groups := make(map[string]*acc)
	for assetID, rate := range fiatRates {
		ticker := assetTickerGroup(assetID, c.log)
		if ticker == "" {
			continue
		}
		g := groups[ticker]
		if g == nil {
			g = new(acc)
			groups[ticker] = g
		}
		g.sum += rate
		g.n++
	}
	tickerRates := make(map[string]float64, len(groups))
	for ticker, g := range groups {
		tickerRates[ticker] = g.sum / float64(g.n)
	}
	return tickerRates
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

	// An explicit enable takes the source back from the expiry retry.
	delete(c.autoDisabledRateSources, source)

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

	// A source removed for expired data is not a user preference. An explicit
	// disable takes ownership of it, so the entry is dropped and the disable is
	// persisted rather than being undone by the expiry retry.
	_, autoDisabled := c.autoDisabledRateSources[source]
	delete(c.autoDisabledRateSources, source)

	if c.fiatRateSources[source] == nil && !autoDisabled {
		return nil // already disabled.
	}

	// Remove fiat rate source.
	delete(c.fiatRateSources, source)

	// Save disabled fiat rate sources to database.
	c.saveDisabledRateSources()

	c.log.Infof("Disabled %s from fetching fiat rates.", source)
	return nil
}

// initFiatRateSources populates the fiat rate sources with every fetcher the
// user has not disabled. It runs during Run, after c.ready is closed — the app
// server is already up by then and serving requests that read fiatRateSources
// (GET /api/user reaches FiatRateSources), so the map is populated under
// ratesMtx like every other write to it. An unsynchronized write here races the
// first request into a fatal concurrent map access, which no Recoverer catches.
func (c *Core) initFiatRateSources(disabledSources []string) {
	c.ratesMtx.Lock()
	defer c.ratesMtx.Unlock()

fetchers:
	for token, rateFetcher := range fiatRateFetchers {
		for _, v := range disabledSources {
			if token == v {
				continue fetchers
			}
		}
		c.fiatRateSources[token] = newCommonRateSource(rateFetcher)
	}
}

// removeExpiredRateSources disables expired fiat rate source.
func (c *Core) removeExpiredRateSources() {
	c.ratesMtx.Lock()
	defer c.ratesMtx.Unlock()

	// Remove fiat rate source with expired exchange rate data. The removal is
	// recorded rather than persisted: a source only reaches this state by going
	// dark after having worked, which is a transient failure, so it is retried
	// by retryAutoDisabledRateSources instead of being disabled for good.
	var disabledSources []string
	for token, source := range c.fiatRateSources {
		if source.isExpired(fiatRateDataExpiry) {
			delete(c.fiatRateSources, token)
			c.autoDisabledRateSources[token] = time.Now()
			disabledSources = append(disabledSources, token)
		}
	}

	if len(disabledSources) > 0 {
		c.log.Warnf("Rate source(s) serving only expired data, will retry in %v: %v",
			fiatRateSourceRetryInterval, strings.Join(disabledSources, ", "))
	}
}

// retryAutoDisabledRateSources restores rate sources that were removed for
// serving expired data once fiatRateSourceRetryInterval has passed, so that an
// outage at a source does not silently cost the wallet that source forever.
func (c *Core) retryAutoDisabledRateSources() {
	c.ratesMtx.Lock()
	defer c.ratesMtx.Unlock()

	// An explicit enable or disable in the meantime drops the source's entry
	// here, so anything still present is owned by the retry rather than by a
	// user preference.
	var retried []string
	for token, disabledAt := range c.autoDisabledRateSources {
		if time.Since(disabledAt) < fiatRateSourceRetryInterval {
			continue
		}
		delete(c.autoDisabledRateSources, token)
		rateFetcher, found := fiatRateFetchers[token]
		if !found || c.fiatRateSources[token] != nil {
			continue
		}
		c.fiatRateSources[token] = newCommonRateSource(rateFetcher)
		retried = append(retried, token)
	}

	if len(retried) > 0 {
		c.log.Infof("Retrying rate source(s) after expiry: %v", strings.Join(retried, ", "))
	}
}

// saveDisabledRateSources saves disabled fiat rate sources to database and
// shuts down rate fetching if there are no exchange rate source. Use under
// ratesMtx lock.
func (c *Core) saveDisabledRateSources() {
	var disabled []string
	for token := range fiatRateFetchers {
		// Skip sources removed for expired data. They are absent from
		// fiatRateSources but are awaiting a retry, not disabled by the user,
		// and persisting them here would make an outage permanent the next time
		// any source is toggled.
		if _, autoDisabled := c.autoDisabledRateSources[token]; autoDisabled {
			continue
		}
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

// WalletFeeState returns an on-demand fee-rate estimate for the asset, computed
// when called rather than cached or pushed. Front ends should call this only
// when a fee figure is needed for display. The returned Rate may be zero when
// no estimate is currently available, in which case callers should present it
// as unavailable rather than as a zero rate.
func (c *Core) WalletFeeState(assetID uint32) (*FeeState, error) {
	w, found := c.wallet(assetID)
	if !found {
		return nil, newError(missingWalletErr, "no wallet for %s", unbip(assetID))
	}
	// FeeRate implementations assume Connect-time state (contexts, cached
	// tips), so don't call into a wallet that isn't connected — and a display
	// poll is no reason to connect one.
	if !w.connected() {
		return nil, fmt.Errorf("%s wallet is not connected", unbip(assetID))
	}
	// Return the rate the wallet would actually use, so the number the user
	// approves is the number that is charged.
	rate := w.usableFeeRate()
	return &FeeState{
		Rate:    rate,
		Send:    w.StandardSendFee(rate),
		StampMS: time.Now().UnixMilli(),
	}, nil
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
