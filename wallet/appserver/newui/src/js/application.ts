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
  TipChangeNote
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
    console.log('Boss Wallet, Build', this.commitHash.substring(0, 8))

    ws.connect(getSocketURI(), () => window.location.reload())
    ws.registerRoute('notify', (note: CoreNote) => this.notify(note))

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
    document.title = data?.title || 'Boss Wallet'
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
    return r
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
  // Only assets with a known fiat rate are included in the totals.
  // ratesComplete is false if any wallet asset is missing a rate.
  portfolioTotals (): PortfolioTotals {
    let available = 0
    let locked = 0
    let numWallets = 0
    let ratesComplete = true

    if (!this.user) return { total: 0, available: 0, locked: 0, numWallets: 0, ratesComplete: false }

    for (const asset of Object.values(this.user.assets)) {
      const { wallet } = asset
      if (!wallet) continue
      numWallets++
      const fiatRate = this.user.fiatRates[asset.id]
      if (!fiatRate) {
        ratesComplete = false
        continue
      }
      const cFactor = asset.unitInfo.conventional.conversionFactor
      const bal = wallet.balance
      available += ((bal.available ?? 0) / cFactor) * fiatRate
      locked += ((bal.locked ?? 0) / cFactor) * fiatRate
    }

    return { total: available + locked, available, locked, numWallets, ratesComplete }
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

  wallet (assetID: number) {
    return this.user.assets[assetID].wallet
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
    if (this.user) this.updateUser(note)
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
        user.assets[n.assetID].wallet.balance = n.balance
        Object.values(this.balanceUpdaters).forEach(u => u())
        break
      }
      case 'walletstate':
      case 'walletconfig': {
        const wallet = (note as WalletConfigNote)?.wallet
        if (!wallet) return
        const asset = user.assets[wallet.assetID]
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
        this.user.fiatRates = (note as RateNote).fiatRates
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
            const w = user.assets[note.assetID].wallet
            if (w) w.syncStatus.blocks = note.tip
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
    if (tx.confirmed) delete w.pendingTxs[tx.id]
    else w.pendingTxs[tx.id] = tx
  }
}

function randomKey (): string {
  return Math.random().toString(36).substring(2, 18)
}

function pageFromURL (): string {
  // Remove leading and trailing slashes
  return window.location.pathname.replace(/^\/|\/$/g, '')
}

function pageRoot (page: string): string {
  return page.split('/')[0]
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
