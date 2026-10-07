import { useEffect, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import { getJSON, postJSON } from '@caribou/shared/http'
import LegalText from '@caribou/shared/components/LegalText'

interface LegalDoc {
  version: string
  terms: string
  privacy: string
}

interface TermsScreenProps {
  onAccepted: () => void
}

// TermsScreen asks the user to accept the current Terms of Use and Privacy
// Policy. It runs before wallet creation on a fresh install, and again after
// unlock whenever the accepted version differs from the current one.
export default function TermsScreen ({ onAccepted }: TermsScreenProps) {
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

  return (
    <div className='mb-screen mb-init'>
      <form className='mb-init-card' onSubmit={e => { e.preventDefault(); accept() }}>
        <h2>{t('Before you start')}</h2>
        <p className='mb-muted'>{t('terms_intro')}</p>
        <div className='mb-terms-body' tabIndex={0} role='region' aria-label={t('terms_intro')}>
          {doc
            ? <>
                <LegalText markdown={doc.terms} />
                <LegalText markdown={doc.privacy} />
              </>
            : !err && <div className='mb-spinner' />}
        </div>
        {doc && <div className='mb-muted mb-init-hint'>{tvar('terms_version', { version: doc.version })}</div>}
        <label className='mb-check-row'>
          <input type='checkbox' checked={agreed} disabled={!doc || submitting} onChange={e => setAgreed(e.target.checked)} />
          <span>{t('terms_agree')}</span>
        </label>
        <div className='mb-err' aria-live='polite'>{err}</div>
        {err && !doc && <button className='mb-btn mb-btn-secondary' type='button' onClick={load}>{t('Retry')}</button>}
        {declined && <p className='mb-muted'>{t('terms_declined')}</p>}
        <button className='mb-btn' type='submit' disabled={!doc || !agreed || submitting}>
          {submitting ? t('Submitting...') : t('Accept and continue')}
        </button>
        <button className='mb-btn mb-btn-secondary' type='button' disabled={submitting} onClick={() => setDeclined(true)}>
          {t('Decline')}
        </button>
      </form>
    </div>
  )
}
