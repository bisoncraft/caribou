import { useState, FormEvent } from 'react'
import t from '@caribou/shared/intl'
import { postJSON } from '@caribou/shared/http'
import { PasswordField } from './ui'

interface UnlockFormProps {
  onUnlocked: () => void
}

// UnlockForm is the mobile lock screen. It posts the app password to the same
// /api/login endpoint the desktop uses; the shared transport handles the rest.
export default function UnlockForm ({ onUnlocked }: UnlockFormProps) {
  const [pw, setPW] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault()
    if (submitting || pw === '') return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/login', { pass: pw })
    setSubmitting(false)
    if (r.ok) {
      onUnlocked()
      return
    }
    setErr(r.errMsg || r.msg || t('Incorrect password.'))
  }

  return (
    <form className='frame unlock' onSubmit={onSubmit}>
      <div className='unlock-hero'>
        <span className='mark md' role='img' aria-label='Caribou' />
        <h1>{t('Welcome back')}</h1>
      </div>
      <div className='foot'>
        <PasswordField
          value={pw}
          onChange={v => { setPW(v); if (err) setErr('') }}
          placeholder={t('Password')}
          disabled={submitting}
          bad={err !== ''}
          autoFocus
        />
        <div className='field-err' aria-live='polite'>{err}</div>
        <button className='btn mt12' type='submit' disabled={pw === '' || submitting}>
          {submitting ? t('Unlocking...') : t('Unlock')}
        </button>
      </div>
    </form>
  )
}
