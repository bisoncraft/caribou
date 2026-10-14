import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import app from './application'
import { BalanceNote, Balance, CoreNote, TipChangeNote, WalletConfigNote, WalletNote } from './registry'
import { makeAsset, makeUnitInfo, makeUser, makeWallet, resetApp } from './testUtils'
import ws from './ws'

function baseNote (type: string): CoreNote {
  return { type, topic: '', subject: '', details: '', severity: 0, stamp: 0, acked: false, id: '1', data: {} }
}

describe('Application.loadPage', () => {
  beforeEach(() => {
    resetApp()
  })

  it('routes the empty page to the portfolio', async () => {
    let pd = app.pageData
    app.pageLoaded = d => { pd = d }
    await app.loadPage('')
    expect(pd.pageRoot).toBe('portfolio')

    await app.loadPage('networks/btc')
    expect(pd.pageRoot).toBe('networks')
    expect(pd.pageParts).toEqual(['networks', 'btc'])
  })
})

describe('Application.notify / updateUser', () => {
  beforeEach(() => {
    resetApp()
    app.user = makeUser([makeAsset(42, 'DCR', { wallet: makeWallet(42) })])
  })

  it('dispatches notes to registered noteReceivers', () => {
    const received: CoreNote[] = []
    app.registerNoteFeed(n => received.push(n))
    const note = baseNote('fiatrateupdate')
    app.notify(note)
    expect(received).toEqual([note])
  })

  it('updates a known asset balance and fires balance updaters', () => {
    const updater = vi.fn()
    app.registerBalanceUpdater(updater)
    const note: BalanceNote = { ...baseNote('balance'), assetID: 42, balance: { available: 5, immature: 0, locked: 0, stamp: '', other: {} } }
    app.notify(note)
    expect(app.user.assets[42].wallet.balance.available).toBe(5)
    expect(updater).toHaveBeenCalledOnce()
  })

  // These three call updateUser() directly rather than through notify(),
  // because notify() wraps updateUser in its own try/catch — going through
  // notify() would make the assertion pass even if updateUser's per-branch
  // guard were removed, silently defeating the test.
  it('ignores a balance note for an asset not in the cache instead of throwing', () => {
    const updater = vi.fn()
    app.registerBalanceUpdater(updater)
    const note: BalanceNote = { ...baseNote('balance'), assetID: 999, balance: { available: 5, immature: 0, locked: 0, stamp: '', other: {} } }
    expect(() => app.updateUser(note)).not.toThrow()
    // The known asset (42) must be untouched, and the updater must not have
    // fired for an update that never happened.
    expect(app.user.assets[42].wallet.balance.available).toBe(0)
    expect(updater).not.toHaveBeenCalled()
  })

  it('ignores a walletstate note for an asset not in the cache instead of throwing', () => {
    const note: WalletConfigNote = { ...baseNote('walletstate'), wallet: makeWallet(999) }
    expect(() => app.updateUser(note)).not.toThrow()
    expect(app.user.assets[999]).toBeUndefined()
  })

  it('applies a walletstate note for a known asset', () => {
    const newWallet = makeWallet(42, { synced: true, peerCount: 3 })
    const note: WalletConfigNote = { ...baseNote('walletconfig'), wallet: newWallet }
    app.notify(note)
    expect(app.user.assets[42].wallet.peerCount).toBe(3)
  })

  it('ignores a tipChange note for an asset not in the cache instead of throwing', () => {
    const payload: TipChangeNote = { route: 'tipChange', assetID: 999, tip: 100, data: {} }
    const note: WalletNote = { ...baseNote('walletnote'), payload }
    expect(() => app.updateUser(note)).not.toThrow()
  })

  it('applies a tipChange note for a known asset', () => {
    const payload: TipChangeNote = { route: 'tipChange', assetID: 42, tip: 12345, data: {} }
    const note: WalletNote = { ...baseNote('walletnote'), payload }
    app.notify(note)
    expect(app.user.assets[42].wallet.syncStatus.blocks).toBe(12345)
  })

  it('wallet() returns undefined instead of throwing for an unknown assetID', () => {
    expect(app.wallet(999)).toBeUndefined()
  })

  it('keeps dispatching to noteReceivers even when updateUser throws on a note', () => {
    const received: CoreNote[] = []
    app.registerNoteFeed(n => received.push(n))
    const spy = vi.spyOn(app, 'updateUser').mockImplementation(() => { throw new Error('boom') })
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => { })

    const note = baseNote('fiatrateupdate')
    expect(() => app.notify(note)).not.toThrow()
    expect(received).toEqual([note])
    expect(consoleErr).toHaveBeenCalled()

    spy.mockRestore()
    consoleErr.mockRestore()
  })

  it('unregisterBalanceUpdater stops further updates from firing', () => {
    const updater = vi.fn()
    const key = app.registerBalanceUpdater(updater)
    app.unregisterBalanceUpdater(key)
    const note: BalanceNote = { ...baseNote('balance'), assetID: 42, balance: { available: 1, immature: 0, locked: 0, stamp: '', other: {} } }
    app.notify(note)
    expect(updater).not.toHaveBeenCalled()
  })
})

