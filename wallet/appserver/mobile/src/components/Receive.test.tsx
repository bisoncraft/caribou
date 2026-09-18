import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { postJSON } from '@caribou/shared/http'
import app from '@caribou/shared/application'
import { ApprovalStatus, TrustlineState } from '@caribou/shared/registry'
import { makeAsset, makeUnitInfo, makeUser, makeWallet, makeWalletDefinition, resetApp } from '@caribou/shared/testUtils'
import Receive from './Receive'

vi.mock('@caribou/shared/http', () => ({ postJSON: vi.fn() }))

// USDC on XRP: the token wallet carries a trust-line state, so Receive gates
// the address on it. XRP=asset 144 with a 1 XRP minimum first deposit.
const xrpID = 144
const usdcXrpID = 144001
const xrpUI = makeUnitInfo('XRP', 1e6)

function setUpXrp (trustline: TrustlineState, xrpBalance = 5e6) {
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
      name: 'USD Coin',
      unitInfo: makeUnitInfo('USDC', 1e6),
      wallet: makeWallet(usdcXrpID, { trustline }),
      token: { networkID: xrpID, name: 'XRP', unitInfo: xrpUI, contractAddress: '', definition: makeWalletDefinition(), supportedAssetVersions: [0] }
    })
  ])
  app.prepareTickerAssets()
}

const noLine = (shortfall: number): TrustlineState => ({ status: ApprovalStatus.NotApproved, reserve: 2e5, shortfall })

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
  vi.mocked(postJSON).mockReset()
})

// Tapping USDC in the asset list lands on the network step's only variant.
function openUsdc () {
  render(<Receive onClose={() => {}} />)
  fireEvent.click(screen.getByText('USDC'))
}

describe('mobile Receive trust-line gate', () => {
  it('withholds the address and offers establishment when the line is missing and affordable', async () => {
    setUpXrp(noLine(0))
    vi.mocked(postJSON).mockResolvedValue({ ok: true, txID: 'abc' })
    openUsdc()

    const btn = await screen.findByRole('button', { name: 'Establish trust line' })
    expect(screen.queryByText('Loading...')).toBeNull()
    expect(screen.queryByText('Copy Address')).toBeNull()
    expect(screen.getByText(/keeps 0.20 XRP of your balance reserved/)).toBeDefined()

    fireEvent.click(btn)
    await vi.waitFor(() => expect(postJSON).toHaveBeenCalledWith('/api/establishtrustline', { assetID: usdcXrpID, pass: '' }))
    expect(vi.mocked(postJSON).mock.calls.some(c => c[0] === '/api/depositaddress')).toBe(false)
  })

  it('states the shortfall and re-targets the overlay at XRP on request', async () => {
    setUpXrp(noLine(1200010), 0)
    vi.mocked(postJSON).mockResolvedValue({ ok: true, address: 'rXRPADDRESS' })
    openUsdc()

    await screen.findByText(/needs at least 1.20001 more XRP/)
    expect(screen.queryByRole('button', { name: 'Establish trust line' })).toBeNull()
    expect(postJSON).not.toHaveBeenCalled()

    // The shortcut switches the same overlay to the XRP address, with the
    // minimum first deposit stated for the still-unfunded account.
    fireEvent.click(screen.getByRole('button', { name: 'Receive XRP' }))
    await screen.findByText('rXRPADDRESS')
    expect(postJSON).toHaveBeenCalledWith('/api/depositaddress', { assetID: xrpID })
    expect(screen.getByText(/first deposit must be at least 1 XRP/)).toBeDefined()
    expect(screen.queryByText(/needs at least/)).toBeNull()
  })

  it('shows progress while the establishing transaction is pending', async () => {
    setUpXrp({ status: ApprovalStatus.Pending, reserve: 2e5, shortfall: 0 })
    openUsdc()

    await screen.findByText('Establishing trust line...')
    expect(screen.queryByRole('button', { name: 'Establish trust line' })).toBeNull()
    expect(postJSON).not.toHaveBeenCalled()
  })

  it('requests the token wallet address once the line exists', async () => {
    setUpXrp({ status: ApprovalStatus.Approved, reserve: 2e5, shortfall: 0 })
    vi.mocked(postJSON).mockResolvedValue({ ok: true, address: 'rXRPADDRESS' })
    openUsdc()

    await screen.findByText('rXRPADDRESS')
    expect(postJSON).toHaveBeenCalledWith('/api/depositaddress', { assetID: usdcXrpID })
    expect(screen.queryByText('Establish trust line')).toBeNull()
    // A funded account carries no minimum-deposit note.
    expect(screen.queryByText(/first deposit must be at least/)).toBeNull()
  })
})
