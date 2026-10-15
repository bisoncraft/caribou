import t, { tvar } from '@caribou/shared/intl'
import { useTermsAcceptance } from '@caribou/shared/useTermsAcceptance'
import LegalText from '@caribou/shared/components/LegalText'
import LocaleSelector from './LocaleSelector'

interface TermsScreenProps {
  onAccepted: () => void
}

// TermsScreen asks the user to accept the current Terms of Use and Privacy
// Policy. It runs before wallet creation on a fresh install, and again after
// unlock whenever the accepted version differs from the current one.
export default function TermsScreen ({ onAccepted }: TermsScreenProps) {
  const { doc, agreed, setAgreed, declined, setDeclined, submitting, err, load, accept, canAccept } =
    useTermsAcceptance(onAccepted)

  return (
    <div className='mb-screen mb-init'>
      <form className='mb-init-card' onSubmit={e => { e.preventDefault(); accept() }}>
        <LocaleSelector />
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
        <button className='mb-btn' type='submit' disabled={!canAccept}>
          {submitting ? t('Submitting...') : t('Accept and continue')}
        </button>
        <button className='mb-btn mb-btn-secondary' type='button' disabled={submitting} onClick={() => setDeclined(true)}>
          {t('Decline')}
        </button>
      </form>
    </div>
  )
}