// FakeSocket stands in for window.WebSocket. close() fires onclose
// synchronously, which is what the browser does for a refused upgrade.
class FakeSocket {
  static instances: FakeSocket[] = []
  onclose: ((evt: any) => void) | null = null
  onopen: (() => void) | null = null
  onmessage: ((evt: any) => void) | null = null
  onerror: ((evt: any) => void) | null = null

  constructor (public url: string) { FakeSocket.instances.push(this) }
  close () { if (this.onclose) this.onclose({ code: 1006 }) }
  send () { /* not exercised here */ }
}

describe('Application session socket', () => {
  // authed drives what /api/user reports, standing in for the server's session
  // state.
  let authed = true

  beforeEach(async () => {
    resetApp()
    vi.useFakeTimers()
    FakeSocket.instances = []
    authed = true
    ;(window as any).log = () => { /* silence ws logging */ }
    ;(window as any).WebSocket = FakeSocket
    ;(window as any).WebSocket.OPEN = 1
    ws.disconnect()
    ws.handlers = {}
    ;(window as any).fetch = vi.fn(async (addr: string) => ({
      status: 200,
      json: async () => addr === '/api/user'
        ? { user: authed ? makeUser([]) : null, inited: true, lang: app.lang, langs: [] }
        : { revision: 'testrevision' }
    }))
    await app.start(() => { /* no page render */ }, () => { /* no re-render */ })
  })

  afterEach(() => {
    ws.disconnect()
    vi.useRealTimers()
  })

  // The server ends sessions on its own (Lock, autolock, a failed biometric
  // Unlock), dropping the socket with it. Nothing on the client drives
  // syncSocket in that case, so without a close handler the retry loop
  // re-attempts an upgrade the server answers with 401 every ~10s forever.
  it('stops reconnecting once the server has ended the session', async () => {
    await app.fetchAppState()
    expect(FakeSocket.instances).toHaveLength(1)

    // The server tears the session down and drops the link.
    authed = false
    FakeSocket.instances[0].close()
    await vi.runAllTimersAsync()

    expect(ws.stopped).toBe(true)
    expect(FakeSocket.instances).toHaveLength(1)
  })

  // The same handler must not turn an ordinary network blip into a logout, nor
  // bypass the reconnect backoff by opening a link from inside onclose.
  it('keeps reconnecting when the session is still live', async () => {
    await app.fetchAppState()
    expect(FakeSocket.instances).toHaveLength(1)

    FakeSocket.instances[0].close()
    await vi.advanceTimersByTimeAsync(11000)

    expect(ws.stopped).toBe(false)
    expect(FakeSocket.instances.length).toBeGreaterThan(1)
  })
})

