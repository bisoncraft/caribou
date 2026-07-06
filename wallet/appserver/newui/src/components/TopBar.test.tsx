import { act } from 'react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import app from '@bossw/shared/application'
import { makeAsset, makeUser, makeWallet, resetApp } from '@bossw/shared/testUtils'
import TopBar from './TopBar'

const dcrID = 42

beforeEach(() => {
  resetApp()
  app.user = makeUser(
    [makeAsset(dcrID, 'DCR', { wallet: makeWallet(dcrID, { balance: { available: 10 * 1e8, immature: 0, locked: 0, stamp: '', other: {} } }) })],
    { [dcrID]: 20 } // $20/DCR fiat rate
  )
})

afterEach(() => {
  cleanup()
})

describe('TopBar', () => {
  it('re-renders the total when a balance updater fires, and unregisters both updaters on unmount', () => {
    const { unmount } = render(<TopBar />)
    expect(screen.getByText('$200.0 USD')).toBeInTheDocument()

    // Simulate a balance note landing: bump available balance and fire the
    // updater the same way Application.updateUser() does.
    app.user.assets[dcrID].wallet.balance.available = 20 * 1e8
    act(() => {
      Object.values(app.balanceUpdaters).forEach(u => u())
    })
    expect(screen.getByText('$400.0 USD')).toBeInTheDocument()

    expect(Object.keys(app.balanceUpdaters)).toHaveLength(1)
    expect(Object.keys(app.fiatRateUpdaters)).toHaveLength(1)
    unmount()
    expect(Object.keys(app.balanceUpdaters)).toHaveLength(0)
    expect(Object.keys(app.fiatRateUpdaters)).toHaveLength(0)
  })
})
