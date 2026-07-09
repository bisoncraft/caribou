import { useEffect, useRef, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { postJSON } from '@caribou/shared/http'
import { TickerAsset, AssetVariant, networkAsset, isVariantActive } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'

interface ReceiveProps {
  ticker?: string
  onClose: () => void
}

function activeVariants (ta: TickerAsset): AssetVariant[] {
  return ta.variants.filter(isVariantActive)
}

// Receive walks asset -> network -> deposit address. Tokens share their
// settlement network's address, so the address is fetched for the network
// asset. Single-variant assets skip the network step.
export default function Receive ({ ticker, onClose }: ReceiveProps) {
  const initialTa = ticker ? (app.tickerMap?.[ticker] ?? null) : null
  const [ta, setTa] = useState<TickerAsset | null>(initialTa)
  const [variant, setVariant] = useState<AssetVariant | null>(
    initialTa && initialTa.variants.length === 1 && isVariantActive(initialTa.variants[0])
      ? initialTa.variants[0]
      : null)
  const [address, setAddress] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const [copied, setCopied] = useState(false)
  const reqRef = useRef(0)

  const fetchAddress = async (v: AssetVariant) => {
    const net = networkAsset(v)
    if (!net) { setErr(t('Network wallet not available.')); return }
    const id = ++reqRef.current
    setLoading(true); setErr(''); setAddress('')
    const r = await postJSON('/api/depositaddress', { assetID: net.id })
    if (id !== reqRef.current) return
    setLoading(false)
    if (!r.ok) { setErr(r.msg || t('Failed to fetch address')); return }
    setAddress(r.address)
  }

  useEffect(() => {
    if (variant) fetchAddress(variant)
  }, [])

  const pickAsset = (chosen: TickerAsset) => {
    setTa(chosen)
    if (chosen.variants.length === 1) {
      const v = chosen.variants[0]
      setVariant(v)
      fetchAddress(v)
    }
  }

  const pickVariant = (v: AssetVariant) => { setVariant(v); fetchAddress(v) }

  const copy = async () => {
    if (!address) return
    try {
      await navigator.clipboard.writeText(address)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch { /* clipboard unavailable */ }
  }

  let body
  if (!ta) {
    const list = (app.tickerList ?? []).filter(item => activeVariants(item).length > 0)
    body = list.length === 0
      ? <p className='mb-muted mb-pad'>{t('Activate an asset first to receive funds.')}</p>
      : (
        <ul className='mb-asset-list'>
          {list.map(item => (
            <li key={item.ticker} className='mb-asset-row mb-tappable' onClick={() => pickAsset(item)}>
              <CoinLogo ticker={item.ticker} alt={item.name} className='mb-coin-logo' />
              <div className='mb-asset-id'>
                <span className='mb-asset-ticker'>{item.ticker}</span>
                <span className='mb-asset-name mb-muted'>{item.name}</span>
              </div>
            </li>
          ))}
        </ul>
        )
  } else if (!variant) {
    const vs = activeVariants(ta)
    body = (
      <>
        <p className='mb-muted mb-pad'>{t('Choose the network to receive on.')}</p>
        <ul className='mb-asset-list'>
          {vs.map(v => {
            const net = networkAsset(v)
            return (
              <li key={v.assetID} className='mb-asset-row mb-tappable' onClick={() => pickVariant(v)}>
                <CoinLogo ticker={net?.unitInfo.conventional.unit ?? ''} className='mb-coin-logo' />
                <div className='mb-asset-id'><span className='mb-asset-ticker'>{net?.name ?? ''}</span></div>
              </li>
            )
          })}
        </ul>
      </>
    )
  } else {
    const net = networkAsset(variant)
    body = (
      <div className='mb-receive'>
        <div className='mb-muted'>
          {tvar('Network: {{ name }}', { name: net?.name ?? '' })}
        </div>
        <div className='mb-seed mb-address'>{loading ? t('Loading...') : address}</div>
        <div className='mb-err'>{err}</div>
        <button className='mb-btn' type='button' onClick={copy} disabled={!address}>
          {copied ? t('Copied!') : t('Copy Address')}
        </button>
      </div>
    )
  }

  return (
    <div className='mb-overlay'>
      <header className='mb-overlay-head'>
        <button className='mb-back' type='button' onClick={onClose} aria-label={t('Back')}>‹</button>
        <span>{ta ? tvar('Receive {{ name }}', { name: ta.name }) : t('Receive')}</span>
      </header>
      <div className='mb-overlay-body'>{body}</div>
    </div>
  )
}
