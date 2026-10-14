import { getJSON, postJSON } from './http'
import {
  AppState,
  AppSettings,
  User,
  PageData,
  UnitInfo,
  PortfolioTotals,
  FormData,
  CoreNote,
  LogMessage,
  BalanceNote,
  WalletNote,
  TransactionNote,
  WalletConfigNote,
  WalletSyncNote,
  RateNote,
  TipChangeNote,
  TrustlineNote
} from './registry'
import { TickerAsset, normalizedTicker } from './assets'
import Doc from './doc'
import State from './state'
import ws from './ws'
import { loadLocale } from './intl'

export class Application {
  user: User | null
  commitHash: string
  langs: string[]
  lang: string
  inited: boolean
  authed: boolean
  appSettings: AppSettings
  pageLoaded: (d: PageData) => void
  reRender: () => void
  pageData: PageData
  tickerList: TickerAsset[]
  tickerMap: Record<string, TickerAsset>
  balanceUpdaters: Record<string, (() => void)>
  fiatRateUpdaters: Record<string, (() => void)>
  setForm: (form: FormData) => void
  noteReceivers: Record<string, (n: CoreNote) => void>
  loggers: Record<string, boolean>
  recorders: Record<string, LogMessage[]>

  constructor () {
    this.user = null
    this.appSettings = { sendRequiresPassword: false }
    this.pageData = { pageParts: [''], pageRoot: '', data: {}, query: new URLSearchParams() }
    this.balanceUpdaters = {}
    this.fiatRateUpdaters = {}
    this.noteReceivers = {}
    this.setForm = () => { }

    // Loggers can be enabled by setting a truthy value to the loggerID using
    // enableLogger. Settings are stored across sessions. See docstring for the
    // log method for more info.
    this.loggers = State.fetchLocal(State.loggersLK) || {}
    window.enableLogger = (loggerID, state) => {
      if (state) this.loggers[loggerID] = true
      else delete this.loggers[loggerID]
      State.storeLocal(State.loggersLK, this.loggers)
      return `${loggerID} logger ${state ? 'enabled' : 'disabled'}`
    }
    // Enable logging from anywhere.
    window.log = (loggerID, ...a) => { this.log(loggerID, ...a) }

    // Recorders can record log messages, and then save them to file on request.
    const recorderKeys = State.fetchLocal(State.recordersLK) || []
    this.recorders = {}
    for (const loggerID of recorderKeys) {
      console.log('recording', loggerID)
      this.recorders[loggerID] = []
    }
    window.recordLogger = (loggerID, on) => {
      if (on) this.recorders[loggerID] = []
      else delete this.recorders[loggerID]
      State.storeLocal(State.recordersLK, Object.keys(this.recorders))
      return `${loggerID} recorder ${on ? 'enabled' : 'disabled'}`
    }
    window.dumpLogger = loggerID => {
      const record = this.recorders[loggerID]
      if (!record) return `no recorder for logger ${loggerID}`
      const a = document.createElement('a')
      a.href = `data:application/octet-stream;base64,${window.btoa(JSON.stringify(record, null, 4))}`
      a.download = `${loggerID}.json`
      document.body.appendChild(a)
      a.click()
      setTimeout(() => {
        document.body.removeChild(a)
      }, 0)
    }

    window.user = () => this.user
  }

  async start (pageLoaded: (d: PageData) => void, reRender: () => void) {
    this.pageLoaded = pageLoaded
    this.reRender = reRender
    await this.fetchBuildInfo()
    console.log('Caribou, Build', this.commitHash.substring(0, 8))

    // The socket itself is opened by syncSocket once a session exists; the
    // route registration survives disconnects, so it is only done here.
    ws.registerRoute('notify', (note: CoreNote) => this.notify(note))
    // The server ends sessions on its own (Lock, autolock, a failed biometric
    // Unlock) and drops the socket with it. Nothing else would notice: without
    // this the retry loop re-attempts the upgrade forever, collecting 401s.
    // Refetching the session state converges syncSocket to disconnect() when
    // the session is really gone, and is a no-op on a transient drop.
    ws.registerRoute('close', () => this.fetchAppState())

    // Handle back navigation from the browser.
    Doc.bind(window, 'popstate', (e: PopStateEvent) => {
      const page = e.state?.page
      if (!page && page !== '') return
      this.loadPage(page, e.state.data, true)
    })

    const page = pageFromURL()
    const lastState = window.history.state
    if (lastState?.page === page) { // probably a refresh
      this.loadPage(page, lastState.data, true)
    } else this.loadPage(page)
  }

