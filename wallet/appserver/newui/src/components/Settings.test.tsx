import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { postJSON } from '@bossw/shared/http'
import app from '@bossw/shared/application'
import { resetApp } from '@bossw/shared/testUtils'
import Settings from './Settings'

vi.mock('@bossw/shared/http', () => ({ postJSON: vi.fn() }))

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
  vi.mocked(postJSON).mockReset()
})

describe('Settings > SeedExport', () => {
  it('exports the seed once even on a rapid double submit', async () => {
    let resolveFn: (v: any) => void
    vi.mocked(postJSON).mockReturnValue(new Promise(resolve => { resolveFn = resolve }))
    render(<Settings />)

    fireEvent.change(screen.getByPlaceholderText('password'), { target: { value: 'hunter2' } })
    const form = screen.getByRole('button', { name: /reveal seed/i }).closest('form')!
    fireEvent.submit(form)
    fireEvent.submit(form)

    expect(postJSON).toHaveBeenCalledTimes(1)

    resolveFn!({ ok: true, seed: 'abandon abandon abandon' })
    await screen.findByText('abandon abandon abandon')
  })
})

describe('Settings > SendSettings', () => {
  it('reverts the optimistic toggle when the save request fails', async () => {
    app.appSettings.sendRequiresPassword = false
    vi.mocked(postJSON).mockResolvedValue({ ok: false, errMsg: 'save failed' })
    render(<Settings />)

    const checkbox = screen.getByLabelText(/require password to send/i) as HTMLInputElement
    expect(checkbox.checked).toBe(false)

    fireEvent.click(checkbox)
    await waitFor(() => expect(checkbox.checked).toBe(false))
    expect(await screen.findByText('save failed')).toBeDefined()
    expect(app.appSettings.sendRequiresPassword).toBe(false)
  })
})
