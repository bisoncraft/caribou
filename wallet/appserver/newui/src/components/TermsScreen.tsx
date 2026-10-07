import { useEffect, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import { getJSON, postJSON } from '@caribou/shared/http'
import LegalText from '@caribou/shared/components/LegalText'
import Checkbox from './Checkbox'
import LocaleSelector from './LocaleSelector'
import Spinner from './Spinner'

interface LegalDoc {
  version: string
  terms: string
  privacy: string
}

export interface TermsScreenParams {
  onAccepted: () => void
}

/*
 * TermsScreen asks the user to accept the current Terms of Use and Privacy
 * Policy. It runs before the init wizard on a fresh install, and again after
 * unlock whenever the accepted version differs from the current one.
 */
export default function TermsScreen ({ onAccepted }: TermsScreenParams) {
  const [doc, setDoc] = useState<LegalDoc | null>(null)
  const [agreed, setAgreed] = useState(false)
  const [declined, setDeclined] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState('')

  const load = () => {
    setErr('')
    getJSON('/api/terms').then(r => {
      if (r.ok) setDoc(r)
      else setErr(r.errMsg || r.msg || t('Could not load the terms.'))
    })
  }

  useEffect(load, [])

  const accept = async () => {
    if (!doc || !agreed || submitting) return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/acceptterms', { version: doc.version })
    setSubmitting(false)
    if (r.ok) onAccepted()
    else setErr(r.errMsg || r.msg || t('Could not save your acceptance.'))
  }

  const canAccept = doc !== null && agreed && !submitting

  return (
    <div className="fill-abs flex-center init-bg">
      <div className="top-right-floater">
        <LocaleSelector />
      </div>
      <form className="modal form-width-500" onSubmit={e => { e.preventDefault(); accept() }}>
        <header>
          <img className="logo-full small me-2" />
          <span>{t('Before you start')}</span>
        </header>
        <div className="px-3 pb-2 fs14 grey">{t('terms_intro')}</div>
        <div className="terms-body mx-3 p-3 fs14 br5 form-input-bg" tabIndex={0} role="region" aria-label={t('terms_intro')}>
          {doc
            ? <>
                <LegalText markdown={doc.terms} />
                <LegalText markdown={doc.privacy} />
              </>
            : !err && <Spinner />}
        </div>
        {doc && <div className="px-3 pt-1 fs12 grey mono">{tvar('terms_version', { version: doc.version })}</div>}
        <label className="checkbox-row">
          <Checkbox checked={agreed} onChange={setAgreed} disabled={!doc || submitting} />
          <span className="fs14">{t('terms_agree')}</span>
        </label>
        <div className="px-3" aria-live="polite">
          {err && <div className="form-err flex-center errcolor text-center">{err}</div>}
          {err && !doc && <button type="button" className="w-100 mt-2" onClick={load}>{t('Retry')}</button>}
          {declined && <div className="fs14 text-center">{t('terms_declined')}</div>}
        </div>
        <div className="d-flex p-3">
          <button type="button" className="flex-grow-1 me-1" disabled={submitting} onClick={() => setDeclined(true)}>
            {t('Decline')}
          </button>
          <button type="submit" className="feature flex-grow-1 ms-1" disabled={!canAccept}>
            {submitting
              ? (
                <span className="d-inline-flex align-items-center justify-content-center">
                  <Spinner className="me-2" />
                  <span>{t('Submitting...')}</span>
                </span>
                )
              : t('Accept and continue')}
          </button>
        </div>
      </form>
    </div>
  )
}
