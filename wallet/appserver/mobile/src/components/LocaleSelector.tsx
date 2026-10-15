import { useState } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'

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

const nameOf = (code: string) => autonyms[code] ?? code

// LocaleSelector lets the user pick the app language, using the languages the
// server reports. It renders nothing until the app state has loaded them.
export default function LocaleSelector () {
  const [lang, setLang] = useState<string>(app.lang)
  const langs = app.langs ?? []
  if (langs.length === 0) return null

  const changeLanguage = async (newLang: string) => {
    await app.changeLocale(newLang)
    app.reRender()
    setLang(newLang)
  }

  // The server builds the list from a map, so its order differs per launch.
  const sorted = [...langs].sort((a, b) => nameOf(a).localeCompare(nameOf(b)))

  return (
    <label className='mb-select-row'>
      <span>{t('Language')}</span>
      <select value={lang} onChange={e => changeLanguage(e.target.value)}>
        {sorted.map(l => <option key={l} value={l}>{nameOf(l)}</option>)}
      </select>
    </label>
  )
}
