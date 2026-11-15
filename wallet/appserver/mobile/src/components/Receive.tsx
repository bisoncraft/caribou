import { useEffect, useRef, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { useFlash } from '@caribou/shared/hooks'
import { TickerAsset, AssetVariant, compareByFiatValue, networkAsset, isVariantActive } from '@caribou/shared/assets'
import { trustlineGate, minDepositNote, networkTickerAsset, useReceiveUpdates } from '@caribou/shared/trustline'
import QRCode from '@caribou/shared/components/QRCode'
import { useNav } from '../nav'
import { canShareText, copyText, shareText } from '../bridge'
import { Icon, NavHeader, ShareIcon, Spinner, Toast } from './ui'
import { AssetChip, AssetList, NetworkList, PickerSheet } from './Pickers'
import TrustlineGate from './TrustlineGate'

interface ReceiveProps {
  ticker?: string
  assetID?: number
}

function activeVariants (ta: TickerAsset): AssetVariant[] {
  return ta.variants.filter(isVariantActive)
}

// initialSelection resolves the flow's starting asset and network: an exact
// assetID wins, then a ticker with a single active network.
function initialSelection (ticker?: string, assetID?: number): { ta: TickerAsset | null, v: AssetVariant | null } {
  const ta = ticker ? app.tickerMap?.[ticker] ?? null : null
  if (!ta) return { ta: null, v: null }
  if (assetID !== undefined) {
    const v = ta.variantLookup[assetID]
    if (v && isVariantActive(v)) return { ta, v }
  }
  const vs = activeVariants(ta)
  return { ta, v: vs.length === 1 ? vs[0] : null }
}

// Receive walks asset -> network -> deposit address. The address is fetched
// from the variant's own wallet: a token wallet shares its network's address
// but only exists (and, for a trust-line token, only accepts deposits) when
// the token itself is set up. Single-network assets skip the network step.
export default function Receive ({ ticker, assetID }: ReceiveProps) {
  const nav = useNav()
  const initial = initialSelection(ticker, assetID)
  const [ta, setTa] = useState<TickerAsset | null>(initial.ta)
  const [variant, setVariant] = useState<AssetVariant | null>(initial.v)
  const [address, setAddress] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const [picker, setPicker] = useState<'asset' | 'network' | null>(null)
  const [copied, flashCopied] = useFlash(1500)
  const reqRef = useRef(0)

  // Wallet state lives on app.user and changes under the view (trust-line
  // notes, balances); re-render when the selected variant's does.
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
    setAddress(''); setErr(''); setLoading(false)
  }

  const pickAsset = (chosen: TickerAsset) => {
    clearAddress()
    setPicker(null)
    setTa(chosen)
    const vs = activeVariants(chosen)
    setVariant(vs.length === 1 ? vs[0] : null)
  }

  const pickVariant = (v: AssetVariant) => {
    if (v.assetID !== variant?.assetID) clearAddress()
    setPicker(null)
    setVariant(v)
  }

  // receiveNetwork re-targets the flow at the selected token's network asset,
  // so the account can be funded to cover the trust line.
  const receiveNetwork = () => {
    if (!variant) return
    const net = networkTickerAsset(variant)
    if (!net) return
    clearAddress()
    setTa(net.ta)
    setVariant(net.variant && isVariantActive(net.variant) ? net.variant : null)
  }

  const copy = async () => {
    if (address && await copyText(address)) flashCopied()
  }

  // A share backend that rejects falls back to copying, so the button never
  // silently does nothing.
  const share = async () => {
    if (address && await shareText(address) === 'failed') copy()
  }

  const receivable = (app.tickerList ?? []).filter(item => activeVariants(item).length > 0).sort(compareByFiatValue)

  let body
  if (!ta) {
    body = (
      <div className='body'>
        <AssetList assets={receivable} onPick={pickAsset} empty={t('Activate an asset first to receive funds.')} />
      </div>
    )
  } else {
    const vs = activeVariants(ta)
    const net = variant ? networkAsset(variant) : undefined
    const header = (
      <div className='recv-head'>
        <AssetChip ta={ta} onClick={() => setPicker('asset')} />
        {variant && (
          vs.length > 1
            ? (
              <button type='button' className='recv-net link-grey' onClick={() => setPicker('network')}>
                {tvar('on the {{ network }} network', { network: net?.name ?? '' })}
                <Icon name='chevron-left' className='chev-down' />
              </button>
              )
            : <div className='recv-net'>{tvar('on the {{ network }} network', { network: net?.name ?? '' })}</div>
        )}
      </div>
    )

    if (!variant) {
      body = (
        <div className='body'>
          {header}
          <h2 className='lbl sec'>{t('Choose the network to receive on.')}</h2>
          <NetworkList variants={vs} onPick={pickVariant} />
        </div>
      )
    } else if (gate) {
      // A token whose account cannot accept deposits yet: the gate replaces
      // the address until the trust line exists.
      body = (
        <div className='body'>
          {header}
          <TrustlineGate variant={variant} state={gate} onReceiveNetwork={receiveNetwork} />
        </div>
      )
    } else {
      const minDeposit = minDepositNote(variant, app.user?.assets[variant.assetID]?.wallet)
      body = (
        <div className='body recv'>
          {header}
          <div className='qr-tile'>
            {address
              ? <QRCode data={address} size={200} />
              : err
                ? <button type='button' className='qr-retry' onClick={() => fetchAddress(variant)}><Icon name='arrowright' />{t('Retry')}</button>
                : <Spinner large />}
          </div>
          <div className='lbl mt24'>{tvar('Your {{ name }} address', { name: ta.name })}</div>
          <p className='recv-addr mono'>{loading ? t('Loading...') : address}</p>
          {err && <div className='err-box mt12'>{err}</div>}
          <div className='btn-row mt24'>
            <button className='btn' type='button' onClick={copy} disabled={!address}>
              <Icon name='copy' className='ico' />{t('Copy')}
            </button>
            {canShareText() && (
              <button className='btn secondary' type='button' onClick={share} disabled={!address}>
                <ShareIcon />{t('Share')}
              </button>
            )}
          </div>
          {minDeposit > 0 && (
            <div className='callout caution mt20'>
              <Icon name='info' />
              <span>
                {tvar('This account is not on the ledger yet. The first deposit must be at least {{ amount }} {{ ticker }}; the network rejects smaller amounts.',
                  { amount: Doc.formatCoinValue(minDeposit, variant.ui), ticker: variant.ticker })}
              </span>
            </div>
          )}
          <div className='callout mt20'>
            <Icon name='info' />
            <span>
              {tvar('Send only {{ name }} ({{ ticker }}) on the {{ network }} network to this address. Funds sent from another network may be lost.',
                { name: ta.name, ticker: variant.ticker, network: net?.name ?? '' })}
            </span>
          </div>
        </div>
      )
    }
  }

  return (
    <div className='frame'>
      <NavHeader title={t('Receive')} onBack={nav.back} close />
      <div className='scroll'>{body}</div>
      {picker === 'asset' && (
        <PickerSheet title={t('Choose asset')} onClose={() => setPicker(null)}>
          <AssetList assets={receivable} onPick={pickAsset} empty={t('Activate an asset first to receive funds.')} />
        </PickerSheet>
      )}
      {picker === 'network' && ta && (
        <PickerSheet title={t('Choose network')} onClose={() => setPicker(null)}>
          <NetworkList variants={activeVariants(ta)} onPick={pickVariant} selected={variant} />
        </PickerSheet>
      )}
      {copied && <Toast>{t('Address copied')}</Toast>}
    </div>
  )
}
