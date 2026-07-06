import { beforeEach, describe, expect, it, vi } from 'vitest'
import app from './application'
import { BalanceNote, CoreNote, TipChangeNote, WalletConfigNote, WalletNote } from './registry'
import { makeAsset, makeUser, makeWallet, resetApp } from './testUtils'

function baseNote (type: string): CoreNote {
  return { type, topic: '', subject: '', details: '', severity: 0, stamp: 0, acked: false, id: '1', data: {} }
}

describe('Application.notify / updateUser', () => {
  beforeEach(() => {
    resetApp()
    app.user = makeUser([makeAsset(42, 'DCR', { wallet: makeWallet(42) })])
  })

  it('dispatches notes to registered noteReceivers', () => {
    const received: CoreNote[] = []
    app.registerNoteFeed(n => received.push(n))
    const note = baseNote('fiatrateupdate')
    app.notify(note)
    expect(received).toEqual([note])
  })

  it('updates a known asset balance and fires balance updaters', () => {
    const updater = vi.fn()
    app.registerBalanceUpdater(updater)
    const note: BalanceNote = { ...baseNote('balance'), assetID: 42, balance: { available: 5, immature: 0, locked: 0, stamp: '', other: {} } }
    app.notify(note)
    expect(app.user.assets[42].wallet.balance.available).toBe(5)
    expect(updater).toHaveBeenCalledOnce()
  })

  // These three call updateUser() directly rather than through notify(),
  // because notify() wraps updateUser in its own try/catch — going through
  // notify() would make the assertion pass even if updateUser's per-branch
  // guard were removed, silently defeating the test.
  it('ignores a balance note for an asset not in the cache instead of throwing', () => {
    const updater = vi.fn()
    app.registerBalanceUpdater(updater)
    const note: BalanceNote = { ...baseNote('balance'), assetID: 999, balance: { available: 5, immature: 0, locked: 0, stamp: '', other: {} } }
    expect(() => app.updateUser(note)).not.toThrow()
    // The known asset (42) must be untouched, and the updater must not have
    // fired for an update that never happened.
    expect(app.user.assets[42].wallet.balance.available).toBe(0)
    expect(updater).not.toHaveBeenCalled()
  })

  it('ignores a walletstate note for an asset not in the cache instead of throwing', () => {
    const note: WalletConfigNote = { ...baseNote('walletstate'), wallet: makeWallet(999) }
    expect(() => app.updateUser(note)).not.toThrow()
    expect(app.user.assets[999]).toBeUndefined()
  })

  it('applies a walletstate note for a known asset', () => {
    const newWallet = makeWallet(42, { synced: true, peerCount: 3 })
    const note: WalletConfigNote = { ...baseNote('walletconfig'), wallet: newWallet }
    app.notify(note)
    expect(app.user.assets[42].wallet.peerCount).toBe(3)
  })

  it('ignores a tipChange note for an asset not in the cache instead of throwing', () => {
    const payload: TipChangeNote = { route: 'tipChange', assetID: 999, tip: 100, data: {} }
    const note: WalletNote = { ...baseNote('walletnote'), payload }
    expect(() => app.updateUser(note)).not.toThrow()
  })

  it('applies a tipChange note for a known asset', () => {
    const payload: TipChangeNote = { route: 'tipChange', assetID: 42, tip: 12345, data: {} }
    const note: WalletNote = { ...baseNote('walletnote'), payload }
    app.notify(note)
    expect(app.user.assets[42].wallet.syncStatus.blocks).toBe(12345)
  })

  it('wallet() returns undefined instead of throwing for an unknown assetID', () => {
    expect(app.wallet(999)).toBeUndefined()
  })

  it('keeps dispatching to noteReceivers even when updateUser throws on a note', () => {
    const received: CoreNote[] = []
    app.registerNoteFeed(n => received.push(n))
    const spy = vi.spyOn(app, 'updateUser').mockImplementation(() => { throw new Error('boom') })
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => { })

    const note = baseNote('fiatrateupdate')
    expect(() => app.notify(note)).not.toThrow()
    expect(received).toEqual([note])
    expect(consoleErr).toHaveBeenCalled()

    spy.mockRestore()
    consoleErr.mockRestore()
  })

  it('unregisterBalanceUpdater stops further updates from firing', () => {
    const updater = vi.fn()
    const key = app.registerBalanceUpdater(updater)
    app.unregisterBalanceUpdater(key)
    const note: BalanceNote = { ...baseNote('balance'), assetID: 42, balance: { available: 1, immature: 0, locked: 0, stamp: '', other: {} } }
    app.notify(note)
    expect(updater).not.toHaveBeenCalled()
  })
})
