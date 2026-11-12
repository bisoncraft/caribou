import { useState, useEffect, FormEvent, ReactNode } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { getJSON, postJSON } from '@caribou/shared/http'
import { useFlash } from '@caribou/shared/hooks'
import { Theme, themes, storedTheme, setTheme } from '@caribou/shared/theme'
import LegalText from '@caribou/shared/components/LegalText'
import { CaribouBridge, bridge, canExportAppLog } from '../bridge'
import { Icon, PasswordField, Sheet, Spinner, Toast, Toggle } from './ui'
import { languageName, useLocale } from './LocaleSelector'
import { SeedWords } from './InitWizard'

interface SettingRowProps {
  title: ReactNode
  desc?: ReactNode
  children?: ReactNode
  onClick?: () => void
  icon?: ReactNode
}

// SettingRow is one row of a settings card: title and description on the
// left, a value or control on the right. With onClick the whole row is a
// button.
function SettingRow ({ title, desc, children, onClick, icon }: SettingRowProps) {
  const inner = (
    <>
      {icon}
      <div className='grow'>
        <div className='st'>{title}</div>
        {desc && <div className='sd'>{desc}</div>}
      </div>
      {children}
    </>
  )
  return onClick
    ? <button type='button' className='srow tappable' onClick={onClick}>{inner}</button>
    : <div className='srow'>{inner}</div>
}

const Chevron = () => <Icon name='chevron-left' className='chev' />

interface SelectValueProps<T extends string | number> {
  value: T
  options: { value: T, label: string }[]
  onChange: (v: T) => void
  label: string
}

// SelectValue shows the current choice with a chevron over an invisible
// native select, so the platform picker opens on tap.
function SelectValue<T extends string | number> ({ value, options, onChange, label }: SelectValueProps<T>) {
  const current = options.find(o => o.value === value)
  return (
    <span className='select-val'>
      {current?.label ?? ''}<Chevron />
      <select
        value={String(value)}
        aria-label={label}
        onChange={e => {
          const o = options.find(x => String(x.value) === e.target.value)
          if (o) onChange(o.value)
        }}
      >
        {options.map(o => <option key={String(o.value)} value={String(o.value)}>{o.label}</option>)}
      </select>
    </span>
  )
}

function LanguageRow () {
  const [lang, langs, change] = useLocale()
  if (langs.length === 0) return null
  return (
    <div className='srow'>
      <div className='grow'>
        <div className='st'>{t('Language')}</div>
        <div className='sd'>{t('Applies across the app, including the unlock screen.')}</div>
      </div>
      <SelectValue value={lang} label={t('Language')} onChange={change} options={langs.map(l => ({ value: l, label: languageName(l) }))} />
    </div>
  )
}

const themeLabel = (th: Theme) => th === 'warm' ? t('Warm') : th === 'cool' ? t('Cool') : t('Light')

function ThemeRow () {
  const [theme, setThemeState] = useState<Theme>(storedTheme())
  return (
    <div className='srow col'>
      <div className='st'>{t('Theme')}</div>
      <div className='swatches' role='radiogroup' aria-label={t('Theme')}>
        {themes.map(th => {
          const on = th === theme
          return (
            <button
              key={th}
              type='button'
              role='radio'
              aria-checked={on}
              className={on ? 'sw on' : 'sw'}
              onClick={() => { setTheme(th); setThemeState(th) }}
            >
              {/* data-theme scopes the palette to the preview, so each
                  swatch shows its theme's live body, fill, and gold tokens. */}
              <span className='swb' data-theme={th}>
                <span />
                <span />
              </span>
              {themeLabel(th)}
            </button>
          )
        })}
      </div>
    </div>
  )
}

// Minute values offered for each idle-lock threshold. 0 is a sentinel: for
// the gate it means "require unlock immediately", for eviction it means
// "never".
const GATE_OPTIONS = [0, 1, 5, 10, 30]
const EVICT_OPTIONS = [30, 60, 120, 0]

function gateLabel (m: number): string {
  return m === 0 ? t('Immediately') : tvar('{{ n }} min', { n: String(m) })
}
function evictLabel (m: number): string {
  if (m === 0) return t('Never')
  if (m % 60 === 0) return m === 60 ? t('1 hour') : tvar('{{ n }} hours', { n: String(m / 60) })
  return tvar('{{ n }} min', { n: String(m) })
}

