import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { ChangeEvent } from 'react'
import app from './application'
import { getJSON, postJSON } from './http'
import { makeAsset, makeUnitInfo, makeUser, makeWallet, resetApp } from './testUtils'
import { IntentsAsset, IntentsPreview } from './intents'
import { useSwapForm, UseSwapFormOptions } from './useSwapForm'

vi.mock('./http', () => ({ getJSON: vi.fn(), postJSON: vi.fn() }))

const catalog: IntentsAsset[] = [
  { assetID: 0, symbol: 'btc', intentsID: 'btc', decimals: 8, atomDecimals: 8, priceUSD: 100000, walletReady: true },
  { assetID: 60, symbol: 'eth', intentsID: 'eth', decimals: 18, atomDecimals: 9, priceUSD: 3000, walletReady: true },
  { assetID: 2, symbol: 'ltc', intentsID: 'ltc', decimals: 8, atomDecimals: 8, priceUSD: 100, walletReady: true }
]

function balance (available: number) {
  return { available, immature: 0, locked: 0, stamp: '', other: {} }
}

function previewFor (atomsOut: number): IntentsPreview {
  return {
    atomsIn: 5e7,
    atomsOut,
    minAtomsOut: atomsOut - 1000,
    minAtomsIn: 0,
    amountInUSD: '',
    amountOutUSD: '',
    timeEstimate: 120,
    appFees: [],
    refundFee: '',
    withdrawFee: ''
  }
}

// deferred returns a promise and its resolver, for responses a test releases
// out of order.
function deferred<T> () {
  let release: (v: T) => void = () => {}
  const promise = new Promise<T>(resolve => { release = resolve })
  return { promise, resolve: (v: T) => release(v) }
}

function typed (v: string) {
  return { target: { value: v } } as ChangeEvent<HTMLInputElement>
}

// flush lets resolved fetches settle and their state updates apply.
async function flush () {
  await act(async () => { await Promise.resolve() })
}

async function renderForm (opts: Partial<UseSwapFormOptions> = {}) {
  const onExecuted = vi.fn()
  const hook = renderHook(() => useSwapForm({ liveView: 'form', onExecuted, ...opts }))
  await flush()
  return { ...hook, onExecuted }
}

beforeEach(() => {
  vi.useFakeTimers()
  localStorage.clear()
  resetApp()
  app.user = makeUser([
    // BTC: 0.5 * 100k = 50k; ETH: 2 * 3k = 6k; LTC: unfunded.
    makeAsset(0, 'btc', { unitInfo: makeUnitInfo('BTC'), wallet: makeWallet(0, { balance: balance(5e7) }) }),
    makeAsset(60, 'eth', { unitInfo: makeUnitInfo('ETH', 1e9), wallet: makeWallet(60, { balance: balance(2e9) }) }),
    makeAsset(2, 'ltc', { unitInfo: makeUnitInfo('LTC'), wallet: makeWallet(2) })
  ])
  vi.mocked(getJSON).mockResolvedValue({ ok: true, assets: catalog })
  vi.mocked(postJSON).mockImplementation(async (path: string) => {
    if (path === '/api/feestate') return { ok: true, feeState: { send: 1000 } }
    return { ok: false }
  })
})

afterEach(() => {
  vi.useRealTimers()
  vi.mocked(getJSON).mockReset()
  vi.mocked(postJSON).mockReset()
  resetApp()
})

describe('useSwapForm pair selection', () => {
  it('defaults to the two most valuable ready wallets', async () => {
    const { result } = await renderForm()
    expect(result.current.sellID).toBe(0)
    expect(result.current.buyID).toBe(60)
  })

  it('restores the last-used pair', async () => {
    localStorage.setItem('intentsPair', JSON.stringify({ sellID: 2, buyID: 0 }))
    const { result } = await renderForm()
    expect(result.current.sellID).toBe(2)
    expect(result.current.buyID).toBe(0)
  })

  it('puts a requested held asset on the sell side', async () => {
    const { result } = await renderForm({ ticker: 'eth' })
    expect(result.current.sellID).toBe(60)
    expect(result.current.buyID).toBe(0)
  })

  it('puts a requested unfunded asset on the buy side', async () => {
    const { result } = await renderForm({ ticker: 'LTC' })
    expect(result.current.sellID).toBe(0)
    expect(result.current.buyID).toBe(2)
  })

  it('surfaces an unavailable engine', async () => {
    vi.mocked(getJSON).mockResolvedValue({ ok: false, msg: 'intents engine not running' })
    const { result } = await renderForm()
    expect(result.current.assetsErr).toBe('intents engine not running')
    expect(result.current.assets).toBeNull()
  })
})

