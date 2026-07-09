import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { postJSON } from '@caribou/shared/http'
import UnlockForm from './UnlockForm'

vi.mock('@caribou/shared/http', () => ({ postJSON: vi.fn() }))

afterEach(() => {
  cleanup()
  vi.mocked(postJSON).mockReset()
})

describe('UnlockForm', () => {
  it('submits the password once even on a rapid double submit', async () => {
    let resolveFn: (v: any) => void
    vi.mocked(postJSON).mockReturnValue(new Promise(resolve => { resolveFn = resolve }))
    const setUnlocked = vi.fn()
    render(<UnlockForm setUnlocked={setUnlocked} />)

    fireEvent.change(screen.getByPlaceholderText('password'), { target: { value: 'hunter2' } })
    const form = screen.getByRole('button', { name: /unlock/i }).closest('form')!

    // Two submits fired back-to-back, before the in-flight request resolves.
    fireEvent.submit(form)
    fireEvent.submit(form)

    expect(postJSON).toHaveBeenCalledTimes(1)

    resolveFn!({ ok: true })
    await waitFor(() => expect(setUnlocked).toHaveBeenCalledOnce())
  })

  it('shows the server error and does not call setUnlocked on failure', async () => {
    vi.mocked(postJSON).mockResolvedValue({ ok: false, errMsg: 'bad password' })
    const setUnlocked = vi.fn()
    render(<UnlockForm setUnlocked={setUnlocked} />)

    fireEvent.change(screen.getByPlaceholderText('password'), { target: { value: 'wrong' } })
    fireEvent.submit(screen.getByRole('button', { name: /unlock/i }).closest('form')!)

    await screen.findByText('bad password')
    expect(setUnlocked).not.toHaveBeenCalled()
  })
})
