import { useState, FormEvent, ChangeEvent } from 'react'
import t from '@caribou/shared/intl'
import { postJSON } from '@caribou/shared/http'
import { normalizeSeed, seedWordCount } from '@caribou/shared/seed'

// InitWizard is the mobile wallet-creation flow. It mirrors the desktop wizard
// state machine (seed prompt -> optional seed restore -> password -> seed
// backup) but with the mobile single-column, native-input layout. It drives the
// same /api/validateseed and /api/init endpoints over the shared transport.

interface SeedPromptProps {
  setUsingSeed: (y: boolean) => void
}

const SeedPrompt = ({ setUsingSeed }: SeedPromptProps) => (
  <div className='mb-init-card'>
    <h1 className='mb-brand'>{t('Welcome to Caribou!')}</h1>
    <p className='mb-muted'>{t('prompt_for_seed')}</p>
    <button className='mb-btn' type='button' onClick={() => setUsingSeed(false)}>{t('No')}</button>
    <button className='mb-btn mb-btn-secondary' type='button' onClick={() => setUsingSeed(true)}>{t('Yes')}</button>
  </div>
)

interface SeedInputProps {
  setSeed: (s: string) => void
  goBack: () => void
}

const SeedInput = ({ setSeed, goBack }: SeedInputProps) => {
  const [workingSeed, setWorkingSeed] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const onChange = (e: ChangeEvent<HTMLTextAreaElement>) => {
    setWorkingSeed(e.target.value)
    if (err) setErr('')
  }

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault()
    if (submitting) return
    const seed = normalizeSeed(workingSeed)
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/validateseed', { seed })
    setSubmitting(false)
    if (r.ok) setSeed(seed)
    else setErr(r.errMsg || r.msg || t('Invalid seed.'))
  }

  const wordCount = seedWordCount(workingSeed)

  return (
    <form className='mb-init-card' onSubmit={onSubmit}>
      <h2>{t('Input your recovery seed')}</h2>
      <textarea
        className='mb-textarea'
        value={workingSeed}
        onChange={onChange}
        placeholder={t('seed_textarea_placeholder')}
        disabled={submitting}
        autoFocus
      />
      <div className={`mb-muted mb-init-hint ${wordCount === 0 ? 'mb-hidden' : ''}`}>
        {wordCount} {wordCount === 1 ? t('word') : t('words')}
      </div>
      <div className='mb-err' aria-live='polite'>{err}</div>
      <button className='mb-btn' type='submit' disabled={wordCount === 0 || submitting}>
        {submitting ? t('Validating...') : t('Recover')}
      </button>
      <button className='mb-btn mb-btn-secondary' type='button' onClick={goBack} disabled={submitting}>
        {t('Go Back')}
      </button>
    </form>
  )
}

interface PasswordFormProps {
  seed: string
  onRegistered: (mnemonic?: string) => void
  goBack: () => void
}

const PasswordForm = ({ seed, onRegistered, goBack }: PasswordFormProps) => {
  const [pw, setPW] = useState('')
  const [pw2, setPW2] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const clearErr = () => { if (err) setErr('') }

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault()
    if (submitting) return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/init', { pass: pw, seed })
    setSubmitting(false)
    if (r.ok) onRegistered(r.mnemonic)
    else setErr(r.errMsg || r.msg || t('Initialization failed.'))
  }

  const canSubmit = pw.trim() !== '' && pw === pw2 && !submitting

  return (
    <form className='mb-init-card' onSubmit={onSubmit}>
      <h2>{t('Set a password for your wallet')}</h2>
      <input
        className='mb-input'
        type='password'
        autoComplete='new-password'
        placeholder={t('password')}
        value={pw}
        onChange={(e) => { setPW(e.target.value); clearErr() }}
        disabled={submitting}
        autoFocus
      />
      <input
        className='mb-input'
        type='password'
        autoComplete='new-password'
        placeholder={t('password again')}
        value={pw2}
        onChange={(e) => { setPW2(e.target.value); clearErr() }}
        disabled={submitting}
      />
      <div className='mb-err' aria-live='polite'>{err}</div>
      <button className='mb-btn' type='submit' disabled={!canSubmit}>
        {submitting ? t('Submitting...') : t('Submit')}
      </button>
      <button className='mb-btn mb-btn-secondary' type='button' onClick={goBack} disabled={submitting}>
        {t('Go Back')}
      </button>
    </form>
  )
}

interface SeedDisplayProps {
  seed?: string
  acked: () => void
}

const SeedDisplay = ({ seed, acked }: SeedDisplayProps) => (
  <div className='mb-init-card'>
    <p>{t('seed_display_warning')}</p>
    {seed && <div className='mb-seed'>{seed}</div>}
    <button className='mb-btn' type='button' onClick={acked}>
      {t('I have written down my seed')}
    </button>
  </div>
)

interface InitWizardProps {
  setInited: () => void
}

export default function InitWizard ({ setInited }: InitWizardProps) {
  const [usingSeed, setUsingSeed] = useState<boolean | undefined>(undefined)
  const [seed, setSeed] = useState<string | undefined>(undefined)
  const [registered, setRegistered] = useState(false)

  const done = () => {
    setSeed(undefined)
    setInited()
  }

  // Restoring from a seed: the user already has it, so skip the backup screen.
  // Creating fresh: show the generated mnemonic for backup before finishing. If
  // the backend returns no mnemonic (it always should on a fresh create), just
  // finish rather than showing a backup screen with no seed to write down.
  const onRegistered = (mnemonic?: string) => {
    if (usingSeed || !mnemonic) { done(); return }
    setSeed(mnemonic)
    setRegistered(true)
  }

  const resetToPrompt = () => {
    setSeed('')
    setUsingSeed(undefined)
  }

  let content
  if (usingSeed === undefined) {
    content = <SeedPrompt setUsingSeed={setUsingSeed} />
  } else if (usingSeed && !seed) {
    content = <SeedInput setSeed={setSeed} goBack={() => setUsingSeed(undefined)} />
  } else if (!registered) {
    content = <PasswordForm seed={seed ?? ''} onRegistered={onRegistered} goBack={resetToPrompt} />
  } else {
    content = <SeedDisplay seed={seed} acked={done} />
  }

  return <div className='mb-screen mb-init'>{content}</div>
}
