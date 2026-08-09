import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { postJSON } from '@caribou/shared/http'
import app from '@caribou/shared/application'
import { resetApp } from '@caribou/shared/testUtils'
import Settings from './Settings'

vi.mock('@caribou/shared/http', () => ({ postJSON: vi.fn() }))

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
  vi.mocked(postJSON).mockReset()
})

describe('Settings > SeedRow', () => {
  it('exports the seed once even on a rapid double submit', async () => {
    let resolveFn: (v: any) => void
    vi.mocked(postJSON).mockReturnValue(new Promise(resolve => { resolveFn = resolve }))
    render(<Settings />)

    // The password form is collapsed until the row button expands it. Once
    // open, the row button reads Cancel, so the reveal-seed name uniquely
    // matches the form's submit button.
    fireEvent.click(screen.getByRole('button', { name: /reveal seed/i }))
    fireEvent.change(screen.getByPlaceholderText('password'), { target: { value: 'hunter2' } })
    const form = screen.getByRole('button', { name: /reveal seed/i }).closest('form')!
    fireEvent.submit(form)
    fireEvent.submit(form)

    expect(postJSON).toHaveBeenCalledTimes(1)

    resolveFn!({ ok: true, seed: 'abandon abandon abandon' })
    await screen.findByText('abandon abandon abandon')
  })
})

describe('Settings > ThemeRow', () => {
  afterEach(() => {
    delete document.body.dataset.theme
    window.localStorage.removeItem('theme')
    document.querySelector('link[rel="icon"]')?.remove()
  })

  it('applies and persists a picked theme', () => {
    const fav = document.createElement('link')
    fav.rel = 'icon'
    fav.href = '/img/favicon-brown.png?v=abc123'
    document.head.appendChild(fav)

    render(<Settings />)
    const cool = screen.getByRole('button', { name: /cool/i })
    expect(cool.getAttribute('aria-pressed')).toBe('false')

    fireEvent.click(cool)
    expect(document.body.dataset.theme).toBe('cool')
    expect(JSON.parse(window.localStorage.getItem('theme')!)).toBe('cool')
    expect(cool.getAttribute('aria-pressed')).toBe('true')
    expect(fav.href).toContain('/img/favicon-blue.png?v=abc123')

    fireEvent.click(screen.getByRole('button', { name: /warm/i }))
    expect(fav.href).toContain('/img/favicon-brown.png?v=abc123')
  })

  it('falls back to warm for an unknown stored theme', () => {
    window.localStorage.setItem('theme', JSON.stringify('neon'))
    render(<Settings />)
    expect(screen.getByRole('button', { name: /warm/i }).getAttribute('aria-pressed')).toBe('true')
  })
})

describe('Settings > ToggleRow', () => {
  it('reverts the optimistic toggle when the save request fails', async () => {
    app.appSettings.sendRequiresPassword = false
    vi.mocked(postJSON).mockResolvedValue({ ok: false, errMsg: 'save failed' })
    render(<Settings />)

    const toggle = screen.getByLabelText(/require password to send/i) as HTMLInputElement
    expect(toggle.checked).toBe(false)

    fireEvent.click(toggle)
    await waitFor(() => expect(toggle.checked).toBe(false))
    expect(await screen.findByText('save failed')).toBeDefined()
    expect(app.appSettings.sendRequiresPassword).toBe(false)
  })
})

describe('Settings > PasswordRow', () => {
  it('rejects mismatched new passwords without calling the API', async () => {
    render(<Settings />)

    fireEvent.click(screen.getByRole('button', { name: 'Change' }))
    fireEvent.change(screen.getByPlaceholderText('current password'), { target: { value: 'hunter2' } })
    fireEvent.change(screen.getByPlaceholderText('new password'), { target: { value: 'hunter3' } })
    fireEvent.change(screen.getByPlaceholderText('new password again'), { target: { value: 'hunter4' } })
    fireEvent.submit(screen.getByRole('button', { name: /change password/i }).closest('form')!)

    expect(await screen.findByText('Passwords do not match.')).toBeDefined()
    expect(postJSON).not.toHaveBeenCalled()
  })

  it('collapses the form and reports success on a completed change', async () => {
    vi.mocked(postJSON).mockResolvedValue({ ok: true })
    render(<Settings />)

    fireEvent.click(screen.getByRole('button', { name: 'Change' }))
    fireEvent.change(screen.getByPlaceholderText('current password'), { target: { value: 'hunter2' } })
    fireEvent.change(screen.getByPlaceholderText('new password'), { target: { value: 'hunter3' } })
    fireEvent.change(screen.getByPlaceholderText('new password again'), { target: { value: 'hunter3' } })
    fireEvent.submit(screen.getByRole('button', { name: /change password/i }).closest('form')!)

    expect(await screen.findByText('Password changed.')).toBeDefined()
    expect(postJSON).toHaveBeenCalledWith('/api/changeapppass', { appPW: 'hunter2', newAppPW: 'hunter3' })
    expect(screen.queryByPlaceholderText('current password')).toBeNull()
  })
})
