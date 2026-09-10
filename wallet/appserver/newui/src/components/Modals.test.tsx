import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import app from '@caribou/shared/application'
import { postJSON } from '@caribou/shared/http'
import { ApprovalStatus } from '@caribou/shared/registry'
import { makeAsset, makeUnitInfo, makeUser, makeWallet, makeWalletDefinition, resetApp } from '@caribou/shared/testUtils'
import Modals from './Modals'
import { useModalBusy } from './modalBusy'

vi.mock('@caribou/shared/http', () => ({ postJSON: vi.fn() }))

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
  vi.mocked(postJSON).mockReset()
})

// XRP (asset 144) with an unfunded account, and USDC on XRP (asset 144001)
// whose wallet reports a missing, unaffordable trust line.
const xrpID = 144
const usdcXrpID = 144001

function setUpXrpAssets () {
  const xrpUI = makeUnitInfo('XRP', 1e6)
  app.user = makeUser([
    makeAsset(xrpID, 'XRP', {
      name: 'XRP',
      unitInfo: xrpUI,
      wallet: makeWallet(xrpID, { minDeposit: 1e6 })
    }),
    makeAsset(usdcXrpID, 'USDC', {
      unitInfo: makeUnitInfo('USDC', 1e6),
      wallet: makeWallet(usdcXrpID, { trustline: { status: ApprovalStatus.NotApproved, reserve: 2e5, shortfall: 1200012 } }),
      token: { networkID: xrpID, name: 'XRP', unitInfo: xrpUI, contractAddress: '', definition: makeWalletDefinition(), supportedAssetVersions: [0] }
    })
  ])
  app.prepareTickerAssets()
}

describe('Modals', () => {
  it('closes the open dialog on Escape', () => {
    const setForm = vi.spyOn(app, 'setForm')
    render(<Modals fd={{ form: 'receive', data: {} }} />)

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(setForm).toHaveBeenCalledWith({ form: '', data: {} })
    setForm.mockRestore()
  })

  it('ignores Escape while a dialog has an operation in flight', () => {
    // Stands in for a dialog that declares an in-flight operation, e.g. an
    // activation dialog mid-request.
    function Busy (): null {
      useModalBusy(true)
      return null
    }
    const setForm = vi.spyOn(app, 'setForm')
    const { unmount } = render(
      <>
        <Busy />
        <Modals fd={{ form: 'receive', data: {} }} />
      </>
    )

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(setForm).not.toHaveBeenCalled()

    // The guard lifts when the busy dialog unmounts.
    unmount()
    render(<Modals fd={{ form: 'receive', data: {} }} />)
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(setForm).toHaveBeenCalledWith({ form: '', data: {} })
    setForm.mockRestore()
  })

  it('re-targets an open Receive when the trust-line gate asks for the network asset', async () => {
    // Receive derives its selection from its props on mount only, so a
    // setForm to the same form with a new ticker must remount it rather than
    // update it in place; otherwise the "Receive XRP" button does nothing.
    setUpXrpAssets()
    vi.mocked(postJSON).mockResolvedValue({ ok: true, address: 'rXRPADDRESS' })
    const setForm = vi.spyOn(app, 'setForm')
    const { rerender } = render(<Modals fd={{ form: 'receive', data: { ticker: 'USDC', assetID: usdcXrpID } }} />)

    fireEvent.click(await screen.findByRole('button', { name: 'Receive XRP' }))
    expect(setForm).toHaveBeenCalledWith({ form: 'receive', data: { ticker: 'XRP' } })

    // The app applies the form change by re-rendering Modals with the new fd.
    rerender(<Modals fd={{ form: 'receive', data: { ticker: 'XRP' } }} />)
    await screen.findByText('rXRPADDRESS')
    expect(postJSON).toHaveBeenCalledWith('/api/depositaddress', { assetID: xrpID })
    expect(screen.queryByRole('button', { name: 'Receive XRP' })).toBeNull()
    setForm.mockRestore()
  })

  it('does not listen for Escape while no dialog is open', () => {
    const setForm = vi.spyOn(app, 'setForm')
    render(<Modals fd={{ form: '', data: {} }} />)

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(setForm).not.toHaveBeenCalled()
    setForm.mockRestore()
  })
})
