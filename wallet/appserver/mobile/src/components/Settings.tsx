import { useState, useEffect, FormEvent, ChangeEvent } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import { postJSON } from '@bossw/shared/http'

// bossw is the native bridge the Android shell injects (window.bossw). It is
// absent on desktop and on the web, so every use is guarded. No method takes or
// returns the app password: enrollBiometrics captures it in a native field, and
// unlock releases it natively, so the secret never reaches this JS.
interface BosswBridge {
  biometricAvailable(): boolean
  biometricEnrolled(): boolean
  enrollBiometrics(): void
  disableBiometrics(): void
  // Idle-lock thresholds in minutes: gateMin (0 = require unlock immediately),
  // evictMin (0 = never evict). getLockConfig returns JSON {gateMin, evictMin}.
  getLockConfig(): string
  setLockConfig(gateMin: number, evictMin: number): void
}
declare global {
  interface Window { bossw?: BosswBridge }
}

// SendSettings mirrors the desktop app-level send preference: whether sending
// requires the app password. The value is written back to app.appSettings so
// the Send flow picks it up without a refetch.
function SendSettings () {
  const [requirePW, setRequirePW] = useState(app.appSettings.sendRequiresPassword)
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')

  const toggle = async (e: ChangeEvent<HTMLInputElement>) => {
    if (saving) return
    const checked = e.target.checked
    const prev = requirePW
    setSaving(true)
    setErr('')
    setRequirePW(checked) // optimistic
    const r = await postJSON('/api/setsendrequirespassword', { require: checked })
    setSaving(false)
    if (!r.ok) {
      setRequirePW(prev)
      setErr(r.errMsg || r.msg || t('Failed to save setting.'))
      return
    }
    app.appSettings.sendRequiresPassword = checked
  }

  return (
    <div className='mb-card'>
      <h2>{t('Send Settings')}</h2>
      <label className='mb-check-row'>
        <input type='checkbox' checked={requirePW} onChange={toggle} disabled={saving} />
        <span>{t('Require password to send')}</span>
      </label>
      {err && <div className='mb-err'>{err}</div>}
    </div>
  )
}

// SeedExport reveals the wallet mnemonic after an app-password check. The seed
// is kept only in local state and cleared on hide.
function SeedExport () {
  const [pw, setPW] = useState('')
  const [seed, setSeed] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [copied, setCopied] = useState(false)

  const reveal = async (e: FormEvent) => {
    e.preventDefault()
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

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(seed)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch { /* clipboard unavailable */ }
  }

  if (seed) {
    return (
      <div className='mb-card'>
        <h2>{t('Backup Wallet Seed')}</h2>
        <p className='mb-err'>{t('export_seed_warning')}</p>
        <div className='mb-seed'>{seed}</div>
        <button className='mb-btn' type='button' onClick={copy}>{copied ? t('Copied!') : t('Copy Seed')}</button>
        <button className='mb-btn mb-btn-secondary' type='button' onClick={() => setSeed('')}>{t('Hide Seed')}</button>
      </div>
    )
  }

  return (
    <form className='mb-card' onSubmit={reveal}>
      <h2>{t('Backup Wallet Seed')}</h2>
      <p className='mb-muted'>{t('export_seed_warning')}</p>
      <input
        className='mb-input'
        type='password'
        autoComplete='current-password'
        placeholder={t('password')}
        value={pw}
        onChange={(e) => { setPW(e.target.value); if (err) setErr('') }}
        disabled={submitting}
      />
      <div className='mb-err' aria-live='polite'>{err}</div>
      <button className='mb-btn' type='submit' disabled={pw === '' || submitting}>
        {submitting ? t('Revealing...') : t('Reveal Seed')}
      </button>
    </form>
  )
}