// BiometricRows renders the native lock controls in tier order: the gate
// threshold ("Lock after"), the biometric toggle, then the eviction threshold
// ("Evict keys after"). They exist only in the native shell (window.caribou).
//
// The biometric toggle governs how keys are re-acquired once they are gone
// (after eviction, or on a cold start): released from the keystore by
// biometrics vs. retyping the password. It does not touch the gate, which is
// a presence check over a still-resident session. It shows when the device
// can enroll OR an enrollment already exists, so a stale enrollment (the
// device's biometrics removed after enrolling) can still be turned off.
//
// The gate row shows only when the device can gate (biometrics OR passcode).
// The gate must sit strictly below eviction or its band [gate, evict) is
// empty, so only lower gate values are offered and lowering eviction pulls
// the gate down with it. Native clamps regardless and is the single
// authority: every write is read back.
function BiometricRows ({ b }: { b: CaribouBridge }) {
  const read = <T, >(f: () => T, fallback: T): T => { try { return f() } catch { return fallback } }
  const [available, setAvailable] = useState(() => read(() => b.biometricAvailable(), false))
  const [canGate, setCanGate] = useState(() => read(() => b.canGate(), false))
  const [enrolled, setEnrolled] = useState(() => read(() => b.biometricEnrolled(), false))
  const [gateMin, setGateMin] = useState(10)
  const [evictMin, setEvictMin] = useState(60)

  useEffect(() => {
    const refresh = () => {
      // Only touch the bridge when visible: visibilitychange also fires on
      // hide, and re-reading then burns synchronous native round-trips on a
      // lifecycle hot path for a view no one is looking at.
      if (document.visibilityState !== 'visible') return
      setAvailable(read(() => b.biometricAvailable(), false))
      setCanGate(read(() => b.canGate(), false))
      setEnrolled(read(() => b.biometricEnrolled(), false))
    }
    window.addEventListener('caribou-biometric-changed', refresh)
    document.addEventListener('visibilitychange', refresh)
    return () => {
      window.removeEventListener('caribou-biometric-changed', refresh)
      document.removeEventListener('visibilitychange', refresh)
    }
  }, [b])

  const readConfig = (gate: number, evict: number) => {
    try {
      const cfg = JSON.parse(b.getLockConfig())
      setGateMin(typeof cfg.gateMin === 'number' ? cfg.gateMin : gate)
      setEvictMin(typeof cfg.evictMin === 'number' ? cfg.evictMin : evict)
    } catch {
      setGateMin(gate)
      setEvictMin(evict)
    }
  }

  useEffect(() => { readConfig(10, 60) }, [b])

  const save = (gate: number, evict: number) => {
    b.setLockConfig(gate, evict)
    readConfig(gate, evict)
  }

  const gateChoices = GATE_OPTIONS.filter(m => evictMin === 0 || m < evictMin)
  // Always include the persisted gate so the select can show it even if the
  // native clamp landed on a value not in GATE_OPTIONS.
  const gateOptions = gateChoices.includes(gateMin) ? gateChoices : [...gateChoices, gateMin].sort((x, y) => x - y)
  const onEvictChange = (evict: number) => {
    let gate = gateMin
    if (evict !== 0 && gate >= evict) {
      const below = GATE_OPTIONS.filter(m => m < evict)
      gate = below.length ? Math.max(...below) : 0
    }
    save(gate, evict)
  }

  // Enrollment is an async native prompt the user can cancel, so the toggle
  // is not flipped optimistically; it follows the stored state via the
  // caribou-biometric-changed event.
  const toggleBio = (on: boolean) => { if (on) b.enrollBiometrics(); else b.disableBiometrics() }

  return (
    <>
      {canGate && (
        <SettingRow title={t('Lock after')} desc={t('Ask for biometrics or the device passcode on return.')}>
          <SelectValue value={gateMin} label={t('Lock after')} onChange={g => save(g, evictMin)} options={gateOptions.map(m => ({ value: m, label: gateLabel(m) }))} />
        </SettingRow>
      )}
      {(available || enrolled) && (
        <SettingRow title={t('Unlock with biometrics')} desc={t('Release keys with your fingerprint or face instead of typing your password.')}>
          <Toggle on={enrolled} onChange={toggleBio} label={t('Unlock with biometrics')} />
        </SettingRow>
      )}
      <SettingRow title={t('Evict keys after')} desc={t('Clears keys from memory. Unlocking then needs biometrics or your password.')}>
        <SelectValue value={evictMin} label={t('Evict keys after')} onChange={onEvictChange} options={EVICT_OPTIONS.map(m => ({ value: m, label: evictLabel(m) }))} />
      </SettingRow>
    </>
  )
}

