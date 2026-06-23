import { useState, ChangeEvent, FormEvent } from 'react'
import t from '@bossw/shared/intl'
import PasswordInput from './PasswordInput'
import LocaleSelector from './LocaleSelector'
import Spinner from './Spinner'
import { postJSON } from '@bossw/shared/http'

interface UnlockedFormParams {
  setUnlocked: () => void
}

export default function UnlockForm ({ setUnlocked }: UnlockedFormParams) {
  const [pw, setPW] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const pwChanged = (e: ChangeEvent<HTMLInputElement>) => {
    setPW(e.target.value)
    if (err) setErr('') // user is correcting input — clear the prior error
  }

  const submitPW = async () => {
    if (submitting || pw === '') return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/login', { pass: pw })
    setSubmitting(false)
    if (r.ok) {
      setUnlocked()
      return
    }
    setErr(r.errMsg || r.msg || t('Incorrect password.'))
  }

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    submitPW()
  }

  const canSubmit = pw !== '' && !submitting

  return (
    <div className="forms">
      <div className="top-right-floater">
        <LocaleSelector />
      </div>
      <form className="modal form-width-250" onSubmit={onSubmit}>
        <header>
          <img className="logo-full small me-2" />
          <span>{t('Welcome Back!')}</span>
        </header>
        <div className="flex-center px-3">
          <PasswordInput
            value={pw}
            onChange={pwChanged}
            disabled={submitting}
            autoFocus
          />
        </div>
        <div className="flex-stretch-column p-3">
          <button type="submit" disabled={!canSubmit} className="feature">
            {submitting
              ? (
                <span className="d-inline-flex align-items-center justify-content-center">
                  <Spinner className="me-2" />
                  <span>{t('Unlocking...')}</span>
                </span>
                )
              : t('Unlock')}
          </button>
        </div>
        <div className="px-3 pb-3" aria-live="polite">
          {err && (
            <div key={err} className="form-err flex-center errcolor text-center">
              {err}
            </div>
          )}
        </div>
      </form>
    </div>
  )
}
