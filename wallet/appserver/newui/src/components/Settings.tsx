import { useState, FormEvent } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import PasswordInput from './PasswordInput'
import Spinner from './Spinner'
import Checkbox from './Checkbox'
import { postJSON } from '@caribou/shared/http'

/*
 * SendSettings exposes app-level send preferences. Currently the only toggle
 * is whether sending funds requires the app password; the value is mirrored
 * into app.appSettings so the Send dialog picks it up without a refetch.
 */
function SendSettings () {
  const [requirePW, setRequirePW] = useState(app.appSettings.sendRequiresPassword)
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')

  const toggle = async (checked: boolean) => {
    if (saving) return
    setSaving(true)
    setErr('')
    const prev = requirePW
    setRequirePW(checked) // optimistic
    const r = await postJSON('/api/setsendrequirespassword', { require: checked })
    setSaving(false)
    if (!r.ok) {
      setRequirePW(prev) // revert on failure
      setErr(r.errMsg || r.msg || t('Failed to save setting.'))
      return
    }
    app.appSettings.sendRequiresPassword = checked
  }

  return (
    <div className="form-width-300">
      <div className="fs18 pb-2 text-center">{t('Send Settings')}</div>
      <label className="checkbox-row">
        <Checkbox checked={requirePW} onChange={toggle} disabled={saving} ariaLabel={t('Require password to send')} />
        <span className="flex-grow-1 fs14">{t('Require password to send')}</span>
        {saving && <Spinner size={14} />}
      </label>
      {err && <div className="errcolor fs12 text-center pt-2">{err}</div>}
    </div>
  )
}

/*
 * SeedExport is a password-gated component that calls /api/exportseed and
 * displays the resulting mnemonic. The user must supply their app password;
 * the seed is then shown inline and can be copied. The reveal state is kept
 * local — navigating away or hiding the seed clears it from memory.
 */
function SeedExport () {
  const [pw, setPW] = useState('')
  const [err, setErr] = useState('')
  const [seed, setSeed] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [copied, setCopied] = useState(false)

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

  const hideSeed = () => {
    setSeed('')
    setCopied(false)
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

  if (seed) {
    return (
      <div className="form-width-300">
        <div className="fs18 pb-2 text-center">{t('Backup Wallet Seed')}</div>
        <div className="fs13 pb-3 text-center errcolor">{t('export_seed_warning')}</div>
        <div className="p-2 fs14 form-input-bg mono br5 mb-2 break-all seed-phrase">
          {seed}
        </div>
        <div className="fs12 pb-3 text-center o-70">
          {t('export_seed_copy_reminder')}
        </div>
        <div className="d-flex flex-column">
          <button type="button" className="feature w-100" onClick={copySeed}>
            {copied ? t('Copied!') : t('Copy Seed')}
          </button>
          <button type="button" className="w-100 mt-2" onClick={hideSeed}>
            {t('Hide Seed')}
          </button>
        </div>
      </div>
    )
  }

  return (
    <form className="form-width-300" onSubmit={onSubmit}>
      <div className="fs18 pb-2 text-center">{t('Backup Wallet Seed')}</div>
      <div className="fs13 pb-3 text-center o-70">
        {t('export_seed_warning')}
      </div>
      <div className="mb-3">
        <PasswordInput
          value={pw}
          onChange={pwChanged}
          disabled={submitting}
          autoFocus
        />
      </div>
      <div aria-live="polite" className="pb-2">
        {err && (
          <div key={err} className="form-err flex-center errcolor text-center fs13">
            {err}
          </div>
        )}
      </div>
      <button type="submit" className="feature w-100" disabled={!canSubmit}>
        {submitting
          ? (
            <span className="d-inline-flex align-items-center justify-content-center">
              <Spinner className="me-2" />
              <span>{t('Revealing...')}</span>
            </span>
            )
          : t('Reveal Seed')}
      </button>
    </form>
  )
}

export default function Settings () {
  return (
    <div className="fill-abs d-flex flex-column align-items-center pt-5">
      <SendSettings />
      <div className="py-4" />
      <SeedExport />
    </div>
  )
}