// SendPasswordRow mirrors the desktop app-level send preference. The value is
// written back to app.appSettings so the send and swap flows pick it up
// without a refetch.
function SendPasswordRow () {
  const [on, setOn] = useState(app.appSettings.sendRequiresPassword)
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')

  const toggle = async (checked: boolean) => {
    if (saving) return
    const prev = on
    setSaving(true)
    setErr('')
    setOn(checked) // optimistic
    const r = await postJSON('/api/setsendrequirespassword', { require: checked })
    setSaving(false)
    if (!r.ok) {
      setOn(prev)
      setErr(r.errMsg || r.msg || t('Failed to save setting.'))
      return
    }
    app.appSettings.sendRequiresPassword = checked
  }

  return (
    <SettingRow
      title={t('Require password to send')}
      desc={err ? <span className='err'>{err}</span> : t('Every send and swap will require your password.')}
    >
      <Toggle on={on} onChange={toggle} disabled={saving} label={t('Require password to send')} />
    </SettingRow>
  )
}

// ChangePasswordSheet changes the app password through /api/changeapppass. A
// successful change deauthorizes every other session; this session receives
// fresh credentials in the response cookies, so no re-login is needed.
function ChangePasswordSheet ({ onClose, onChanged }: { onClose: () => void, onChanged: () => void }) {
  const [cur, setCur] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const mismatch = confirm !== '' && next !== confirm
  const canSubmit = !submitting && cur !== '' && next !== '' && next === confirm

  const submit = async (e?: FormEvent) => {
    e?.preventDefault()
    if (!canSubmit) return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/changeapppass', { appPW: cur, newAppPW: next })
    setSubmitting(false)
    if (!r.ok) {
      setErr(r.errMsg || r.msg || t('Failed to change password.'))
      return
    }
    onChanged()
  }

  return (
    <Sheet
      title={t('Change password')}
      onClose={() => { if (!submitting) onClose() }}
      foot={
        <button type='button' className='btn' disabled={!canSubmit} onClick={() => submit()}>
          {submitting ? <><Spinner />{t('Submitting...')}</> : t('Change password')}
        </button>
      }
    >
      <form onSubmit={submit}>
        <label className='flbl'>{t('Current password')}</label>
        <PasswordField value={cur} onChange={setCur} label={t('Current password')} disabled={submitting} autoFocus />
        <label className='flbl mt16'>{t('New password')}</label>
        <PasswordField value={next} onChange={setNext} label={t('New password')} autoComplete='new-password' disabled={submitting} />
        <label className='flbl mt16'>{t('Confirm new password')}</label>
        <PasswordField value={confirm} onChange={setConfirm} label={t('Confirm new password')} autoComplete='new-password' disabled={submitting} bad={mismatch} />
        {mismatch && <div className='field-err'>{t('Passwords do not match.')}</div>}
        {err && <div className='err-box mt12' aria-live='polite'>{err}</div>}
      </form>
    </Sheet>
  )
}

// SeedSheet reveals the wallet mnemonic after an app-password check. The
// seed lives only in this component's state and is dropped when the sheet
// closes. There is deliberately no copy control: the clipboard is readable by
// other apps.
function SeedSheet ({ onClose }: { onClose: () => void }) {
  const [pw, setPW] = useState('')
  const [seed, setSeed] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [hidden, setHidden] = useState(false)

  const reveal = async (e?: FormEvent) => {
    e?.preventDefault()
    if (submitting || pw === '') return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/exportseed', { pass: pw })
    setSubmitting(false)
    if (r.ok && r.seed) {
      setSeed(r.seed)
      setPW('')
    } else {
      setErr(r.errMsg || r.msg || t('Failed to export seed.'))
    }
  }

  const words = seed ? seed.trim().split(/\s+/) : []

  return (
    <Sheet
      title={t('Wallet seed')}
      onClose={onClose}
      foot={seed
        ? <button type='button' className='btn secondary' onClick={onClose}>{t('Done')}</button>
        : (
          <button type='button' className='btn' disabled={pw === '' || submitting} onClick={() => reveal()}>
            {submitting ? <><Spinner />{t('Revealing...')}</> : t('Reveal seed')}
          </button>
          )}
    >
      <div className='callout caution strong'>
        <Icon name='key' />
        <span>{t('Anyone with these words controls this wallet\'s funds, on any device. Never photograph them or type them into another app.')}</span>
      </div>
      {seed
        ? (
          <>
            <SeedWords words={words} hidden={hidden} />
            <div className='center mt8'>
              <button type='button' className='btn quiet inline' onClick={() => setHidden(!hidden)}>
                <Icon name={hidden ? 'eyeopen' : 'eyeclosed'} />
                {hidden ? t('Show words') : t('Hide words')}
              </button>
            </div>
          </>
          )
        : (
          <form onSubmit={reveal} className='mt20'>
            <label className='flbl'>{t('App password')}</label>
            <PasswordField value={pw} onChange={v => { setPW(v); if (err) setErr('') }} label={t('App password')} disabled={submitting} autoFocus />
            {err && <div className='err-box mt12' aria-live='polite'>{err}</div>}
          </form>
          )}
    </Sheet>
  )
}

