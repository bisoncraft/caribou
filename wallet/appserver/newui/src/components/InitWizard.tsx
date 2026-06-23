import { useState, FormEvent } from 'react'
import t from '@bossw/shared/intl'
import PasswordInput from './PasswordInput'
import LocaleSelector from './LocaleSelector'
import Spinner from './Spinner'
import { postJSON } from '@bossw/shared/http'
import { normalizeSeed, seedWordCount } from '@bossw/shared/seed'

/*
 * SeedPromptForm asks the user whether they are restoring from an existing
 * seed. It is the first screen of the init wizard.
 */
interface SeedPromptFormParams { setUsingSeed: (y: boolean) => void }

const SeedPromptForm = ({ setUsingSeed }: SeedPromptFormParams) => {
  const onUsingSeedClicked = () => setUsingSeed(true)
  const onNoSeedClicked = () => setUsingSeed(false)

  return (
    <form className="modal" onSubmit={(e: FormEvent) => e.preventDefault()}>
      <header>
        <img className="logo-full small me-2" />
        <span>{t('Welcome to Boss Wallet!')}</span>
      </header>
      <div className="px-3">{t('prompt_for_seed')}</div>
      <div className="d-flex align-items-center p-3">
        <button type="button" className="fs16 me-1 flex-grow-1" onClick={onUsingSeedClicked}>{t('Yes')}</button>
        <button type="button" className="feature fs16 ms-1 flex-grow-1" onClick={onNoSeedClicked}>{t('No')}</button>
      </div>
    </form>
  )
}

interface PasswordFormParams {
  setRegistered: (y: boolean) => void
  setSeed: (s: string) => void
  setUsingSeed: (y: boolean | undefined) => void
  seed: string
}

const PasswordForm = ({ setRegistered, setUsingSeed, setSeed, seed }: PasswordFormParams) => {
  const [pw, setPW] = useState('')
  const [pw2, setPW2] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const onGoBackClicked = () => {
    setPW('')
    setPW2('')
    setErr('')
    // Clear any validated restore seed too, otherwise re-picking "No" on the
    // prompt would silently create a wallet from the discarded seed, and
    // re-picking "Yes" would skip the SeedInput screen.
    setSeed('')
    setUsingSeed(undefined)
  }

  const submitPW = async () => {
    if (submitting) return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/init', { pass: pw, seed })
    setSubmitting(false)
    if (r.ok) {
      setRegistered(true)
      if (r.mnemonic) setSeed(r.mnemonic)
    } else {
      setErr(r.errMsg || r.msg || t('Initialization failed.'))
    }
  }

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    submitPW()
  }

  const canSubmit = pw.trim() !== '' && pw === pw2 && !submitting

  return (
    <form className="modal form-width-300" onSubmit={onSubmit}>
      <div className="fs18 p-3 text-center">{t('Set a password for your wallet')}</div>
      <div className="flex-stretch-column px-3">
        <div className="mb-2">
          <PasswordInput
            onChange={(e) => { setPW(e.target.value); if (err) setErr('') }}
            value={pw}
            disabled={submitting}
            autoFocus
          />
        </div>
        <div>
          <PasswordInput
            placeholder={t('password again')}
            onChange={(e) => { setPW2(e.target.value); if (err) setErr('') }}
            value={pw2}
            disabled={submitting}
          />
        </div>
      </div>
      <div className="px-3 pt-2" aria-live="polite">
        {err && (
          <div key={err} className="form-err flex-center errcolor text-center">
            {err}
          </div>
        )}
      </div>
      <div className="flex-stretch-column p-3">
        <button type="submit" className="feature w-100" disabled={!canSubmit}>
          {submitting
            ? (
              <span className="d-inline-flex align-items-center justify-content-center">
                <Spinner className="me-2" />
                <span>{t('Submitting...')}</span>
              </span>
              )
            : t('Submit')}
        </button>
        <button type="button" className="w-100 mt-2" onClick={onGoBackClicked} disabled={submitting}>
          {t('Go Back')}
        </button>
      </div>
    </form>
  )
}