// BiometricSettings offers the native biometric-unlock opt-in. It renders only
// on the mobile shell (window.bossw present) and only when the device has usable
// biometric hardware. Toggling calls into the native bridge; the actual enroll
// or disable completes natively (a biometric prompt), and the shell fires a
// 'bossw-biometric-changed' event when done, which we use to re-read the state.
function BiometricSettings () {
  const bridge = typeof window !== 'undefined' ? window.bossw : undefined
  const [available, setAvailable] = useState(() => {
    try { return !!bridge && bridge.biometricAvailable() } catch { return false }
  })
  const [enrolled, setEnrolled] = useState(() => {
    try { return !!bridge && bridge.biometricEnrolled() } catch { return false }
  })

  useEffect(() => {
    if (!bridge) return
    const refresh = () => {
      // Only touch the native bridge when the document is actually visible. The
      // visibilitychange handler fires on hide too, and re-reading state then
      // would burn synchronous JNI round-trips on a lifecycle hot path for a
      // view no one is looking at.
      if (document.visibilityState !== 'visible') return
      try {
        setAvailable(bridge.biometricAvailable())
        setEnrolled(bridge.biometricEnrolled())
      } catch { /* bridge gone */ }
    }
    window.addEventListener('bossw-biometric-changed', refresh)
    // The user may enroll a fingerprint in system settings while this pane stays
    // mounted; re-check availability when the app regains focus so the toggle
    // appears without a full reload.
    document.addEventListener('visibilitychange', refresh)
    return () => {
      window.removeEventListener('bossw-biometric-changed', refresh)
      document.removeEventListener('visibilitychange', refresh)
    }
  }, [bridge])

  if (!bridge) return null

  // Don't flip optimistically: enrollment is an async native prompt the user can
  // cancel. The checkbox reflects the real stored state, updated via the event.
  const toggle = (e: ChangeEvent<HTMLInputElement>) => {
    if (e.target.checked) bridge.enrollBiometrics()
    else bridge.disableBiometrics()
  }

  return (
    <div className='mb-card'>
      <h2>{t('Security')}</h2>
      {available && (
        <label className='mb-check-row'>
          <input type='checkbox' checked={enrolled} onChange={toggle} />
          <span>{t('Unlock with biometrics')}</span>
        </label>
      )}
      <LockTiming bridge={bridge} available={available} />
    </div>
  )
}

// Minute values offered for each threshold. 0 is a sentinel: for the gate it
// means "require unlock immediately", for eviction it means "never".
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

// LockTiming lets the user tune the two idle thresholds. Reads the current
// values from the native bridge and writes both back on any change. The gate row
// only shows when the device can actually gate (biometrics available); without
// them the gate has no mechanism and only eviction applies.
function LockTiming ({ bridge, available }: { bridge: BosswBridge, available: boolean }) {
  const [gateMin, setGateMin] = useState(10)
  const [evictMin, setEvictMin] = useState(60)

  useEffect(() => {
    try {
      const cfg = JSON.parse(bridge.getLockConfig())
      if (typeof cfg.gateMin === 'number') setGateMin(cfg.gateMin)
      if (typeof cfg.evictMin === 'number') setEvictMin(cfg.evictMin)
    } catch { /* older shell without the config bridge */ }
  }, [bridge])

  // Native owns persistence and is the single authority on the stored pair: it
  // clamps the gate to sit strictly below eviction so the band never goes empty.
  // Write through the bridge, then read back what it actually stored rather than
  // trusting our optimistic values, so the UI can never drift from the truth even
  // if the native clamp adjusts the pair.
  const save = (gate: number, evict: number) => {
    bridge.setLockConfig(gate, evict)
    try {
      const cfg = JSON.parse(bridge.getLockConfig())
      setGateMin(typeof cfg.gateMin === 'number' ? cfg.gateMin : gate)
      setEvictMin(typeof cfg.evictMin === 'number' ? cfg.evictMin : evict)
    } catch {
      setGateMin(gate)
      setEvictMin(evict)
    }
  }

  // The gate is the cheaper tier and must sit strictly below eviction, or its
  // band [gate, evict) is empty and it would never fire. Offer only gate values
  // below the chosen eviction (Never, 0, is always above), and when eviction
  // drops to at or below the current gate, pull the gate down with it. Native
  // clamps regardless; this just keeps the offered choices coherent.
  const gateChoices = GATE_OPTIONS.filter((m) => evictMin === 0 || m < evictMin)
  // Always include the persisted gate so the controlled select can render it even
  // if the native clamp landed on a value not in GATE_OPTIONS.
  const gateOptions = gateChoices.includes(gateMin)
    ? gateChoices
    : [...gateChoices, gateMin].sort((a, b) => a - b)
  const onEvictChange = (evict: number) => {
    let gate = gateMin
    if (evict !== 0 && gate >= evict) {
      const below = GATE_OPTIONS.filter((m) => m < evict)
      gate = below.length ? Math.max(...below) : 0
    }
    save(gate, evict)
  }

  return (
    <>
      {available && (
        <label className='mb-select-row'>
          <span>{t('Lock after')}</span>
          <select value={gateMin} onChange={(e) => save(Number(e.target.value), evictMin)}>
            {gateOptions.map((m) => <option key={m} value={m}>{gateLabel(m)}</option>)}
          </select>
        </label>
      )}
      <label className='mb-select-row'>
        <span>{t('Evict keys after')}</span>
        <select value={evictMin} onChange={(e) => onEvictChange(Number(e.target.value))}>
          {EVICT_OPTIONS.map((m) => <option key={m} value={m}>{evictLabel(m)}</option>)}
        </select>
      </label>
    </>
  )
}

export default function Settings () {
  return (
    <div className='mb-settings'>
      <BiometricSettings />
      <SendSettings />
      <SeedExport />
    </div>
  )
}