describe('useSwapForm quoting', () => {
  it('applies only the newest preview response', async () => {
    const first = deferred<any>()
    const second = deferred<any>()
    const pending = [first, second]
    vi.mocked(postJSON).mockImplementation(async (path: string) => {
      if (path === '/api/feestate') return { ok: true, feeState: { send: 1000 } }
      if (path === '/api/intents/preview') return pending.shift()?.promise
      return { ok: false }
    })
    const { result } = await renderForm()
    expect(result.current.formStatus).toBe('amount')

    act(() => result.current.amountChanged('sell', 'asset')(typed('0.1')))
    await act(async () => { vi.advanceTimersByTime(350) })
    expect(result.current.formStatus).toBe('quoting')

    act(() => result.current.amountChanged('sell', 'asset')(typed('0.2')))
    await act(async () => { vi.advanceTimersByTime(350) })

    second.resolve({ ok: true, preview: previewFor(6e9) })
    await flush()
    first.resolve({ ok: true, preview: previewFor(3e9) })
    await flush()

    expect(result.current.preview?.atomsOut).toBe(6e9)
    expect(result.current.formStatus).toBe('ready')
    expect(result.current.buyAssetValue).toBe('6')
  })

  it('reports an amount over the available balance', async () => {
    const { result } = await renderForm()
    act(() => result.current.amountChanged('sell', 'asset')(typed('0.75')))
    expect(result.current.overBalance).toBe(true)
    expect(result.current.formStatus).toBe('balance')
  })

  // A native asset pays the deposit fee from the swapped balance, so Max
  // leaves room for it and the full balance counts as over.
  it('keeps the deposit fee out of Max on a native sell asset', async () => {
    const { result } = await renderForm()
    await flush()
    act(() => result.current.setMax())
    expect(result.current.amount).toBe('0.49999')
    expect(result.current.overBalance).toBe(false)
    act(() => result.current.amountChanged('sell', 'asset')(typed('0.5')))
    expect(result.current.overBalance).toBe(true)
  })

  it('rejects malformed entry and accepts a comma decimal', async () => {
    const { result } = await renderForm()
    act(() => result.current.amountChanged('sell', 'asset')(typed('1,5')))
    expect(result.current.amount).toBe('1.5')
    act(() => result.current.amountChanged('sell', 'asset')(typed('1.5.')))
    expect(result.current.amount).toBe('1.5')
  })

  it('schedules refreshes only while a live view shows', async () => {
    const { result } = await renderForm()
    act(() => result.current.amountChanged('sell', 'asset')(typed('0.1')))
    expect(result.current.nextRefreshAt).toBeGreaterThan(0)

    const { result: idle } = await renderForm({ liveView: null })
    act(() => idle.current.amountChanged('sell', 'asset')(typed('0.1')))
    expect(idle.current.nextRefreshAt).toBe(0)
  })
})

describe('useSwapForm execute', () => {
  it('posts the accepted bound and hands over the swap record', async () => {
    const swap = { depositAddress: 'dep1' }
    vi.mocked(postJSON).mockImplementation(async (path: string) => {
      if (path === '/api/feestate') return { ok: true, feeState: { send: 1000 } }
      if (path === '/api/intents/preview') return { ok: true, preview: previewFor(6e9) }
      if (path === '/api/intents/execute') return { ok: true, swap }
      return { ok: false }
    })
    const { result, onExecuted } = await renderForm()
    act(() => result.current.amountChanged('sell', 'asset')(typed('0.1')))
    await act(async () => { vi.advanceTimersByTime(350) })
    await flush()
    expect(result.current.canExecute).toBe(true)

    await act(async () => { await result.current.execute() })
    expect(postJSON).toHaveBeenCalledWith('/api/intents/execute', {
      originAssetID: 0,
      destAssetID: 60,
      value: 1e7,
      slippageBps: 100,
      exactOutput: false,
      minAtomsOut: 6e9 - 1000,
      maxAtomsIn: 0,
      pass: ''
    })
    expect(onExecuted).toHaveBeenCalledWith(swap)
  })

  it('cannot execute an amount over the balance even with a quote', async () => {
    vi.mocked(postJSON).mockImplementation(async (path: string) => {
      if (path === '/api/feestate') return { ok: true, feeState: { send: 1000 } }
      if (path === '/api/intents/preview') return { ok: true, preview: previewFor(6e9) }
      return { ok: false }
    })
    const { result } = await renderForm()
    act(() => result.current.amountChanged('sell', 'asset')(typed('0.75')))
    await act(async () => { vi.advanceTimersByTime(350) })
    await flush()
    expect(result.current.preview).not.toBeNull()
    expect(result.current.canExecute).toBe(false)
  })

  it('requires the password when sends require one', async () => {
    app.appSettings = { sendRequiresPassword: true }
    vi.mocked(postJSON).mockImplementation(async (path: string) => {
      if (path === '/api/intents/preview') return { ok: true, preview: previewFor(6e9) }
      return { ok: false }
    })
    const { result } = await renderForm()
    act(() => result.current.amountChanged('sell', 'asset')(typed('0.1')))
    await act(async () => { vi.advanceTimersByTime(350) })
    await flush()
    expect(result.current.canExecute).toBe(false)
    act(() => result.current.setPW('pw'))
    expect(result.current.canExecute).toBe(true)
  })
})
