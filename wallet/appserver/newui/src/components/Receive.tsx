import { useEffect, useRef, useState, FormEvent } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { postJSON } from '@caribou/shared/http'
import { TickerAsset, AssetVariant, networkAsset, isVariantActive } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import Spinner from './Spinner'

// activeVariants returns the variants of a ticker whose underlying wallet
// is created and can therefore receive funds. For a native variant that
// means its own wallet exists; for a token it means the parent (network)
// wallet exists, because tokens share the network's deposit address.
function activeVariants (ta: TickerAsset): AssetVariant[] {
  return ta.variants.filter(isVariantActive)
}

// allVariants returns all variants, active or not.
function allVariants (ta: TickerAsset): AssetVariant[] {
  return ta.variants
}

function networkTicker (v: AssetVariant): string {
  const n = networkAsset(v)
  return n ? n.unitInfo.conventional.unit : ''
}

const noSubmit = (e: FormEvent) => e.preventDefault()

interface ReceiveProps {
  // Optional preselected ticker (e.g. when launched from a portfolio row's
  // Receive icon). When set, the asset picker is skipped on mount.
  ticker?: string
}

// preselected resolves an optional ticker prop to a TickerAsset and, when
// the ticker has exactly one variant, that variant — so we can skip the
// asset (and where unambiguous, the network) picker on mount. Returns
// nulls if the ticker is missing or has no active variant.
function preselected (ticker?: string): { ta: TickerAsset | null, v: AssetVariant | null } {
  if (!ticker) return { ta: null, v: null }
  const ta = app.tickerMap?.[ticker]
  if (!ta || activeVariants(ta).length === 0) return { ta: null, v: null }
  // Skip step 2 only for single-variant tickers (same rule as pickAsset).
  return { ta, v: ta.variants.length === 1 ? ta.variants[0] : null }
}