  async loadPage (page: string, data?: any, skipPush?: boolean) {
    const url = new URL(`/${page}`, window.location.origin)
    if (!skipPush) {
      window.history.pushState({ page: page, data: data }, '', url.toString())
    }
    document.title = data?.title || 'Caribou'
    this.storePageData({ pageParts: page.split('/'), pageRoot: pageRoot(page), data: data, query: url.searchParams })
  }

  storePageData (pd: PageData) {
    this.pageData = pd
    this.pageLoaded(pd)
  }

  async fetchBuildInfo () {
    const resp = await getJSON('/api/buildinfo')
    if (!resp.ok) return
    this.commitHash = resp.revision
  }

  async fetchAppState (): Promise<AppState> {
    const r = await getJSON('/api/user')
    const needsTickers = r.user && !this.user
    this.user = r.user
    this.inited = r.inited
    this.authed = Boolean(r.user)
    if (r.lang !== this.lang) {
      await loadLocale(r.lang, this.commitHash, false)
    }
    this.lang = r.lang
    this.langs = r.langs
    if (r.appSettings) this.appSettings = r.appSettings
    if (needsTickers) this.prepareTickerAssets()
    this.syncSocket()
    return r
  }

  /*
   * syncSocket brings the notification socket in line with the session state.
   * The server serves /ws only to an authenticated session, so connecting
   * before login just burns retries — and a retry landing after login would
   * count as a reconnect and reload the page out from under the user.
   */
  syncSocket () {
    if (this.authed) ws.connect(getSocketURI(), () => window.location.reload())
    else ws.disconnect()
  }

  async changeLocale (lang: string) {
    await postJSON('/api/setlocale', lang)
    await loadLocale(lang, this.commitHash, false)
    this.lang = lang
  }

  async fetchUser (): Promise<User | null> {
    const r = await this.fetchAppState()
    return r.user
  }

  registerNoteFeed (receivers: (n: CoreNote) => void): string {
    const k = randomKey()
    this.noteReceivers[k] = receivers
    return k
  }

  unregisterNoteFeed (k: string) {
    delete this.noteReceivers[k]
  }

  prepareTickerAssets () {
    const tickerList: TickerAsset[] = []
    const tickerMap: Record<string, TickerAsset> = {}
    for (const a of Object.values(this.user.assets)) {
      const normedTicker = normalizedTicker(a)
      let ta = tickerMap[normedTicker]
      if (ta) {
        ta.addVariant(a)
        continue
      }
      ta = new TickerAsset(a)
      tickerList.push(ta)
      tickerMap[normedTicker] = ta
    }
    this.tickerList = tickerList
    this.tickerMap = tickerMap
  }

