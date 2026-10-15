import { useEffect, useState } from 'react'
import t from './intl'
import { getJSON, postJSON } from './http'

export interface LegalDoc {
  version: string
  terms: string
  privacy: string
}

// useTermsAcceptance holds the state and handlers shared by the desktop and
// mobile TermsScreen: loading the current Terms of Use and Privacy Policy and
// recording the user's acceptance of the version they were shown. Each bundle
// keeps only its rendering.
export function useTermsAcceptance (onAccepted: () => void) {
  const [doc, setDoc] = useState<LegalDoc | null>(null)
  const [agreed, setAgreed] = useState(false)
  const [declined, setDeclined] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState('')

  const load = () => {
    setErr('')
    getJSON('/api/terms').then(r => {
      if (r.ok) setDoc(r)
      else setErr(r.errMsg || r.msg || t('Could not load the terms.'))
    })
  }

  useEffect(load, [])

  const accept = async () => {
    if (!doc || !agreed || submitting) return
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/acceptterms', { version: doc.version })
    setSubmitting(false)
    if (r.ok) onAccepted()
    else setErr(r.errMsg || r.msg || t('Could not save your acceptance.'))
  }

  const canAccept = doc !== null && agreed && !submitting

  return { doc, agreed, setAgreed, declined, setDeclined, submitting, err, load, accept, canAccept }
}
