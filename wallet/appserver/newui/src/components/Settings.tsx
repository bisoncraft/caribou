import { useState, FormEvent, ReactNode } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import PasswordInput from './PasswordInput'
import Spinner from './Spinner'
import Toggle from './Toggle'
import LocaleSelector from './LocaleSelector'
import { postJSON } from '@caribou/shared/http'
import { Theme, themes, storedTheme, setTheme } from '../theme'

const themeLabels: Record<Theme, string> = { warm: 'Warm', cool: 'Cool', light: 'Light' }

/*
 * SettingsRow is one row of a settings card: a title and grey description on
 * the left, an optional control on the right, and an optional expansion
 * rendered full-width beneath the row.
 */
function SettingsRow ({ title, desc, children, expansion }: {
  title: string
  desc: string
  children?: ReactNode
  expansion?: ReactNode
}) {
  return (
    <div className="settings-row p-3">
      <div className="d-flex align-items-center">
        <div className="flex-grow-1 pe-3">
          <div className="fs14 demi">{title}</div>
          <div className="fs13 grey pt-1">{desc}</div>
        </div>
        {children}
      </div>
      {expansion}
    </div>
  )
}

/*
 * ToggleRow is a SettingsRow whose control is a Toggle persisted through an
 * API endpoint. The value is applied optimistically and reverted on failure;
 * onSaved mirrors the confirmed value into app.appSettings.
 */
function ToggleRow ({ title, desc, initial, save }: {
  title: string
  desc: string
  initial: boolean
  save: (on: boolean) => Promise<string>
}) {
  const [on, setOn] = useState(initial)
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')

  const toggle = async (checked: boolean) => {
    if (saving) return
    setSaving(true)
    setErr('')
    const prev = on
    setOn(checked) // optimistic
    const errMsg = await save(checked)
    setSaving(false)
    if (errMsg !== '') {
      setOn(prev) // revert on failure
      setErr(errMsg)
    }
  }

  return (
    <SettingsRow
      title={title}
      desc={desc}
      expansion={err ? <div className="errcolor fs12 pt-2">{err}</div> : null}
    >
      {saving && <Spinner size={14} className="me-2" />}
      <Toggle checked={on} onChange={toggle} disabled={saving} ariaLabel={title} />
    </SettingsRow>
  )
}

/*
 * ThemeRow selects among the color themes. Each option button carries its own
 * data-theme attribute, so it renders as a live preview of the palette it
 * selects (see the palette emission in scss/vars.scss). The choice applies
 * immediately and persists to localStorage.
 */
function ThemeRow () {
  const [theme, setPicked] = useState<Theme>(storedTheme())

  const pick = (th: Theme) => {
    setTheme(th)
    setPicked(th)
  }

  return (
    <SettingsRow title={t('Theme')} desc={t('theme_desc')}>
      <div className="d-flex theme-picker">
        {themes.map(th => (
          <button
            key={th}
            type="button"
            aria-pressed={th === theme}
            data-theme={th}
            className="theme-swatch d-flex flex-column align-items-center"
            onClick={() => pick(th)}
          >
            <span className="d-flex mb-1">
              <span className="theme-swatch-dot fill-accent me-1" />
              <span className="theme-swatch-dot fill-gold" />
            </span>
            <span className="fs13">{t(themeLabels[th])}</span>
          </button>
        ))}
      </div>
    </SettingsRow>
  )
}

async function saveSendRequiresPW (on: boolean): Promise<string> {
  const r = await postJSON('/api/setsendrequirespassword', { require: on })
  if (!r.ok) return r.errMsg || r.msg || t('Failed to save setting.')
  app.appSettings.sendRequiresPassword = on
  return ''
}

/*
 * PasswordRow changes the app password through /api/changeapppass. The form
 * expands inline beneath the row. A successful change deauthorizes every
 * other session; this session receives fresh credentials in the response
 * cookies, so no re-login is needed here.
 */
function PasswordRow () {
  const [open, setOpen] = useState(false)
  const [currentPW, setCurrentPW] = useState('')
  const [newPW, setNewPW] = useState('')
  const [confirmPW, setConfirmPW] = useState('')
  const [err, setErr] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [changed, setChanged] = useState(false)

  const reset = () => {
    setCurrentPW('')
    setNewPW('')
    setConfirmPW('')
    setErr('')
  }

  const toggleOpen = () => {
    reset()
    setOpen(!open)
  }

  const canSubmit = !submitting && currentPW !== '' && newPW !== '' && confirmPW !== ''

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault()
    if (!canSubmit) return
    if (newPW !== confirmPW) {
      setErr(t('Passwords do not match.'))
      return
    }
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/changeapppass', { appPW: currentPW, newAppPW: newPW })
    setSubmitting(false)
    if (!r.ok) {
      setErr(r.errMsg || r.msg || t('Failed to change password.'))
      return
    }
    reset()
    setOpen(false)
    setChanged(true)
    setTimeout(() => setChanged(false), 2500)
  }

  const expansion = open
    ? (
      <form className="pt-3" onSubmit={onSubmit}>
        <div className="mb-2">
          <PasswordInput
            value={currentPW}
            onChange={e => setCurrentPW(e.target.value)}
            placeholder={t('current password')}
            disabled={submitting}
            autoFocus
          />
        </div>
        <div className="mb-2">
          <PasswordInput
            value={newPW}
            onChange={e => setNewPW(e.target.value)}
            placeholder={t('new password')}
            disabled={submitting}
          />
        </div>
        <div className="mb-2">
          <PasswordInput
            value={confirmPW}
            onChange={e => setConfirmPW(e.target.value)}
            placeholder={t('new password again')}
            disabled={submitting}
          />
        </div>
        <div aria-live="polite">
          {err && <div className="errcolor fs13 pb-2">{err}</div>}
        </div>
        <button type="submit" className="feature w-100" disabled={!canSubmit}>
          {submitting
            ? (
              <span className="d-inline-flex align-items-center justify-content-center">
                <Spinner className="me-2" />
                <span>{t('Submitting...')}</span>
              </span>
              )
            : t('Change password')}
        </button>
      </form>
      )
    : null

  return (
    <SettingsRow title={t('Password')} desc={t('change_password_desc')} expansion={expansion}>
      {changed && <span className="fs13 grey me-2">{t('Password changed.')}</span>}
      <button type="button" onClick={toggleOpen}>
        {open ? t('Cancel') : t('Change')}
      </button>
    </SettingsRow>
  )
}

