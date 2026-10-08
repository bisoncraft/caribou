import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import app from '@caribou/shared/application'
import { makeAsset, makeUser, makeWallet, resetApp } from '@caribou/shared/testUtils'
import { IntentsAsset } from '../intents'
import Swap from './Swap'

const btcID = 0
const ltcID = 2

function catalogEntry (assetID: number, symbol: string): IntentsAsset {
  return { assetID, symbol, intentsID: symbol, decimals: 8, atomDecimals: 8, priceUSD: 1, walletReady: true }
}

function mockCatalog (assets: IntentsAsset[]) {
  vi.spyOn(window, 'fetch').mockImplementation(
    async () => new Response(JSON.stringify({ ok: true, assets }), { status: 200 }))
}

beforeEach(() => {
  resetApp()
  app.user = makeUser([
    makeAsset(btcID, 'BTC', { name: 'Bitcoin', wallet: makeWallet(btcID) }),
    makeAsset(ltcID, 'LTC', { name: 'Litecoin', wallet: makeWallet(ltcID) })
  ])
  app.prepareTickerAssets()
})

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('Swap opened from an asset', () => {
  // The asset view no longer checks the catalog before enabling its swap
  // button, so the dialog is where a user learns the asset cannot be
  // swapped. Without the hint the asset is just silently absent.
  it('says so when the catalog has no route for the asset', async () => {
    mockCatalog([catalogEntry(ltcID, 'LTC')])

    render(<Swap ticker="BTC" assetIDs={[btcID]} />)

    expect(await screen.findByText('No swap route is available for BTC.')).toBeInTheDocument()
  })

  it('stays quiet when the catalog has a route for the asset', async () => {
    mockCatalog([catalogEntry(btcID, 'BTC'), catalogEntry(ltcID, 'LTC')])

    render(<Swap ticker="BTC" assetIDs={[btcID]} />)

    await screen.findByRole('button', { name: 'Slippage settings' })
    expect(screen.queryByText(/No swap route/)).not.toBeInTheDocument()
  })

  it('stays quiet when not opened from an asset', async () => {
    mockCatalog([catalogEntry(ltcID, 'LTC')])

    render(<Swap />)

    await screen.findByRole('button', { name: 'Slippage settings' })
    expect(screen.queryByText(/No swap route/)).not.toBeInTheDocument()
  })
})
