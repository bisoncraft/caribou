import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { postJSON } from '@bossw/shared/http'
import app from '@bossw/shared/application'
import { makeAsset, makeUser, makeWallet, resetApp } from '@bossw/shared/testUtils'
import Receive from './Receive'

vi.mock('@bossw/shared/http', () => ({ postJSON: vi.fn() }))

// USDC on two networks (Ethereum=asset 60, Polygon=asset 966), both active
// (settlement-network wallets exist), so Receive lands straight on the
// step-2 network picker for a preselected 'USDC' ticker.
const ethID = 60
const polyID = 966
const usdcEthID = 6001
const usdcPolyID = 9661

function setUpUsdcTicker () {
  app.user = makeUser([
    makeAsset(ethID, 'ETH', { name: 'Ethereum', wallet: makeWallet(ethID) }),
    makeAsset(polyID, 'POLYGON', { name: 'Polygon', wallet: makeWallet(polyID) }),
    makeAsset(usdcEthID, 'USDC', {
      wallet: makeWallet(usdcEthID),
      token: { networkID: ethID, name: 'Ethereum', unitInfo: makeAsset(ethID, 'ETH').unitInfo, contractAddress: '0x1', supportedAssetVersions: [0] }
    }),
    makeAsset(usdcPolyID, 'USDC', {
      wallet: makeWallet(usdcPolyID),
      token: { networkID: polyID, name: 'Polygon', unitInfo: makeAsset(polyID, 'POLYGON').unitInfo, contractAddress: '0x2', supportedAssetVersions: [0] }
    })
  ])
  app.prepareTickerAssets()
}

beforeEach(() => {
  resetApp()
  setUpUsdcTicker()
})

afterEach(() => {
  cleanup()
  vi.mocked(postJSON).mockReset()
})

describe('Receive stale-response race', () => {
  it('discards an older /api/depositaddress response that resolves after the user picked a different network', async () => {
    // Pick Ethereum (request A starts, left unresolved), then go Back and
    // pick Polygon (request B starts and resolves first). Request A must
    // not clobber Polygon's address when it finally resolves late.
    let resolveEth: (v: any) => void
    let resolvePoly: (v: any) => void
    vi.mocked(postJSON).mockImplementation((_addr, body) => {
      if (body.assetID === ethID) return new Promise(resolve => { resolveEth = resolve })
      if (body.assetID === polyID) return new Promise(resolve => { resolvePoly = resolve })
      throw new Error(`unexpected assetID ${body.assetID}`)
    })

    render(<Receive ticker="USDC" />)

    fireEvent.click(screen.getByText('Ethereum'))
    await screen.findByText('Loading...') // request A in flight, unresolved

    fireEvent.click(screen.getByRole('button', { name: 'Back' }))
    fireEvent.click(screen.getByText('Polygon'))

    resolvePoly!({ ok: true, address: 'poly-address' })
    await screen.findByText('poly-address')

    // Request A (Ethereum) finally resolves after the fact — must be a no-op.
    resolveEth!({ ok: true, address: 'eth-address' })
    await new Promise(resolve => setTimeout(resolve, 0))
    expect(screen.getByText('poly-address')).toBeInTheDocument()
    expect(screen.queryByText('eth-address')).not.toBeInTheDocument()
  })
})
