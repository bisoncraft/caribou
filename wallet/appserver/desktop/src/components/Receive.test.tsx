import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { postJSON } from '@caribou/shared/http'
import app from '@caribou/shared/application'
import { ApprovalStatus, TrustlineState } from '@caribou/shared/registry'
import { makeAsset, makeUnitInfo, makeUser, makeWallet, makeWalletDefinition, resetApp } from '@caribou/shared/testUtils'
import Receive from './Receive'

vi.mock('@caribou/shared/http', () => ({ postJSON: vi.fn() }))

// USDC on two networks (Ethereum=asset 60, Polygon=asset 966), both active
// (token wallets exist), so Receive lands straight on the step-2 network
// picker for a preselected 'USDC' ticker.
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
      token: { networkID: ethID, name: 'Ethereum', unitInfo: makeAsset(ethID, 'ETH').unitInfo, contractAddress: '0x1', definition: makeWalletDefinition() }
    }),
    makeAsset(usdcPolyID, 'USDC', {
      wallet: makeWallet(usdcPolyID),
      token: { networkID: polyID, name: 'Polygon', unitInfo: makeAsset(polyID, 'POLYGON').unitInfo, contractAddress: '0x2', definition: makeWalletDefinition() }
    })
  ])
  app.prepareTickerAssets()
}

// USDC on XRP: the token wallet carries a trust-line state, so Receive gates
// the address on it. XRP=asset 144 with a 1 XRP minimum first deposit.
const xrpID = 144
const usdcXrpID = 144001
const xrpUI = makeUnitInfo('XRP', 1e6)

function setUpXrpTicker (trustline: TrustlineState, xrpBalance = 5e6) {
  app.user = makeUser([
    makeAsset(xrpID, 'XRP', {
      name: 'XRP',
      unitInfo: xrpUI,
      wallet: makeWallet(xrpID, {
        minDeposit: 1e6,
        balance: { available: xrpBalance, immature: 0, locked: 0, stamp: '', other: {} }
      })
    }),
    makeAsset(usdcXrpID, 'USDC', {
      unitInfo: makeUnitInfo('USDC', 1e6),
      wallet: makeWallet(usdcXrpID, { trustline }),
      token: { networkID: xrpID, name: 'XRP', unitInfo: xrpUI, contractAddress: '', definition: makeWalletDefinition() }
    })
  ])
  app.prepareTickerAssets()
}

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
  vi.mocked(postJSON).mockReset()
})

describe('Receive stale-response race', () => {
  it('discards an older /api/depositaddress response that resolves after the user picked a different network', async () => {
    setUpUsdcTicker()
    // Pick Ethereum (request A starts, left unresolved), then return to the
    // network picker via the change-network control and pick Polygon
    // (request B starts and resolves first). Request A must not clobber
    // Polygon's address when it finally resolves late. Requests are keyed on
    // the token wallet, not the network wallet.
    let resolveEth: (v: any) => void
    let resolvePoly: (v: any) => void
    vi.mocked(postJSON).mockImplementation((_addr, body) => {
      if (body.assetID === usdcEthID) return new Promise(resolve => { resolveEth = resolve })
      if (body.assetID === usdcPolyID) return new Promise(resolve => { resolvePoly = resolve })
      throw new Error(`unexpected assetID ${body.assetID}`)
    })

    render(<Receive ticker="USDC" />)

    fireEvent.click(screen.getByText('Ethereum'))
    await screen.findByText('Loading...') // request A in flight, unresolved

    fireEvent.click(screen.getByRole('button', { name: 'Change network' }))
    fireEvent.click(screen.getByText('Polygon'))

    resolvePoly!({ ok: true, address: 'poly-address' })
    await screen.findByText('poly-address')

    // Request A (Ethereum) finally resolves after the fact — must be a no-op.
    resolveEth!({ ok: true, address: 'eth-address' })
    await new Promise(resolve => setTimeout(resolve, 0))
    expect(screen.getByText('poly-address')).toBeDefined()
    expect(screen.queryByText('eth-address')).toBeNull()
  })
})