interface SeedInputParams {
  setSeed: (s: string) => void
  setUsingSeed: (y: boolean | undefined) => void
}

export const SeedInput = ({ setSeed, setUsingSeed }: SeedInputParams) => {
  const [err, setErr] = useState('')
  const [workingSeed, setWorkingSeed] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const onChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    setWorkingSeed(e.target.value)
    if (err) setErr('')
  }

  const onGoBackClicked = () => setUsingSeed(undefined)

  const submitSeed = async () => {
    if (submitting) return
    const seed = normalizeSeed(workingSeed)
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/validateseed', { seed })
    setSubmitting(false)
    if (r.ok) {
      setSeed(seed)
    } else {
      setErr(r.errMsg || r.msg || t('Invalid seed.'))
    }
  }

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    submitSeed()
  }

  const wordCount = seedWordCount(workingSeed)
  const canSubmit = wordCount > 0 && !submitting

  return (
    <form className="modal" onSubmit={onSubmit}>
      <div className="fs18 p-3 text-center">{t('Input your recovery seed')}</div>
      <div className="d-flex px-3">
        <textarea
          className="fs14"
          onChange={onChange}
          value={workingSeed}
          placeholder={t('seed_textarea_placeholder')}
          disabled={submitting}
          autoFocus
        />
      </div>
      <div className="px-3 pt-1 fs12 text-center" style={{ opacity: wordCount === 0 ? 0 : 1 }}>
        {wordCount} {wordCount === 1 ? t('word') : t('words')}
      </div>
      <div className="px-3 pt-1" aria-live="polite">
        {err && (
          <div key={err} className="form-err flex-center errcolor text-center">
            {err}
          </div>
        )}
      </div>
      <div className="d-flex flex-column p-3">
        <button
          type="submit"
          className="feature w-100"
          disabled={!canSubmit}>
          {submitting
            ? (
              <span className="d-inline-flex align-items-center justify-content-center">
                <Spinner className="me-2" />
                <span>{t('Validating...')}</span>
              </span>
              )
            : t('Recover')}
        </button>
        <button
          type="button"
          className="w-100 mt-2"
          disabled={submitting}
          onClick={onGoBackClicked}>
          {t('Go Back')}
        </button>
      </div>
    </form>
  )
}

interface SeedDisplayParams {
  acked: () => void
  seed: string | undefined
}

export const SeedDisplay = ({ acked, seed }: SeedDisplayParams) => {
  return (
    <form className="modal form-width-300" onSubmit={(e: FormEvent) => e.preventDefault()}>
      <div className="pt-3 px-3">{t('seed_display_warning')}</div>
      {seed
        ? <div className="mt-3 mx-3 p-2 fs14 form-input-bg mono">{seed}</div>
        : null}
      <div className="flex-stretch-column p-3">
        <button type="button" className="feature w-100" onClick={acked}>
          {t('I have written down my seed')}
        </button>
      </div>
    </form>
  )
}

export interface InitWizardParams {
  setInited: () => void
}

export default function InitWizard ({ setInited }: InitWizardParams) {
  const [usingSeed, setUsingSeed] = useState<boolean | undefined>(undefined)
  const [seed, setSeed] = useState<string | undefined>(undefined)
  const [registered, setRegistered] = useState<boolean>(false)

  const done = () => {
    setSeed(undefined)
    setInited()
  }

  const registerFormFeedback = () => {
    setRegistered(true)
    if (usingSeed) done()
  }

  const renderContent = () => {
    if (usingSeed === undefined) return <SeedPromptForm setUsingSeed={setUsingSeed} />
    if (usingSeed && !seed) return <SeedInput setSeed={setSeed} setUsingSeed={setUsingSeed} />
    if (!registered) return <PasswordForm setRegistered={registerFormFeedback} setSeed={setSeed} setUsingSeed={setUsingSeed} seed={seed ?? ''} />
    return <SeedDisplay acked={done} seed={seed} />
  }

  return (
    <div className="fill-abs flex-center init-bg">
      <div className="top-right-floater">
        <LocaleSelector />
      </div>
      {renderContent()}
    </div>
  )
}