/*
 * SeedRow is a password-gated seed backup row. Revealing calls
 * /api/exportseed and displays the resulting mnemonic inline. The reveal
 * state is kept local — collapsing the row clears the seed from memory.
 */
function SeedRow () {
  const [open, setOpen] = useState(false)
  const [pw, setPW] = useState('')
  const [err, setErr] = useState('')
  const [seed, setSeed] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [copied, setCopied] = useState(false)

  const toggleOpen = () => {
    setPW('')
    setErr('')
    setSeed('')
    setCopied(false)
    setOpen(!open)
  }

  const pwChanged = (e: React.ChangeEvent<HTMLInputElement>) => {
    setPW(e.target.value)
    if (err) setErr('')
  }

  const revealSeed = async () => {
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

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    revealSeed()
  }

  const copySeed = async () => {
    try {
      await navigator.clipboard.writeText(seed)
      setCopied(true)
      setTimeout(() => setCopied(false), 2500)
    } catch {
      // clipboard API unavailable — silently ignore
    }
  }

  const canSubmit = pw !== '' && !submitting

  const expansion = !open
    ? null
    : seed
      ? (
        <div className="pt-3">
          <div className="p-2 fs14 form-input-bg mono br5 mb-2 break-all seed-phrase">
            {seed}
          </div>
          <div className="fs12 pb-2 o-70">
            {t('export_seed_copy_reminder')}
          </div>
          <button type="button" className="feature w-100" onClick={copySeed}>
            {copied ? t('Copied!') : t('Copy seed')}
          </button>
        </div>
        )
      : (
        <form className="pt-3" onSubmit={onSubmit}>
          <div className="mb-2">
            <PasswordInput
              value={pw}
              onChange={pwChanged}
              disabled={submitting}
              autoFocus
            />
          </div>
          <div aria-live="polite">
            {err && <div className="errcolor fs13 pb-2">{err}</div>}
          </div>
          <button type="submit" className="feature w-100" disabled={!canSubmit}>
            {submitting
              ? (
                <span className="d-inline-flex align-items-center justify-content-center">
                  <Spinner className="me-2" />
                  <span>{t('Revealing...')}</span>
                </span>
                )
              : t('Reveal seed')}
          </button>
        </form>
        )

  return (
    <SettingsRow title={t('Wallet seed')} desc={t('export_seed_warning')} expansion={expansion}>
      <button type="button" onClick={toggleOpen}>
        {open ? (seed ? t('Hide seed') : t('Cancel')) : t('Reveal seed')}
      </button>
    </SettingsRow>
  )
}

/*
 * LogRow downloads the zipped application log through /api/exportapplog. The
 * endpoint responds with a content-disposition attachment, so assigning
 * location triggers a download without leaving the page.
 */
function LogRow () {
  return (
    <SettingsRow title={t('Application log')} desc={t('export_log_desc')}>
      <button type="button" onClick={() => { window.location.href = '/api/exportapplog' }}>
        {t('Export')}
      </button>
    </SettingsRow>
  )
}

export default function Settings () {
  return (
    <div className="fill-abs overflow-y-auto">
      <div className="settings-inner">
        <div className="section-label">{t('Preferences')}</div>
        <div className="settings-card">
          <SettingsRow title={t('Language')} desc={t('language_desc')}>
            <LocaleSelector />
          </SettingsRow>
          <ThemeRow />
        </div>
        <div className="section-label">{t('Security')}</div>
        <div className="settings-card">
          <ToggleRow
            title={t('Require password to send')}
            desc={t('send_requires_password_desc')}
            initial={app.appSettings.sendRequiresPassword}
            save={saveSendRequiresPW}
          />
          <PasswordRow />
        </div>
        <div className="section-label">{t('Backup')}</div>
        <div className="settings-card">
          <SeedRow />
        </div>
        <div className="section-label">{t('Diagnostics')}</div>
        <div className="settings-card">
          <LogRow />
        </div>
      </div>
    </div>
  )
}