export default function Receive ({ ticker }: ReceiveProps) {
  const initial = preselected(ticker)
  const [ta, setTa] = useState<TickerAsset | null>(initial.ta)
  const [variant, setVariant] = useState<AssetVariant | null>(initial.v)
  const [address, setAddress] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const [copied, setCopied] = useState(false)
  // Per-variant activation state for inline Activate in step 2.
  const [activating, setActivating] = useState<number | null>(null)
  const [activateErr, setActivateErr] = useState<Record<number, string>>({})
  // Monotonic request ID so stale /api/depositaddress responses can't
  // overwrite a newer selection.
  const reqIDRef = useRef(0)

  const close = () => app.setForm({ form: '', data: {} })

  const fetchAddress = async (v: AssetVariant) => {
    const network = networkAsset(v)
    if (!network) {
      setErr(t('Network wallet not available.'))
      return
    }
    const id = ++reqIDRef.current
    setLoading(true); setErr(''); setAddress('')
    const r = await postJSON('/api/depositaddress', { assetID: network.id })
    if (id !== reqIDRef.current) return // a newer request superseded us
    setLoading(false)
    if (!r.ok) {
      setErr(r.msg || t('Failed to fetch address'))
      return
    }
    setAddress(r.address)
  }

  const pickAsset = async (chosen: TickerAsset) => {
    setTa(chosen)
    setActivating(null)
    setActivateErr({})
    // Only skip the network picker for single-variant tickers (e.g. BTC,
    // native ETH). Multi-network tokens always show step 2 so the user
    // can see other networks and activate them inline if they want.
    if (chosen.variants.length === 1) {
      const v = chosen.variants[0]
      setVariant(v)
      await fetchAddress(v)
    }
  }

  // When the component mounts with a single-variant preselected ticker,
  // initial state already puts us at step 3 — but the address still
  // needs to be fetched. Empty deps: this is a one-shot mount effect.
  useEffect(() => {
    if (initial.v) fetchAddress(initial.v)
  }, [])

  const pickVariant = async (v: AssetVariant) => {
    setVariant(v)
    await fetchAddress(v)
  }

  const resetForBack = () => {
    reqIDRef.current++ // invalidate any in-flight request
    setAddress(''); setErr(''); setCopied(false); setLoading(false)
  }

  const backToAssets = () => {
    setTa(null); setVariant(null); setActivating(null); setActivateErr({}); resetForBack()
  }

  const backToNetwork = () => {
    // Step 2 is reachable for any multi-variant ticker (active or not),
    // since inactive variants render as inline-activate rows. Single-variant
    // tickers skip step 2 entirely, so Back goes to step 1 there.
    if (ta && ta.variants.length > 1) {
      setVariant(null); resetForBack()
    } else {
      backToAssets()
    }
  }

  const copy = async () => {
    if (!address) return
    await navigator.clipboard.writeText(address)
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }

  // Inline activate: fires a single-network activation from the network picker.
  const inlineActivate = async (v: AssetVariant) => {
    const netAsset = networkAsset(v)
    const walletDef = netAsset?.info?.availablewallets?.[0]
    if (!walletDef) {
      setActivateErr(prev => ({ ...prev, [v.assetID]: t('No wallet definition available') }))
      return
    }
    setActivating(v.assetID)
    setActivateErr(prev => { const next = { ...prev }; delete next[v.assetID]; return next })
    const r = await postJSON('/api/newwallet', {
      assetID: netAsset.id,
      walletType: walletDef.type,
      config: {}
    })
    setActivating(null)
    if (!r.ok) {
      setActivateErr(prev => ({
        ...prev,
        [v.assetID]: r.errMsg || r.msg || t('Failed to activate')
      }))
      return
    }
    // Activation succeeded: proceed to fetch address for this variant.
    await pickVariant(v)
  }

  // Step 1: asset picker
  if (!ta) {
    const list = (app.tickerList ?? []).filter(item => activeVariants(item).length > 0)
    if (list.length === 0) {
      return (
        <form className="modal form-width-300" onSubmit={noSubmit}>
          <header><span>{t('Receive')}</span></header>
          <div className="px-3 pb-3 grey text-center">{t('Activate an asset first to receive funds.')}</div>
          <div className="flex-stretch-column p-3">
            <button type="button" onClick={close}>{t('Close')}</button>
          </div>
        </form>
      )
    }
    return (
      <form className="modal form-width-300" onSubmit={noSubmit}>
        <header><span>{t('Receive')}</span></header>
        <div className="pb-2">
          {list.map(item => (
            <div key={item.ticker} className="d-flex align-items-center px-3 py-2 hoverbg pointer" onClick={() => pickAsset(item)}>
              <CoinLogo ticker={item.ticker} className="mini-icon me-2" />
              <span className="flex-grow-1">{item.name}</span>
              <span className="grey fs12">{item.ticker}</span>
            </div>
          ))}
        </div>
        <div className="flex-stretch-column p-3">
          <button type="button" onClick={close}>{t('Cancel')}</button>
        </div>
      </form>
    )
  }

  // Step 2: network picker for multi-network tokens — shows active + inactive variants.
  if (!variant) {
    // Stable partition: active networks first, then inactive. Preserves the
    // original variant ordering within each group.
    const vs = [...allVariants(ta)].sort((a, b) => {
      const aActive = isVariantActive(a) ? 0 : 1
      const bActive = isVariantActive(b) ? 0 : 1
      return aActive - bActive
    })
    const anyActivating = activating !== null
    return (
      <form className="modal form-width-300" onSubmit={noSubmit}>
        <header>
          <CoinLogo ticker={ta.ticker} className="mini-icon me-2" />
          <span>{tvar('Receive {{ name }}', { name: ta.name })}</span>
        </header>
        <div className="px-3 pb-2 fs14 grey text-center">{t('Choose the network to receive on.')}</div>
        <div className="pb-2">
          {vs.map(v => {
            const nt = networkTicker(v)
            const nName = networkAsset(v)?.name ?? ''
            const active = isVariantActive(v)
            const isThisActivating = activating === v.assetID
            const variantErr = activateErr[v.assetID]
            if (active) {
              return (
                <div
                  key={v.assetID}
                  className="d-flex align-items-center px-3 py-2 hoverbg pointer"
                  onClick={() => pickVariant(v)}
                >
                  <CoinLogo ticker={nt} className="mini-icon me-2" />
                  <span className="flex-grow-1">{nName}</span>
                  <span className="grey fs12">{nt}</span>
                </div>
              )
            }
            // Inactive network variant row.
            return (
              <div key={v.assetID} className="receive-inactive-net px-3 py-2">
                <div className="d-flex align-items-center">
                  <CoinLogo ticker={nt} className="mini-icon me-2 receive-inactive-logo" />
                  <span className="flex-grow-1 grey">{nName}</span>
                  <button
                    type="button"
                    className="receive-activate-btn fs12"
                    onClick={() => inlineActivate(v)}
                    disabled={anyActivating}
                  >
                    {isThisActivating
                      ? <><Spinner size={12} className="me-1" />{t('Activating...')}</>
                      : t('Activate')}
                  </button>
                </div>
                {variantErr && (
                  <div className="errcolor fs12 mt-1">{variantErr}</div>
                )}
              </div>
            )
          })}
        </div>
        <div className="flex-stretch-column p-3">
          <button type="button" onClick={backToAssets}>{t('Back')}</button>
        </div>
      </form>
    )
  }

  // Step 3: address display
  const netName = networkAsset(variant)?.name ?? ''
  const netTicker = networkTicker(variant)
  return (
    <form className="modal form-width-300" onSubmit={noSubmit}>
      <header>
        <CoinLogo ticker={ta.ticker} className="mini-icon me-2" />
        <span>{tvar('Receive {{ name }}', { name: ta.name })}</span>
      </header>
      <div className="px-3 pb-2 fs14 d-flex align-items-center justify-content-center grey">
        <span className="me-2">{t('Network:')}</span>
        <CoinLogo ticker={netTicker} className="mini-icon me-1" />
        <span>{netName}</span>
      </div>
      <div className="px-3 pb-2 fs14 mono receive-address text-center">
        {loading ? t('Loading...') : address}
      </div>
      <div className={`flex-center errcolor px-3 pb-2 ${err ? '' : 'd-none'}`}>{err}</div>
      <div className="flex-stretch-column p-3">
        <button type="button" className="feature" onClick={copy} disabled={!address}>
          {copied ? t('Copied!') : t('Copy Address')}
        </button>
        <button type="button" className="mt-2" onClick={backToNetwork}>{t('Back')}</button>
        <button type="button" className="mt-2" onClick={close}>{t('Close')}</button>
      </div>
    </form>
  )
}
