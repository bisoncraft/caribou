import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { cleanup, render } from '@testing-library/react'
import app from '@bossw/shared/application'
import { resetApp } from '@bossw/shared/testUtils'
import Portfolio from './Portfolio'

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
})

describe('Portfolio effect cleanup', () => {
  it('unregisters its balance/fiat-rate/note-feed subscriptions on unmount', () => {
    const { unmount } = render(<Portfolio />)

    expect(Object.keys(app.balanceUpdaters)).toHaveLength(1)
    expect(Object.keys(app.fiatRateUpdaters)).toHaveLength(1)
    expect(Object.keys(app.noteReceivers)).toHaveLength(1)

    unmount()

    expect(Object.keys(app.balanceUpdaters)).toHaveLength(0)
    expect(Object.keys(app.fiatRateUpdaters)).toHaveLength(0)
    expect(Object.keys(app.noteReceivers)).toHaveLength(0)
  })
})
