import { useEffect, useRef, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { TickerAsset, AssetVariant, networkAsset, isVariantActive } from '@caribou/shared/assets'
import { trustlineGate, minDepositNote, networkTickerAsset, useReceiveUpdates } from '@caribou/shared/trustline'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import TrustlineGate from './TrustlineGate'

interface ReceiveProps {
  ticker?: string
  onClose: () => void
}

function activeVariants (ta: TickerAsset): AssetVariant[] {
  return ta.variants.filter(isVariantActive)
}

// Receive walks asset -> network -> deposit address. The address is fetched
// from the variant's own wallet: a token wallet shares its network's address
// but only exists (and, for a trust-line token, only accepts deposits) when
// the token itself is set up. Single-variant assets skip the network step.
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

  // Wallet state lives on app.user and changes under us (trust-line notes,
  // balances); re-render when the selected variant's does.
  useReceiveUpdates(variant)

  const fetchAddress = async (v: AssetVariant) => {
    const id = ++reqRef.current
    setLoading(true); setErr(''); setAddress('')
    const r = await postJSON('/api/depositaddress', { assetID: v.assetID })
    if (id !== reqRef.current) return
    setLoading(false)
    if (!r.ok) { setErr(r.msg || t('Failed to fetch address')); return }
    setAddress(r.address)
  }

  const gate = variant ? trustlineGate(variant) : null

  // Fetch the address whenever a variant is selected and nothing gates it.
  // A gated token fetches once its trust line confirms, which flips gate to
  // null through the wallet's trust-line note.
  useEffect(() => {
    if (variant && !gate && !address && !loading) fetchAddress(variant)
  }, [variant, gate === null])

  const clearAddress = () => {
    reqRef.current++ // invalidate any in-flight request
    setAddress(''); setErr(''); setLoading(false); setCopied(false)
  }

  const pickAsset = (chosen: TickerAsset) => {
    setTa(chosen)
    if (chosen.variants.length === 1) setVariant(chosen.variants[0])
  }

  const pickVariant = (v: AssetVariant) => { setVariant(v) }

  // receiveNetwork re-targets the overlay at the selected token's network
  // asset, so the account can be funded to cover the trust line.
  const receiveNetwork = () => {
    if (!variant) return
    const net = networkTickerAsset(variant)
    if (!net) return
    clearAddress()
    setTa(net.ta)
    setVariant(net.variant && isVariantActive(net.variant) ? net.variant : null)
  }

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
  } else if (gate) {
    // A token whose account cannot accept deposits yet: the gate replaces
    // the address until the trust line exists.
    body = (
      <>
        <div className='mb-muted mb-pad'>
          {tvar('Network: {{ name }}', { name: networkAsset(variant)?.name ?? '' })}
        </div>
        <TrustlineGate variant={variant} state={gate} onReceiveNetwork={receiveNetwork} />
      </>
    )
  } else {
    const net = networkAsset(variant)
    const minDeposit = minDepositNote(variant, app.user?.assets[variant.assetID]?.wallet)
    body = (
      <div className='mb-receive'>
        <div className='mb-muted'>
          {tvar('Network: {{ name }}', { name: net?.name ?? '' })}
        </div>
        <div className='mb-seed mb-address'>{loading ? t('Loading...') : address}</div>
        <div className='mb-err'>{err}</div>
        {minDeposit > 0 && (
          <p className='mb-muted'>
            {tvar('This account is not on the ledger yet. The first deposit must be at least {{ amount }} {{ ticker }}; the network rejects smaller amounts.',
              { amount: Doc.formatCoinValue(minDeposit, variant.ui), ticker: variant.ticker })}
          </p>
        )}
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
