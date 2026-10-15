// termsScreenSuite is the behavior suite for the first-run Terms screen. The
// desktop (newui) and mobile bundles each render their own TermsScreen, but
// both must behave the same, so each bundle's test file runs this one suite
// against its component. The calling test file must
// vi.mock('@caribou/shared/http') with getJSON and postJSON stubs. Like
// testUtils, it is deliberately not re-exported from index.ts so it never
// ends up in a production bundle.
import type { ComponentType } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { getJSON, postJSON } from './http'

const doc = {
  ok: true,
  version: '2026-09-21',
  terms: '# Terms of Use\n\nYou hold your own keys.',
  privacy: '# Privacy Policy\n\nNo telemetry.'
}

const acceptButton = () => screen.getByRole('button', { name: /accept and continue/i })

export function describeTermsScreen (TermsScreen: ComponentType<{ onAccepted: () => void }>) {
  describe('TermsScreen', () => {
    beforeEach(() => {
      vi.mocked(getJSON).mockResolvedValue(doc)
    })

    afterEach(() => {
      cleanup()
      vi.mocked(getJSON).mockReset()
      vi.mocked(postJSON).mockReset()
    })

    it('shows both documents and keeps Accept disabled until the box is ticked', async () => {
      render(<TermsScreen onAccepted={vi.fn()} />)
      await screen.findByText('You hold your own keys.')
      expect(screen.getByText('No telemetry.')).toBeTruthy()

      expect((acceptButton() as HTMLButtonElement).disabled).toBe(true)
      fireEvent.click(screen.getByRole('checkbox'))
      expect((acceptButton() as HTMLButtonElement).disabled).toBe(false)
    })

    it('records the version the user was shown and then continues', async () => {
      vi.mocked(postJSON).mockResolvedValue({ ok: true })
      const onAccepted = vi.fn()
      render(<TermsScreen onAccepted={onAccepted} />)
      await screen.findByText('You hold your own keys.')

      fireEvent.click(screen.getByRole('checkbox'))
      fireEvent.click(acceptButton())

      await waitFor(() => expect(onAccepted).toHaveBeenCalledOnce())
      expect(postJSON).toHaveBeenCalledWith('/api/acceptterms', { version: '2026-09-21' })
    })

    it('stays on the screen and shows the error when saving fails', async () => {
      vi.mocked(postJSON).mockResolvedValue({ ok: false, msg: 'disk full' })
      const onAccepted = vi.fn()
      render(<TermsScreen onAccepted={onAccepted} />)
      await screen.findByText('You hold your own keys.')

      fireEvent.click(screen.getByRole('checkbox'))
      fireEvent.click(acceptButton())

      await screen.findByText('disk full')
      expect(onAccepted).not.toHaveBeenCalled()
    })

    it('never records acceptance when the user declines', async () => {
      const onAccepted = vi.fn()
      render(<TermsScreen onAccepted={onAccepted} />)
      await screen.findByText('You hold your own keys.')

      fireEvent.click(screen.getByRole('button', { name: /decline/i }))

      // No locale is loaded in tests, so t() returns the message key.
      await screen.findByText('terms_declined')
      expect(postJSON).not.toHaveBeenCalled()
      expect(onAccepted).not.toHaveBeenCalled()
    })

    it('cannot be accepted when the documents fail to load', async () => {
      vi.mocked(getJSON).mockResolvedValue({ ok: false, errMsg: 'HTTP 500' })
      render(<TermsScreen onAccepted={vi.fn()} />)

      await screen.findByText('HTTP 500')
      expect((screen.getByRole('checkbox') as HTMLInputElement).disabled).toBe(true)
      expect((acceptButton() as HTMLButtonElement).disabled).toBe(true)
    })

    it('recovers when Retry succeeds after a failed load', async () => {
      vi.mocked(getJSON).mockResolvedValueOnce({ ok: false, errMsg: 'HTTP 500' })
      render(<TermsScreen onAccepted={vi.fn()} />)
      await screen.findByText('HTTP 500')

      fireEvent.click(screen.getByRole('button', { name: /retry/i }))

      await screen.findByText('You hold your own keys.')
      expect(screen.queryByText('HTTP 500')).toBeNull()
      expect((screen.getByRole('checkbox') as HTMLInputElement).disabled).toBe(false)
      expect(screen.queryByRole('button', { name: /retry/i })).toBeNull()
    })

    it('refuses to submit when the box is not ticked', async () => {
      render(<TermsScreen onAccepted={vi.fn()} />)
      await screen.findByText('You hold your own keys.')

      // Bypass the disabled button, as a keyboard or scripted submit could.
      fireEvent.submit(acceptButton().closest('form')!)

      expect(postJSON).not.toHaveBeenCalled()
    })

    it('names the scrollable terms region for assistive tech', async () => {
      render(<TermsScreen onAccepted={vi.fn()} />)
      await screen.findByText('You hold your own keys.')

      expect(screen.getByRole('region', { name: 'terms_intro' })).toBeTruthy()
    })
  })
}
