import { useState } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { Icon } from './ui'

// Each language is named in its own language, so a user who picked the wrong
// one can still find theirs. These mirror the desktop LocaleSelector.
const autonyms: Record<string, string> = {
  'en-US': 'English',
  'pt-BR': 'Português',
  'zh-CN': '中文',
  'pl-PL': 'Polski',
  'de-DE': 'Deutsch',
  'ar': 'العربية'
}

export const languageName = (code: string) => autonyms[code] ?? code

// useLocale returns the current language, the server's languages sorted by
// display name, and a setter that switches the app language.
export function useLocale (): [string, string[], (lang: string) => Promise<void>] {
  const [lang, setLang] = useState<string>(app.lang)
  // The server builds the list from a map, so its order differs per launch.
  const sorted = [...(app.langs ?? [])].sort((a, b) => languageName(a).localeCompare(languageName(b)))
  const change = async (newLang: string) => {
    await app.changeLocale(newLang)
    app.reRender()
    setLang(newLang)
  }
  return [lang, sorted, change]
}

// LocaleSelector is the language pill shown on first-run screens: the current
// language's name over an invisible native select, so the platform's own
// picker opens on tap. It renders nothing until the app state has loaded the
// language list.
export default function LocaleSelector () {
  const [lang, langs, change] = useLocale()
  if (langs.length === 0) return null
  return (
    <label className='lang-pill'>
      {languageName(lang)}
      <Icon name='chevron-left' className='lang-pill-chev' />
      <select value={lang} aria-label={t('Language')} onChange={e => change(e.target.value)}>
        {langs.map(l => <option key={l} value={l}>{languageName(l)}</option>)}
      </select>
    </label>
  )
}