  // portfolioTotals returns a breakdown of the portfolio value in USD.
  // Only ticker groups with a known fiat rate are included in the totals.
  // ratesComplete is false if any group holding a wallet is missing a rate.
  //
  // Totals are priced per ticker group with the group's rate, the same basis
  // the portfolio rows use (TickerAsset.xcRate). Pricing the header per asset
  // with user.fiatRates while the rows price per group would let the rows, the
  // header, and the portfolio-% column disagree with each other.
  portfolioTotals (): PortfolioTotals {
    let available = 0
    let locked = 0
    let immature = 0
    let numWallets = 0
    let ratesComplete = true

    if (!this.user) return { total: 0, available: 0, locked: 0, immature: 0, numWallets: 0, ratesComplete: false }

    for (const ta of this.tickerList ?? []) {
      // numWallets counts wallets, not groups: a group spanning several
      // networks holds one wallet per variant.
      for (const v of ta.variants) if (this.wallet(v.assetID)) numWallets++
      if (!ta.hasWallets) continue
      const fiatRate = this.tickerRate(ta.ticker)
      if (!fiatRate) {
        ratesComplete = false
        continue
      }
      // avail, locked and immature are already conventional units summed across
      // the group's variants. Immature counts toward the total but not toward
      // available, matching TickerAsset.total, which the portfolio rows price:
      // were it excluded here, a row's value and the header it sums into would
      // disagree for any wallet holding immature funds.
      available += ta.avail * fiatRate
      locked += ta.locked * fiatRate
      immature += ta.immature * fiatRate
      // TODO: immature is counted in `total` but not surfaced on its own
      // anywhere in desktop. Immature is funds the wallet owns but that are not
      // yet spendable pending confirmations, e.g. coinbase outputs or
      // transaction change, so a user watching the "Available" stat sees the
      // funds vanish until they mature even though the total still holds them.
      //
      // This is most visible on XMR: Monero spends whole outputs, so every send
      // returns the remainder as a change output that is locked for 10
      // confirmations (CRYPTONOTE_DEFAULT_TX_SPENDABLE_AGE). Send from a single
      // large output and almost the entire balance drops to immature for ~10
      // blocks, then reappears. Coinbase outputs lock for 60 blocks. The XMR
      // wallet reports this correctly as Balance.Immature (see
      // wallet/asset/xmr/xmr.go Balance: Immature = total - unlocked); the gap
      // is purely on the display side.
      //
      // Remaining fix is presentational: show a distinct "Maturing"/"Immature"
      // line in PortfolioTotals and in the per-wallet balance view, reading
      // PortfolioTotals.immature, so users see the funds are still theirs and
      // simply pending. Keep it out of `available` (it is not spendable).
    }

    return { total: available + locked + immature, available, locked, immature, numWallets, ratesComplete }
  }

  asset (assetID: number) {
    return this.user.assets[assetID]
  }

  unitInfo (assetID: number): UnitInfo {
    return this.user.assets[assetID].unitInfo
  }

  idRate (assetID: number) {
    return this.user.fiatRates[assetID]
  }

  tickerRate (ticker: string) {
    return this.user.tickerRates[ticker]
  }

  wallet (assetID: number) {
    return this.user.assets[assetID]?.wallet
  }

  registerBalanceUpdater (updater: () => void): string {
    const key = randomKey()
    this.balanceUpdaters[key] = updater
    return key
  }

  unregisterBalanceUpdater (key: string) {
    delete this.balanceUpdaters[key]
  }

  registerFiatRateUpdater (updater: () => void): string {
    const key = randomKey()
    this.fiatRateUpdaters[key] = updater
    return key
  }

  unregisterFiatRateUpdater (key: string) {
    delete this.fiatRateUpdaters[key]
  }

  registerFormSetter (setForm: (form: FormData) => void) {
    this.setForm = setForm
  }

  unregisterFormSetter () {
    this.setForm = () => { }
  }

  /*
 * log prints to the console if a logger has been enabled. Loggers are created
 * implicitly by passing a loggerID to log. i.e. you don't create a logger,
 * you just log to it. Loggers are enabled by invoking a global function,
 * enableLogger(loggerID, onOffBoolean), from the browser's js console. Your
 * choices are stored across sessions. Some common and useful loggers are
 * listed below, but this list is not meant to be comprehensive.
 *
 * LoggerID   Description
 * --------   -----------
 * notes      Notifications of all levels.
 * ws.........Websocket connection status changes.
 */
  log (loggerID: string, ...msg: any) {
    if (this.loggers[loggerID]) console.log(`${nowString()}[${loggerID}]:`, ...msg)
    if (this.recorders[loggerID]) {
      this.recorders[loggerID].push({
        time: nowString(),
        msg: msg
      })
    }
  }

  /*
 * notify is the top-level handler for notifications received from the client.
 * Notifications are propagated to the loadedPage.
 */
  notify (note: CoreNote) {
    // Handle type-specific updates.
    this.log('notes', 'notify', note)
    if (this.user) {
      try {
        this.updateUser(note)
      } catch (error) {
        console.error('updateUser error:', error.message ? error.message : error)
        console.log(note)
        console.log(error.stack)
      }
    }
    // Inform the view.
    for (const f of Object.values(this.noteReceivers)) {
      try {
        f(note)
      } catch (error) {
        console.error('note feeder error:', error.message ? error.message : error)
        console.log(note)
        console.log(error.stack)
      }
    }
  }

