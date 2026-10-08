import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, waitFor } from '@testing-library/react'
import app from '@caribou/shared/application'
import { makeAsset, makeUser, makeWallet, resetApp } from '@caribou/shared/testUtils'
import Asset from './Asset'

const btcID = 0

function mockFetch () {
  return vi.spyOn(window, 'fetch').mockImplementation(
    async () => new Response(JSON.stringify({ ok: true }), { status: 200 }))
}

beforeEach(() => {
  resetApp()
  app.user = makeUser([makeAsset(btcID, 'BTC', { name: 'Bitcoin', wallet: makeWallet(btcID) })])
  app.prepareTickerAssets()
})

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('Asset view swap catalog', () => {
  it('does not request the swap catalog just by being opened', async () => {
    // The catalog request goes to a third party, so it must wait until the
    // user starts a swap; fetching it here would expose their IP on every
    // asset view.
    const fetchSpy = mockFetch()

    render(<Asset ticker="BTC" />)

    // The swap-records request is made on mount, so once it has gone out
    // the mount effects have run and any catalog request would be visible.
    await waitFor(() => {
      expect(fetchSpy.mock.calls.map(c => String(c[0]))).toContain('/api/intents/swaps')
    })
    expect(fetchSpy.mock.calls.map(c => String(c[0]))).not.toContain('/api/intents/assets')
  })
})