interface LegalDoc {
  version: string
  terms: string
  privacy: string
}

// LegalSheet shows the Terms of Use or the Privacy Policy.
function LegalSheet ({ which, onClose }: { which: 'terms' | 'privacy', onClose: () => void }) {
  const [doc, setDoc] = useState<LegalDoc | null>(null)
  const [err, setErr] = useState('')
  useEffect(() => {
    getJSON('/api/terms').then(r => {
      if (r.ok) setDoc(r as LegalDoc)
      else setErr(r.msg || t('Failed to load.'))
    })
  }, [])
  return (
    <Sheet title={which === 'terms' ? t('Terms of Use') : t('Privacy Policy')} onClose={onClose} tall>
      {err && <div className='err-box'>{err}</div>}
      {!doc && !err && <div className='empty'><Spinner large /></div>}
      {doc && (
        <div className='legal flat'>
          <LegalText markdown={which === 'terms' ? doc.terms : doc.privacy} />
          <div className='legal-ver'>{tvar('terms_version', { version: doc.version })}</div>
        </div>
      )}
    </Sheet>
  )
}

type SheetKind = 'password' | 'seed' | 'terms' | 'privacy' | null

// Settings is the settings tab: preferences, security, backup, diagnostics,
// and about.
export default function Settings () {
  const [sheet, setSheet] = useState<SheetKind>(null)
  const [pwChanged, flashPWChanged] = useFlash(2500)
  const b = bridge()

  // Inside a shell the log goes to the native share sheet; in a plain
  // browser it downloads.
  const shareLog = () => {
    if (b) b.shareAppLog?.()
    else window.location.href = '/api/exportapplog'
  }

  const version = app.appVersion

  return (
    <div className='scroll'>
      <div className='page'>
        <h1 className='page-title'>{t('Settings')}</h1>

        <h2 className='lbl sec'>{t('Preferences')}</h2>
        <div className='list'>
          <LanguageRow />
          <ThemeRow />
        </div>

        <h2 className='lbl sec'>{t('Security')}</h2>
        <div className='list'>
          {b && <BiometricRows b={b} />}
          <SendPasswordRow />
          <SettingRow title={t('Change password')} onClick={() => setSheet('password')}><Chevron /></SettingRow>
        </div>

        <h2 className='lbl sec'>{t('Backup')}</h2>
        <div className='list'>
          <SettingRow
            title={t('Wallet seed')}
            desc={t('View your recovery seed.')}
            icon={<Icon name='key' className='gold fs17' />}
            onClick={() => setSheet('seed')}
          >
            <Chevron />
          </SettingRow>
        </div>

        {canExportAppLog() && (
          <>
            <h2 className='lbl sec'>{t('Diagnostics')}</h2>
            <div className='list'>
              <SettingRow title={t('Application log')} onClick={shareLog}>
                <span className='accent demi fs14'>{b ? t('Share') : t('Download')}</span>
              </SettingRow>
            </div>
          </>
        )}

        <h2 className='lbl sec'>{t('About')}</h2>
        <div className='list'>
          {version && <SettingRow title={t('Version')}><span className='grey'>{version}</span></SettingRow>}
          <SettingRow title={t('Terms of Use')} onClick={() => setSheet('terms')}><Chevron /></SettingRow>
          <SettingRow title={t('Privacy Policy')} onClick={() => setSheet('privacy')}><Chevron /></SettingRow>
        </div>
      </div>

      {sheet === 'password' && <ChangePasswordSheet onClose={() => setSheet(null)} onChanged={() => { setSheet(null); flashPWChanged() }} />}
      {sheet === 'seed' && <SeedSheet onClose={() => setSheet(null)} />}
      {(sheet === 'terms' || sheet === 'privacy') && <LegalSheet which={sheet} onClose={() => setSheet(null)} />}
      {pwChanged && <Toast>{t('Password changed.')}</Toast>}
    </div>
  )
}
