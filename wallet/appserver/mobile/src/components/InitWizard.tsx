import { useMemo, useState, FormEvent } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import { postJSON } from '@caribou/shared/http'
import { normalizeSeed, seedWordCount } from '@caribou/shared/seed'
import LocaleSelector from './LocaleSelector'
import { CheckRow, Icon, PasswordField, Segments } from './ui'

// InitWizard is the mobile wallet-creation flow, after the terms are
// accepted. Creating a wallet runs password -> seed backup -> seed quiz;
// restoring runs seed entry -> password. Both drive the same /api/validateseed
// and /api/init endpoints as the desktop wizard.

type Step = 'welcome' | 'seed' | 'password' | 'backup' | 'verify'

interface SetupTopProps {
  onBack?: () => void
  step: number
  steps: number
}

// SetupTop is the setup header: a back button and the progress segments.
function SetupTop ({ onBack, step, steps }: SetupTopProps) {
  return (
    <div className='setup-top'>
      {onBack
        ? <button type='button' className='ib' onClick={onBack} aria-label={t('Back')}><Icon name='chevron-left' /></button>
        : <span className='slot' />}
      <Segments n={step} of={steps} />
    </div>
  )
}

function Welcome ({ onCreate, onRestore }: { onCreate: () => void, onRestore: () => void }) {
  return (
    <div className='frame'>
      <div className='setup-top end'>
        <LocaleSelector />
      </div>
      <div className='welcome-hero'>
        <span className='mark lg' role='img' aria-label='Caribou' />
        <h1>Caribou</h1>
        <p>{t('Your personal treasury.')}</p>
      </div>
      <div className='foot welcome-foot'>
        <button type='button' className='btn' onClick={onCreate}>{t('Create a new wallet')}</button>
        <button type='button' className='btn secondary' onClick={onRestore}>{t('Restore from seed')}</button>
        <p className='grey fs13 center mt8'>{t('Caribou is self-custodial. Only you hold the keys.')}</p>
      </div>
    </div>
  )
}

interface SeedInputProps {
  setSeed: (s: string) => void
  goBack: () => void
}

function SeedInput ({ setSeed, goBack }: SeedInputProps) {
  const [workingSeed, setWorkingSeed] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

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
    <form className='frame' onSubmit={onSubmit}>
      <SetupTop onBack={goBack} step={1} steps={2} />
      <div className='setup-main'>
        <h1 className='setup-title'>{t('Restore from seed')}</h1>
        <p className='setup-sub'>{t('Enter your recovery seed words in order, separated by spaces.')}</p>
        <div className={err ? 'area bad mt24' : 'area mt24'}>
          <textarea
            className='mono'
            rows={5}
            value={workingSeed}
            onChange={e => { setWorkingSeed(e.target.value); if (err) setErr('') }}
            placeholder={t('seed_textarea_placeholder')}
            aria-label={t('Recovery seed')}
            autoCapitalize='off'
            autoCorrect='off'
            autoComplete='off'
            spellCheck={false}
            disabled={submitting}
            autoFocus
          />
        </div>
        <div className='grey fs13 mt8 ms4'>
          {wordCount > 0 && `${wordCount} ${wordCount === 1 ? t('word') : t('words')}`}
        </div>
        {err && <div className='err-box mt12' aria-live='polite'>{err}</div>}
        <button className='btn mt-auto' type='submit' disabled={wordCount === 0 || submitting}>
          {submitting ? t('Validating...') : t('Continue')}
        </button>
      </div>
    </form>
  )
}

interface PasswordFormProps {
  seed: string
  restoring: boolean
  onRegistered: (mnemonic?: string) => void
  goBack: () => void
}

