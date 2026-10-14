import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { getJSON } from '@caribou/shared/http'
import app from '@caribou/shared/application'
import { describeTermsScreen } from '@caribou/shared/termsScreenSuite'
import TermsScreen from './TermsScreen'

vi.mock('@caribou/shared/http', () => ({ getJSON: vi.fn(), postJSON: vi.fn() }))

describeTermsScreen(TermsScreen)

// The terms screen comes before the wizard and settings, so on a fresh install
// it is the only place a mobile user can change language.
describe('TermsScreen language selector', () => {
  afterEach(() => {
    cleanup()
    app.lang = 'en-US'
    app.langs = []
  })

  const renderWith = (lang: string, langs: string[]) => {
    vi.mocked(getJSON).mockResolvedValue({ ok: true, version: 'v', terms: '', privacy: '' })
    app.lang = lang
    app.langs = langs
    render(<TermsScreen onAccepted={vi.fn()} />)
    return screen.getByRole('combobox', { name: 'Language' }) as HTMLSelectElement
  }

  // The server lists languages in map order, so the selector sorts them.
  it('offers the server languages in a stable order', () => {
    const select = renderWith('en-US', ['pt-BR', 'en-US', 'de-DE'])
    expect(Array.from(select.options).map(o => o.value)).toEqual(['de-DE', 'en-US', 'pt-BR'])
  })

  // A user who picked the wrong language must still be able to find theirs.
  it('names each language in its own language', () => {
    const select = renderWith('ar', ['en-US', 'ar', 'zh-CN'])
    const names = Array.from(select.options).map(o => o.textContent)
    expect(names).toContain('English')
    expect(names).toContain('العربية')
    expect(names).toContain('中文')
  })
})
