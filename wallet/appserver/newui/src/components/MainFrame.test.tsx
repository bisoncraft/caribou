import { act } from 'react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import app from '@bossw/shared/application'
import { resetApp } from '@bossw/shared/testUtils'
import MainFrame from './MainFrame'

const pd = { pageParts: ['portfolio'], pageRoot: 'portfolio', data: {}, query: new URLSearchParams() }

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
})

describe('MainFrame form-setter registration', () => {
  it('routes app.setForm() calls to the Modals overlay while mounted, and unregisters on unmount', () => {
    const { unmount } = render(<MainFrame pd={pd} />)

    act(() => {
      app.setForm({ form: 'unknown-form', data: {} })
    })
    expect(screen.getByText('Unknown form: unknown-form')).toBeInTheDocument()

    unmount()

    // After unmount, app.setForm falls back to a no-op — must not throw or
    // resurrect a torn-down component's state setter.
    expect(() => app.setForm({ form: 'anything', data: {} })).not.toThrow()
  })
})