function PasswordForm ({ seed, restoring, onRegistered, goBack }: PasswordFormProps) {
  const [pw, setPW] = useState('')
  const [pw2, setPW2] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault()
    if (!canSubmit) return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/init', { pass: pw, seed })
    setSubmitting(false)
    if (r.ok) onRegistered(r.mnemonic)
    else setErr(r.errMsg || r.msg || t('Initialization failed.'))
  }

  const matches = pw !== '' && pw === pw2
  const mismatch = pw2 !== '' && !pw.startsWith(pw2) && pw !== pw2
  const canSubmit = pw.trim() !== '' && matches && !submitting

  return (
    <form className='frame' onSubmit={onSubmit}>
      <SetupTop onBack={submitting ? undefined : goBack} step={restoring ? 2 : 1} steps={restoring ? 2 : 3} />
      <div className='setup-main'>
        <h1 className='setup-title'>{t('Set a password')}</h1>
        <p className='setup-sub'>{t('It unlocks Caribou on this device and approves sends. It cannot be recovered, but your seed can always restore the wallet.')}</p>
        <label className='flbl mt24' htmlFor='pw1'>{t('Password')}</label>
        <PasswordField
          id='pw1'
          value={pw}
          onChange={v => { setPW(v); if (err) setErr('') }}
          autoComplete='new-password'
          label={t('Password')}
          disabled={submitting}
          autoFocus
        />
        <label className='flbl mt16' htmlFor='pw2'>{t('Confirm password')}</label>
        <PasswordField
          id='pw2'
          value={pw2}
          onChange={v => { setPW2(v); if (err) setErr('') }}
          autoComplete='new-password'
          label={t('Confirm password')}
          disabled={submitting}
          bad={mismatch}
          adornment={matches ? <span className='ib pos fs16' aria-hidden='true'><Icon name='check' /></span> : undefined}
        />
        {matches && <div className='field-ok'>{t('Passwords match')}</div>}
        {mismatch && <div className='field-err'>{t('Passwords do not match')}</div>}
        {err && <div className='err-box mt12' aria-live='polite'>{err}</div>}
        <button className='btn mt-auto' type='submit' disabled={!canSubmit}>
          {submitting ? t('Creating wallet...') : t('Continue')}
        </button>
      </div>
    </form>
  )
}

interface SeedBackupProps {
  words: string[]
  onContinue: () => void
}

function SeedBackup ({ words, onContinue }: SeedBackupProps) {
  const [hidden, setHidden] = useState(false)
  const [written, setWritten] = useState(false)
  const n = String(words.length)
  return (
    <div className='frame'>
      <SetupTop step={2} steps={3} />
      <div className='setup-main'>
        <h1 className='setup-title'>{t('Write down your seed')}</h1>
        <p className='setup-sub'>{tvar('These {{ n }} words restore your wallet if this device is lost. Write them on paper, in order.', { n })}</p>
        <div className='callout caution strong mt16'>
          <Icon name='key' />
          <span>{t('Anyone with these words controls this wallet\'s funds, on any device. Never photograph them or type them into another app.')}</span>
        </div>
        <SeedWords words={words} hidden={hidden} />
        <div className='center mt8'>
          <button type='button' className='btn quiet inline' onClick={() => setHidden(!hidden)}>
            <Icon name={hidden ? 'eyeopen' : 'eyeclosed'} />
            {hidden ? t('Show words') : t('Hide words')}
          </button>
        </div>
        <div className='mt-auto pt16'>
          <CheckRow checked={written} onChange={setWritten}>
            {tvar('I have written down all {{ n }} words in order.', { n })}
          </CheckRow>
        </div>
        <button type='button' className='btn mt16' disabled={!written} onClick={onContinue}>{t('Continue')}</button>
      </div>
    </div>
  )
}

// SeedWords is the numbered three-column seed grid. hidden blurs the words in
// place so the layout does not jump.
export function SeedWords ({ words, hidden }: { words: string[], hidden?: boolean }) {
  return (
    <ol className={hidden ? 'words hidden mt16' : 'words mt16'} aria-label={t('Recovery seed')}>
      {words.map((w, i) => (
        <li key={i}><span className='n'>{i + 1}</span><span className='w'>{w}</span></li>
      ))}
    </ol>
  )
}

interface Question {
  pos: number // zero-based word index
  answer: string
  opts: string[]
}