describe('Application.portfolioTotals', () => {
  // USDC deployed on two networks: one ticker group, two variants, 6 decimals.
  const usdcEthID = 60001
  const usdcPolyID = 966001

  function bal (overrides: Partial<Balance> = {}): Balance {
    return { available: 0, immature: 0, locked: 0, stamp: '', other: {}, ...overrides }
  }

  function usdcVariant (id: number, balance: Balance) {
    return makeAsset(id, `usdc.${id}`, {
      tickerGroup: 'USDC',
      unitInfo: makeUnitInfo('USDC', 1e6),
      wallet: makeWallet(id, { balance })
    })
  }

  // rowValue is how the Portfolio prices a row (see desktop Portfolio.tsx and the
  // mobile Portfolio): the group's conventional total times the group's rate.
  function rowValue (): number {
    return (app.tickerList ?? []).reduce((sum, ta) => sum + ta.total * (ta.xcRate ?? 0), 0)
  }

  beforeEach(() => {
    resetApp()
  })

  it('prices a multi-network group with the group rate, counting a wallet per variant', () => {
    app.user = makeUser(
      [usdcVariant(usdcEthID, bal({ available: 100 * 1e6 })), usdcVariant(usdcPolyID, bal({ available: 50 * 1e6 }))],
      { [usdcEthID]: 1, [usdcPolyID]: 1 }
    )
    app.prepareTickerAssets()

    const totals = app.portfolioTotals()
    expect(totals.total).toBeCloseTo(150)
    expect(totals.available).toBeCloseTo(150)
    expect(totals.numWallets).toBe(2)
    expect(totals.ratesComplete).toBe(true)
  })

  // The header total and the rows must be computed on one basis. Pricing the
  // header per asset from user.fiatRates while the rows price per ticker group
  // let them disagree, which also broke the portfolio-% column.
  it('totals on the same basis as the rows when a group\'s variants quote different rates', () => {
    app.user = makeUser(
      [usdcVariant(usdcEthID, bal({ available: 100 * 1e6 })), usdcVariant(usdcPolyID, bal({ available: 50 * 1e6 }))],
      { [usdcEthID]: 1, [usdcPolyID]: 1.1 } // group rate is the 1.05 average
    )
    app.prepareTickerAssets()

    expect(app.tickerRate('USDC')).toBeCloseTo(1.05)
    const totals = app.portfolioTotals()
    expect(totals.total).toBeCloseTo(157.5) // 150 USDC * 1.05
    expect(rowValue()).toBeCloseTo(totals.total)
  })

  // TickerAsset.total includes immature, so the header has to as well or a row
  // and the total it sums into disagree for any wallet holding immature funds.
  it('counts immature funds in the total but not in available', () => {
    app.user = makeUser(
      [usdcVariant(usdcEthID, bal({ available: 100 * 1e6, locked: 20 * 1e6, immature: 30 * 1e6 }))],
      { [usdcEthID]: 1 }
    )
    app.prepareTickerAssets()

    const totals = app.portfolioTotals()
    expect(totals.available).toBeCloseTo(100)
    expect(totals.locked).toBeCloseTo(20)
    expect(totals.immature).toBeCloseTo(30)
    expect(totals.total).toBeCloseTo(150)
    expect(totals.total).toBeCloseTo(totals.available + totals.locked + totals.immature)
    expect(rowValue()).toBeCloseTo(totals.total)
  })

  it('reports incomplete rates for a wallet group with no rate, without dropping the wallet count', () => {
    app.user = makeUser([usdcVariant(usdcEthID, bal({ available: 100 * 1e6 }))])
    app.prepareTickerAssets()

    const totals = app.portfolioTotals()
    expect(totals.ratesComplete).toBe(false)
    expect(totals.total).toBe(0)
    expect(totals.numWallets).toBe(1)
  })
})
