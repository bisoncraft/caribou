import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import app from './application'
import Doc from './doc'
import { makeAsset, makeUnitInfo, makeUser, makeWallet, resetApp } from './testUtils'
import { IntentsAsset, IntentsSwap, StateKnownDepositTx, StateProcessing, StateSuccess } from './intents'
import { WalletTransaction } from './registry'
import {
  activeStep, bridgeTrust, displayBalance, plainAtoms, plainUSD, slippagePct, swapPickList, swapProgressInfo, unitInfoFor
} from './swap'

function intentsAsset (assetID: number, symbol: string, overrides: Partial<IntentsAsset> = {}): IntentsAsset {
  return {
    assetID,
    symbol,
    intentsID: `nep141:${symbol.toLowerCase()}`,
    decimals: 8,
    atomDecimals: 8,
    priceUSD: 0,
    walletReady: true,
    ...overrides
  }
}

function makeSwap (overrides: Partial<IntentsSwap> = {}): IntentsSwap {
  return {
    depositAddress: 'dep1',
    state: StateKnownDepositTx,
    originAssetID: 0,
    destAssetID: 60,
    originAsset: 'btc',
    destAsset: 'eth',
    atomsIn: 1e8,
    amountIn: '1',
    amountOut: '30',
    atomsOut: 30e9,
    recipient: 'r',
    refundTo: 'f',
    deadline: '',
    timeEstimate: 120,
    createdAt: '',
    updatedAt: '',
    ...overrides
  }
}

beforeEach(() => {
  resetApp()
  app.user = makeUser([
    makeAsset(0, 'btc', { unitInfo: makeUnitInfo('BTC'), wallet: makeWallet(0, { balance: { available: 5e7, immature: 0, locked: 0, stamp: '', other: {} } }) }),
    makeAsset(60, 'eth', { unitInfo: makeUnitInfo('ETH', 1e9), wallet: makeWallet(60, { balance: { available: 1e9, immature: 0, locked: 0, stamp: '', other: {} } }) }),
    makeAsset(2, 'ltc', { unitInfo: makeUnitInfo('LTC') })
  ], { 0: 100000, 60: 3000 })
})

afterEach(() => {
  resetApp()
})

describe('formatting helpers', () => {
  it('renders input-safe amounts without grouping or trailing zeros', () => {
    expect(plainAtoms(150000000, 8)).toBe('1.5')
    expect(plainAtoms(1234567890000, 8)).toBe('12345.6789')
    expect(plainAtoms(0, 8)).toBe('')
    expect(plainUSD(12.345)).toBe('12.35')
    expect(plainUSD(0)).toBe('')
  })

  it('floors balances to four significant digits', () => {
    expect(displayBalance(0)).toBe('0')
    expect(displayBalance(0.123456)).toBe('0.1234')
    expect(displayBalance(0.00098765)).toBe('0.0009876')
  })

  it('renders basis points as a percentage', () => {
    expect(slippagePct(50)).toBe(`${(0.5).toLocaleString()}%`)
    expect(slippagePct(100)).toBe('1%')
  })

  it('classifies bridge custody per chain with a generic fallback', () => {
    expect(bridgeTrust('btc')).toBe('bridge custody by a distributed MPC signing network (Omni)')
    expect(bridgeTrust('POL')).toBe('bridge custody by a distributed MPC signing network (HOT)')
    expect(bridgeTrust('XYZ')).toBe('bridge custody by a third-party bridge operator')
  })

  it('builds a stand-in unit info for an asset unknown to the build', () => {
    const ui = unitInfoFor(intentsAsset(999, 'wbtc', { atomDecimals: 6 }))
    expect(ui.conventional).toEqual({ unit: 'WBTC', conversionFactor: 1e6 })
    expect(unitInfoFor(intentsAsset(0, 'btc'))).toBe(app.user?.assets[0].unitInfo)
  })
})

describe('swapPickList', () => {
  const assets = [
    intentsAsset(2, 'ltc'),
    intentsAsset(999, 'xyz', { walletReady: false }),
    intentsAsset(60, 'eth', { atomDecimals: 9 }),
    intentsAsset(0, 'btc')
  ]

  it('orders ready wallets by USD balance with inactive entries last', () => {
    // BTC: 0.5 * 100k = 50k; ETH: 1 * 3k = 3k; LTC: no balance.
    expect(swapPickList(assets, '').map(a => a.assetID)).toEqual([0, 60, 2, 999])
  })

  it('filters by ticker, name, or catalog symbol', () => {
    expect(swapPickList(assets, ' Et ').map(a => a.assetID)).toEqual([60])
    expect(swapPickList(assets, 'xyz').map(a => a.assetID)).toEqual([999])
    expect(swapPickList(assets, 'nothing')).toEqual([])
  })
})

describe('swap progress', () => {
  it('maps processing to the swapping step and earlier states to deposit', () => {
    expect(activeStep(StateProcessing)).toBe(1)
    expect(activeStep(StateKnownDepositTx)).toBe(0)
  })

  it('derives display values from the record', () => {
    const info = swapProgressInfo(makeSwap({ appFees: [{ recipient: 'a', fee: 20 }, { recipient: 'b', fee: 5 }] }), null)
    expect(info.active).toBe(true)
    expect(info.depositRejected).toBe(false)
    expect(info.originTicker).toBe('BTC')
    expect(info.destTicker).toBe('ETH')
    expect(info.rateText).toBe(`1 BTC = ${Doc.formatFourSigFigs(30)} ETH`)
    expect(info.feeBps).toBe(25)
    expect(info.feeAsset).toBe(app.user?.assets[0])
  })

  it('flags an on-chain rejected deposit only while the record is active', () => {
    const tx = { fees: 1000, rejected: true } as WalletTransaction
    expect(swapProgressInfo(makeSwap(), tx).depositRejected).toBe(true)
    expect(swapProgressInfo(makeSwap({ state: StateSuccess }), tx).depositRejected).toBe(false)
  })
})
