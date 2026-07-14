// testUtils provides fixture builders and app-singleton reset helpers shared
// by application.test.ts and the newui/mobile component test suites. It is
// deliberately not re-exported from index.ts so it never ends up in a
// production bundle.
import type { SupportedAsset, User, UnitInfo, WalletState, SyncStatus, WalletDefinition } from './registry'
import app from './application'

export function makeUnitInfo (unit: string, conversionFactor = 1e8): UnitInfo {
  return {
    atomicUnit: 'atoms',
    conventional: { unit, conversionFactor },
    denominations: [{ unit, conversionFactor }],
    feeRateDenom: unit
  }
}

export function makeWalletDefinition (overrides: Partial<WalletDefinition> = {}): WalletDefinition {
  return {
    seeded: true,
    type: '',
    tab: '',
    description: '',
    configpath: '',
    configopts: [],
    noauth: false,
    guidelink: '',
    ...overrides
  }
}

export function makeSyncStatus (overrides: Partial<SyncStatus> = {}): SyncStatus {
  return { synced: true, targetHeight: 0, startingBlocks: 0, blocks: 0, txs: 0, ...overrides }
}

export function makeWallet (assetID: number, overrides: Partial<WalletState> = {}): WalletState {
  return {
    symbol: '',
    assetID,
    version: 0,
    type: '',
    class: '',
    traits: 0,
    open: true,
    running: true,
    disabled: false,
    balance: { available: 0, immature: 0, locked: 0, stamp: '', other: {} },
    address: '',
    encrypted: false,
    peerCount: 0,
    synced: true,
    syncProgress: 1,
    syncStatus: makeSyncStatus(),
    approved: {},
    pendingTxs: {},
    ...overrides
  }
}

// makeAsset builds a minimal SupportedAsset. Pass `wallet` in overrides to
// mark the asset active (has a created wallet); omit it for an inactive
// asset — matches how the real API response distinguishes the two.
export function makeAsset (id: number, symbol: string, overrides: Partial<SupportedAsset> = {}): SupportedAsset {
  const base = {
    id,
    symbol,
    name: symbol,
    unitInfo: makeUnitInfo(symbol)
  }
  return { ...base, ...overrides } as unknown as SupportedAsset
}

export function makeUser (assets: SupportedAsset[], fiatRates: Record<number, number> = {}): User {
  const assetMap: Record<number, SupportedAsset> = {}
  for (const a of assets) assetMap[a.id] = a
  // Mirror core.tickerConversions: collapse per-asset fiat rates to one rate per
  // ticker group, so a fixture with fiatRates also exposes the matching
  // tickerRates that TickerAsset.xcRate reads.
  const tickerRates: Record<string, number> = {}
  for (const [id, rate] of Object.entries(fiatRates)) {
    const a = assetMap[Number(id)]
    if (a) tickerRates[a.tickerGroup || a.unitInfo.conventional.unit] = rate
  }
  return { inited: true, seedgentime: 0, assets: assetMap, fiatRates, tickerRates, net: 0 }
}

// resetApp restores the shared Application singleton to a clean baseline.
// app is a module-level singleton (one instance for the whole process), so
// state set by one test leaks into the next unless explicitly cleared.
export function resetApp () {
  app.user = null
  app.tickerList = []
  app.tickerMap = {}
  app.balanceUpdaters = {}
  app.fiatRateUpdaters = {}
  app.noteReceivers = {}
  app.setForm = () => { }
  app.appSettings = { sendRequiresPassword: false }
  app.pageData = { pageParts: [''], pageRoot: '', data: {}, query: new URLSearchParams() }
}
