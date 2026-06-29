import { useState, FormEvent, ChangeEvent } from 'react'
import t from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import { postJSON } from '@bossw/shared/http'

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

export default function Settings () {
  return (
    <div className='mb-settings'>
      <SendSettings />
      <SeedExport />
    </div>
  )
}