describe('Receive trust-line gate', () => {
  it('withholds the address and offers establishment when the line is missing and affordable', async () => {
    setUpXrpTicker({ status: ApprovalStatus.NotApproved, reserve: 2e5, shortfall: 0 })
    vi.mocked(postJSON).mockResolvedValue({ ok: true, txID: 'abc' })

    render(<Receive ticker="USDC" assetID={usdcXrpID} />)

    const btn = await screen.findByRole('button', { name: 'Establish trust line' })
    expect(screen.queryByText('Loading...')).toBeNull()
    expect(screen.queryByText('Show QR code')).toBeNull()
    // The cost is stated from the wallet's live reserve figure.
    expect(screen.getByText(/keeps 0.20 XRP of your balance reserved/)).toBeDefined()

    fireEvent.click(btn)
    await vi.waitFor(() => expect(postJSON).toHaveBeenCalledWith('/api/establishtrustline', { assetID: usdcXrpID, pass: '' }))
    // No deposit-address request was made for a gated token.
    expect(vi.mocked(postJSON).mock.calls.some(c => c[0] === '/api/depositaddress')).toBe(false)
  })

  it('states the shortfall and points at the network asset when the account cannot afford the line', async () => {
    setUpXrpTicker({ status: ApprovalStatus.NotApproved, reserve: 2e5, shortfall: 1200010 })

    render(<Receive ticker="USDC" assetID={usdcXrpID} />)

    await screen.findByText(/needs at least 1.20001 more XRP/)
    expect(screen.queryByRole('button', { name: 'Establish trust line' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Receive XRP' })).toBeDefined()
    expect(postJSON).not.toHaveBeenCalled()
  })

  it('shows progress while the establishing transaction is pending', async () => {
    setUpXrpTicker({ status: ApprovalStatus.Pending, reserve: 2e5, shortfall: 0 })

    render(<Receive ticker="USDC" assetID={usdcXrpID} />)

    await screen.findByText('Establishing trust line...')
    expect(screen.queryByRole('button', { name: 'Establish trust line' })).toBeNull()
    expect(postJSON).not.toHaveBeenCalled()
  })

  it('requests the token wallet address once the line exists', async () => {
    setUpXrpTicker({ status: ApprovalStatus.Approved, reserve: 2e5, shortfall: 0 })
    vi.mocked(postJSON).mockResolvedValue({ ok: true, address: 'rXRPADDRESS' })

    render(<Receive ticker="USDC" assetID={usdcXrpID} />)

    await screen.findByText('rXRPADDRESS')
    expect(postJSON).toHaveBeenCalledWith('/api/depositaddress', { assetID: usdcXrpID })
    expect(screen.queryByText('Establish trust line')).toBeNull()
  })
})

describe('Receive minimum first deposit', () => {
  it('notes the base reserve for a native account not yet on the ledger', async () => {
    setUpXrpTicker({ status: ApprovalStatus.Approved, reserve: 2e5, shortfall: 0 }, 0)
    vi.mocked(postJSON).mockResolvedValue({ ok: true, address: 'rXRPADDRESS' })

    render(<Receive ticker="XRP" />)

    await screen.findByText('rXRPADDRESS')
    expect(screen.getByText(/first deposit must be at least 1 XRP/)).toBeDefined()
  })

  it('omits the note once the account holds a balance', async () => {
    setUpXrpTicker({ status: ApprovalStatus.Approved, reserve: 2e5, shortfall: 0 }, 5e6)
    vi.mocked(postJSON).mockResolvedValue({ ok: true, address: 'rXRPADDRESS' })

    render(<Receive ticker="XRP" />)

    await screen.findByText('rXRPADDRESS')
    expect(screen.queryByText(/first deposit must be at least/)).toBeNull()
  })
})