function shuffle<T> (a: T[]): T[] {
  const out = [...a]
  for (let i = out.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1))
    ;[out[i], out[j]] = [out[j], out[i]]
  }
  return out
}

// makeQuiz picks three word positions, each with the right word and two
// distractors drawn from the seed's other words.
function makeQuiz (words: string[]): Question[] {
  const positions = shuffle(words.map((_, i) => i)).slice(0, 3).sort((a, b) => a - b)
  return positions.map(pos => {
    const answer = words[pos]
    const others = shuffle(Array.from(new Set(words.filter(w => w !== answer)))).slice(0, 2)
    return { pos, answer, opts: shuffle([answer, ...others]) }
  })
}

function SeedVerify ({ words, onBack, onDone }: { words: string[], onBack: () => void, onDone: () => void }) {
  const quiz = useMemo(() => makeQuiz(words), [words])
  const [picks, setPicks] = useState<Record<number, string>>({})
  const allPicked = quiz.every((_, i) => picks[i] !== undefined)
  const correct = quiz.every((q, i) => picks[i] === q.answer)
  return (
    <div className='frame'>
      <SetupTop onBack={onBack} step={3} steps={3} />
      <div className='setup-main'>
        <h1 className='setup-title'>{t('Confirm your seed')}</h1>
        <p className='setup-sub'>{t('Pick the right word for each position to show the backup is complete.')}</p>
        <div className='quiz mt24'>
          {quiz.map((q, qi) => (
            <fieldset key={q.pos} className='q'>
              <legend className='ql'>{t('Word')} <b>#{q.pos + 1}</b></legend>
              <div className='opts'>
                {q.opts.map(o => {
                  const on = picks[qi] === o
                  return (
                    <button
                      key={o}
                      type='button'
                      className={on ? 'opt on' : 'opt'}
                      aria-pressed={on}
                      onClick={() => setPicks({ ...picks, [qi]: o })}
                    >
                      {on && <Icon name='check' />}{o}
                    </button>
                  )
                })}
              </div>
            </fieldset>
          ))}
        </div>
        {allPicked && !correct && <div className='field-err'>{t('One or more words do not match your seed. Check your backup.')}</div>}
        <button type='button' className='btn mt-auto' disabled={!correct} onClick={onDone}>{t('Finish setup')}</button>
      </div>
    </div>
  )
}

interface InitWizardProps {
  setInited: () => void
}

export default function InitWizard ({ setInited }: InitWizardProps) {
  const [step, setStep] = useState<Step>('welcome')
  const [restoring, setRestoring] = useState(false)
  const [seed, setSeed] = useState('')
  const [mnemonic, setMnemonic] = useState<string[]>([])

  const done = () => {
    setSeed('')
    setMnemonic([])
    setInited()
  }

  // Restoring: the user already holds the seed, so there is nothing to back
  // up. Creating: back up the generated mnemonic, then quiz it. If the
  // backend returns no mnemonic (it always should on a fresh create), finish
  // rather than show a backup screen with nothing to write down.
  const onRegistered = (m?: string) => {
    if (restoring || !m) { done(); return }
    setMnemonic(m.trim().split(/\s+/))
    setStep('backup')
  }

  switch (step) {
    case 'welcome':
      return (
        <Welcome
          onCreate={() => { setRestoring(false); setSeed(''); setStep('password') }}
          onRestore={() => { setRestoring(true); setStep('seed') }}
        />
      )
    case 'seed':
      return <SeedInput setSeed={s => { setSeed(s); setStep('password') }} goBack={() => setStep('welcome')} />
    case 'password':
      return (
        <PasswordForm
          seed={seed}
          restoring={restoring}
          onRegistered={onRegistered}
          goBack={() => setStep(restoring ? 'seed' : 'welcome')}
        />
      )
    case 'backup':
      return <SeedBackup words={mnemonic} onContinue={() => setStep('verify')} />
    case 'verify':
      return <SeedVerify words={mnemonic} onBack={() => setStep('backup')} onDone={done} />
  }
}
