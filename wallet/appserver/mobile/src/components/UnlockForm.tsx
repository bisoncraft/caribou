import { useState, ChangeEvent, FormEvent } from 'react'
import t from '@caribou/shared/intl'
import { postJSON } from '@caribou/shared/http'

interface UnlockFormProps {
  onUnlocked: () => void
}

// UnlockForm is the mobile lock screen. It posts the app password to the same
// /api/login endpoint the desktop uses; the shared transport handles the rest.
export default function UnlockForm ({ onUnlocked }: UnlockFormProps) {
  const [pw, setPW] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const onChange = (e: ChangeEvent<HTMLInputElement>) => {
    setPW(e.target.value)
    if (err) setErr('') // user is correcting input
  }

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
    <form className='mb-screen mb-unlock' onSubmit={onSubmit}>
      <h1 className='mb-brand'>Caribou</h1>
      <p className='mb-muted'>{t('Welcome Back!')}</p>
      <input
        className='mb-input'
        type='password'
        autoComplete='current-password'
        placeholder={t('password')}
        value={pw}
        onChange={onChange}
        disabled={submitting}
        autoFocus
      />
      <button className='mb-btn' type='submit' disabled={pw === '' || submitting}>
        {submitting ? t('Unlocking...') : t('Unlock')}
      </button>
      <div className='mb-err' aria-live='polite'>{err}</div>
    </form>
  )
}
