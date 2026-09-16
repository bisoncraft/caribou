import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import app from '@caribou/shared/application'
import { WalletTransaction } from '@caribou/shared/registry'
import { TxType } from '@caribou/shared/tx'
import { makeAsset, makeUser, makeWallet, resetApp } from '@caribou/shared/testUtils'
import { IntentsSwap } from '../intents'
import TxRow, { TxRowItem } from './TxRow'

const btcID = 0

function setUp (): TxRowItem {
  resetApp() // replaces app.setForm, so the spy is installed after it
  vi.spyOn(app, 'setForm').mockImplementation(() => {})
  app.user = makeUser([makeAsset(btcID, 'BTC', { name: 'Bitcoin', wallet: makeWallet(btcID) })])
  app.prepareTickerAssets()
  const ta = app.tickerMap.BTC
  const variant = ta.variants[0]
  const tx = {
    type: TxType.Send,
    id: 'txid',
    amount: 1000,
    fees: 10,
    assetID: btcID,
    networkID: btcID,
    blockNumber: 1,
    timestamp: 0,
    confirmed: true,
    additionalData: {}
  } as unknown as WalletTransaction
  const swap = { depositAddress: 'deposit-addr' } as IntentsSwap
  return { tx, ta, variant, swapRelation: { swap, role: 'deposit' } }
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('TxRow keyboard activation', () => {
  it('opens the transaction detail on Enter pressed on the row itself', () => {
    const item = setUp()
    render(<TxRow item={item} />)
    const row = screen.getByRole('button', { name: /^Sent BTC/ })

    // fireEvent returns false when a handler called preventDefault, which
    // the row does for its own activation keys.
    expect(fireEvent.keyDown(row, { key: 'Enter' })).toBe(false)
    expect(app.setForm).toHaveBeenCalledWith({ form: 'txdetail', data: { assetID: item.variant.assetID, tx: item.tx } })
  })

  it('leaves a key pressed on the swap-leg badge to the badge', () => {
    // The badge is a real button nested in the row, so its keydown bubbles
    // to the row. If the row prevented default and opened the transaction,
    // the badge would be keyboard-inoperable and open the wrong dialog. The
    // row must ignore keys whose target is not the row itself, leaving the
    // badge's own activation (the click a browser synthesizes on Enter or
    // Space) to open the swap.
    const item = setUp()
    render(<TxRow item={item} />)
    const badge = screen.getByRole('button', { name: /cross-chain swap/ })

    expect(fireEvent.keyDown(badge, { key: 'Enter' })).toBe(true)
    expect(fireEvent.keyDown(badge, { key: ' ' })).toBe(true)
    expect(app.setForm).not.toHaveBeenCalled()

    fireEvent.click(badge)
    expect(app.setForm).toHaveBeenCalledTimes(1)
    expect(app.setForm).toHaveBeenCalledWith({ form: 'swap', data: { depositAddress: 'deposit-addr' } })
  })
})
