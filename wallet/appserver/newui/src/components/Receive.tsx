import { useRef, useState, FormEvent } from 'react'
import t, { tvar } from '../js/intl'
import app from '../js/application'
import { postJSON } from '../js/http'
import { TickerAsset, AssetVariant } from '../js/assets'
import CoinLogo from './CoinLogo'

// activeVariants returns the variants of a ticker whose underlying wallet
// is created and can therefore receive funds. For a native variant that
// means its own wallet exists; for a token it means the parent (network)
// wallet exists, because tokens share the network's deposit address.
function activeVariants (ta: TickerAsset): AssetVariant[] {
  return ta.variants.filter(v => {
    const networkID = v.token ? v.token.networkID : v.assetID
    return Boolean(app.user.assets[networkID]?.wallet)
  })
}

// networkAsset returns the SupportedAsset for the variant's settlement
// chain (the variant itself for natives, the parent for tokens).
function networkAsset (v: AssetVariant) {
  const networkID = v.token ? v.token.networkID : v.assetID
  return app.user.assets[networkID]
}

function networkTicker (v: AssetVariant): string {
  const n = networkAsset(v)
  return n ? n.unitInfo.conventional.unit : ''
}

const noSubmit = (e: FormEvent) => e.preventDefault()

export default function Receive () {
  const [ta, setTa] = useState<TickerAsset | null>(null)
  const [variant, setVariant] = useState<AssetVariant | null>(null)
  const [address, setAddress] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const [copied, setCopied] = useState(false)
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
    const vs = activeVariants(chosen)
    if (vs.length === 1) {
      setVariant(vs[0])
      await fetchAddress(vs[0])
    }
  }

  const pickVariant = async (v: AssetVariant) => {
    setVariant(v)
    await fetchAddress(v)
  }

  const resetForBack = () => {
    reqIDRef.current++ // invalidate any in-flight request
    setAddress(''); setErr(''); setCopied(false); setLoading(false)
  }

  const backToAssets = () => {
    setTa(null); setVariant(null); resetForBack()
  }

  const backToNetwork = () => {
    // Only meaningful when ta has >1 active variant; otherwise behaves like backToAssets.
    if (ta && activeVariants(ta).length > 1) {
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

  // Step 2: network picker for multi-network tokens
  if (!variant) {
    const vs = activeVariants(ta)
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
            return (
              <div key={v.assetID} className="d-flex align-items-center px-3 py-2 hoverbg pointer" onClick={() => pickVariant(v)}>
                <CoinLogo ticker={nt} className="mini-icon me-2" />
                <span className="flex-grow-1">{nName}</span>
                <span className="grey fs12">{nt}</span>
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
  const isToken = Boolean(variant.token)
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
      {isToken && address && (
        <div className="px-3 pb-2 fs12 errcolor text-center">
          {tvar('Only send {{ ticker }} on the {{ network }} network. Other tokens or networks may result in lost funds.', { ticker: ta.ticker, network: netName })}
        </div>
      )}
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
