import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { postJSON } from '@caribou/shared/http'
import InitWizard from './InitWizard'

vi.mock('@caribou/shared/http', () => ({ postJSON: vi.fn() }))

afterEach(() => {
  cleanup()
  vi.mocked(postJSON).mockReset()
})

describe('InitWizard', () => {
  it('submits /api/init once even on a rapid double submit of the password form', async () => {
    let resolveFn: (v: any) => void
    vi.mocked(postJSON).mockReturnValue(new Promise(resolve => { resolveFn = resolve }))
    const setInited = vi.fn()
    render(<InitWizard setInited={setInited} />)

    // Skip the seed-restore prompt ("No" -> fresh wallet), landing on the
    // password form.
    fireEvent.click(screen.getByRole('button', { name: 'No' }))

    const [pw, pw2] = screen.getAllByPlaceholderText(/password/i)
    fireEvent.change(pw, { target: { value: 'hunter2hunter2' } })
    fireEvent.change(pw2, { target: { value: 'hunter2hunter2' } })

    const form = screen.getByRole('button', { name: /submit/i }).closest('form')!
    fireEvent.submit(form)
    fireEvent.submit(form)

    expect(postJSON).toHaveBeenCalledTimes(1)

    // A fresh (non-restore) wallet shows the seed backup screen before
    // finishing, so setInited() isn't called until the user acks it there.
    resolveFn!({ ok: true, mnemonic: 'abandon abandon abandon' })
    await screen.findByText(/written down my seed/i)
    expect(setInited).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: /written down my seed/i }))
    expect(setInited).toHaveBeenCalledOnce()
  })
})