  updateUser (note: CoreNote) {
    const { user } = this
    switch (note.type) {
      case 'balance': {
        const n: BalanceNote = note as BalanceNote
        const asset = user.assets[n.assetID]
        if (!asset) {
          console.error(`balance note for unknown asset ${n.assetID}`)
          break
        }
        asset.wallet.balance = n.balance
        Object.values(this.balanceUpdaters).forEach(u => u())
        break
      }
      case 'walletstate':
      case 'walletconfig': {
        const wallet = (note as WalletConfigNote)?.wallet
        if (!wallet) return
        const asset = user.assets[wallet.assetID]
        if (!asset) {
          console.error(`${note.type} note for unknown asset ${wallet.assetID}`)
          break
        }
        asset.wallet = wallet
        // Refresh every ticker that contains this asset as a variant OR as a
        // token's parent network — creating ETH unlocks every ERC-20 ticker.
        for (const ta of this.tickerList ?? []) {
          if (ta.variantLookup[wallet.assetID] ||
              ta.variants.some(v => v.token?.networkID === wallet.assetID)) {
            ta.updateHasWallets()
          }
        }
        break
      }
      case 'walletsync': {
        const n = note as WalletSyncNote
        const w = this.wallet(n.assetID)
        if (w) {
          w.syncStatus = n.syncStatus
          w.synced = w.syncStatus.synced
          w.syncProgress = n.syncProgress
        }
        break
      }
      case 'fiatrateupdate': {
        const rn = note as RateNote
        this.user.fiatRates = rn.fiatRates
        this.user.tickerRates = rn.tickerRates
        Object.values(this.fiatRateUpdaters).forEach(u => u())
        break
      }
      case 'walletnote': {
        const n = note as WalletNote
        switch (n.payload.route) {
          case 'transaction': {
            const txNote = n.payload as TransactionNote
            this.handleTransactionNote(n.payload.assetID, txNote)
            break
          }
          case 'actionRequired': {
            // TODO: implement required actions
            break
          }
          case 'actionResolved': {
            // TODO: implement
            break
          }
          case 'tipChange': {
            const note = n.payload as TipChangeNote
            const w = user.assets[note.assetID]?.wallet
            if (w) w.syncStatus.blocks = note.tip
            break
          }
          case 'trustline': {
            const note = n.payload as TrustlineNote
            const w = user.assets[note.assetID]?.wallet
            if (w) w.trustline = note.payload
            break
          }
        }
        break
      }
    }
  }

  handleTransactionNote (assetID: number, note: TransactionNote) {
    const w = this.wallet(assetID)
    if (!w) return
    const tx = note.transaction
    if (tx.confirmed) {
      const pendingID = tx.additionalData?.pendingID
      if (pendingID) delete w.pendingTxs[pendingID]
      delete w.pendingTxs[tx.id]
    } else w.pendingTxs[tx.id] = tx
  }
}

function randomKey (): string {
  return Math.random().toString(36).substring(2, 18)
}

function pageFromURL (): string {
  // Remove leading and trailing slashes
  return window.location.pathname.replace(/^\/|\/$/g, '')
}

/*
 * pageRoot maps a page path to the view that renders it. The empty page — a
 * visit to the bare origin, or a URL reset — is the portfolio. Defining the
 * default route here, at the one point every navigation flows through, keeps
 * each entry path (fresh authed visit, unlock, init) from needing its own
 * redirect.
 */
function pageRoot (page: string): string {
  return page.split('/')[0] || 'portfolio'
}

/* nowString creates a string formatted like HH:MM:SS.xxx */
function nowString (): string {
  const stamp = new Date()
  const h = stamp.getHours().toString().padStart(2, '0')
  const m = stamp.getMinutes().toString().padStart(2, '0')
  const s = stamp.getSeconds().toString().padStart(2, '0')
  const ms = stamp.getMilliseconds().toString().padStart(3, '0')
  return `${h}:${m}:${s}.${ms}`
}

/* getSocketURI returns the websocket URI for the client. */
function getSocketURI (): string {
  const protocol = (window.location.protocol === 'https:') ? 'wss' : 'ws'
  return `${protocol}://${window.location.host}/ws`
}

const app = new Application()

export default app
