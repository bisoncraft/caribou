import t, { tvar } from '@caribou/shared/intl'
import { useTermsAcceptance } from '@caribou/shared/useTermsAcceptance'
import LegalText from '@caribou/shared/components/LegalText'
import LocaleSelector from './LocaleSelector'
import { CheckRow, Spinner } from './ui'

interface TermsScreenProps {
  onAccepted: () => void
}

// TermsScreen asks the user to accept the current Terms of Use and Privacy
// Policy. It runs before wallet creation on a fresh install, and again after
// unlock whenever the accepted version differs from the current one. On a
// fresh install it is the only screen before setup where the language can be
// changed, so it carries the language selector.
export default function TermsScreen ({ onAccepted }: TermsScreenProps) {
  const { doc, agreed, setAgreed, declined, setDeclined, submitting, err, load, accept, canAccept } =
    useTermsAcceptance(onAccepted)

  return (
    <form className='frame' onSubmit={e => { e.preventDefault(); accept() }}>
      <div className='setup-top end'>
        <LocaleSelector />
      </div>
      <div className='setup-main'>
        <h1 className='setup-title'>{t('Before you start')}</h1>
        <p className='setup-sub'>{t('terms_intro')}</p>
        <div className='legal' tabIndex={0} role='region' aria-label={t('terms_intro')}>
          {doc
            ? <>
                <LegalText markdown={doc.terms} />
                <LegalText markdown={doc.privacy} />
              </>
            : !err && <div className='center-col'><Spinner large /></div>}
        </div>
        {doc && <div className='legal-ver'>{tvar('terms_version', { version: doc.version })}</div>}
        <div className='mt16'>
          <CheckRow checked={agreed} disabled={!doc || submitting} onChange={setAgreed}>
            {t('terms_agree')}
          </CheckRow>
        </div>
        {err && <div className='err-box mt12' aria-live='polite'>{err}</div>}
        {err && !doc && <button className='btn secondary mt12' type='button' onClick={load}>{t('Retry')}</button>}
        {declined && <p className='grey fs13 mt12'>{t('terms_declined')}</p>}
        <button className='btn mt16' type='submit' disabled={!canAccept}>
          {submitting ? t('Submitting...') : t('Accept and continue')}
        </button>
        <button className='btn quiet mt4' type='button' disabled={submitting} onClick={() => setDeclined(true)}>
          {t('Decline')}
        </button>
      </div>
    </form>
  )
}
