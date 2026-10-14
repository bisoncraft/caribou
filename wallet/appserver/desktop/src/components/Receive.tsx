import { useEffect, useLayoutEffect, useRef, useState, FormEvent } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { useFlash } from '@caribou/shared/hooks'
import { TickerAsset, AssetVariant, networkAsset, isVariantActive, compareByFiatValue } from '@caribou/shared/assets'
import { trustlineGate, minDepositNote, useReceiveUpdates } from '@caribou/shared/trustline'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import QRCode from '@caribou/shared/components/QRCode'
import Spinner from './Spinner'
import TrustlineGate from './TrustlineGate'

// activeVariants returns the variants of a ticker whose own wallet exists and
// can therefore be received to. Token wallets are created alongside their
// network wallet, so for a token this normally coincides with the network
// being activated.
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
  // Optional preselected variant of that ticker, by asset ID. When set and
  // active, the network picker is skipped too.
  assetID?: number
}

// preselected resolves an optional ticker prop to a TickerAsset and, when
// the ticker has exactly one variant or a specific active variant was named,
// that variant — so we can skip the asset (and where unambiguous, the network)
// picker on mount. Returns nulls if the ticker is missing or has no active
// variant.
function preselected (ticker?: string, assetID?: number): { ta: TickerAsset | null, v: AssetVariant | null } {
  if (!ticker) return { ta: null, v: null }
  const ta = app.tickerMap?.[ticker]
  if (!ta || activeVariants(ta).length === 0) return { ta: null, v: null }
  const named = assetID === undefined ? undefined : ta.variantLookup[assetID]
  if (named && isVariantActive(named)) return { ta, v: named }
  // Skip step 2 only for single-variant tickers (same rule as pickAsset).
  return { ta, v: ta.variants.length === 1 ? ta.variants[0] : null }
}

export default function Receive ({ ticker, assetID }: ReceiveProps) {
  const initial = preselected(ticker, assetID)
  const [ta, setTa] = useState<TickerAsset | null>(initial.ta)
  const [variant, setVariant] = useState<AssetVariant | null>(initial.v)
  const [address, setAddress] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const [copied, flashCopied, clearCopied] = useFlash(1500)
  // Per-variant activation state for inline Activate in step 2.
  const [activating, setActivating] = useState<number | null>(null)
  const [activateErr, setActivateErr] = useState<Record<number, string>>({})
  const [showQR, setShowQR] = useState(false)
  // Step-1 asset search filter.
  const [query, setQuery] = useState('')
  // Character budget for the address pill, from measured font metrics: the
  // span is monospace, so capacity is its laid-out width over one glyph
  // advance. null until measured; display falls back to a fixed truncation.
  const [charBudget, setCharBudget] = useState<number | null>(null)
  const addrSpanRef = useRef<HTMLSpanElement | null>(null)
  // Monotonic request ID so stale /api/depositaddress responses can't
  // overwrite a newer selection.
  const reqIDRef = useRef(0)
  // Wallet state lives on app.user and changes under us (trust-line notes,
  // balances); re-render when the selected variant's does.
  useReceiveUpdates(variant)

  const close = () => app.setForm({ form: '', data: {} })

  // Measure the address span's capacity. The span flex-grows into the free
  // space, so its width is independent of its (possibly truncated) content —
  // no feedback loop. Re-measured when webfonts land, since the mono font's
  // advance is the divisor.
  useLayoutEffect(() => {
    const span = addrSpanRef.current
    if (!span) return
    const measure = () => {
      const style = getComputedStyle(span)
      const ctx = document.createElement('canvas').getContext('2d')
      if (!ctx) return // e.g. jsdom; the fixed fallback truncation applies
      ctx.font = `${style.fontSize} ${style.fontFamily}`
      const charW = ctx.measureText('0').width
      if (charW > 0) setCharBudget(Math.floor(span.getBoundingClientRect().width / charW))
    }
    measure()
    if (document.fonts) document.fonts.ready.then(measure)
  }, [address])

  // displayAddress fills the measured budget: the whole address when it
  // fits, otherwise middle-truncated to exactly the capacity.
  const displayAddress = () => {
    if (charBudget === null) return Doc.shortenAddress(address, 10)
    if (address.length <= charBudget) return address
    return Doc.shortenAddress(address, Math.max(4, Math.floor((charBudget - 1) / 2)))
  }

  // fetchAddress asks the variant's own wallet for its deposit address. For a
  // token that is the token wallet, which shares the network's address but
  // only exists (and, for a trust-line token, only accepts deposits) when the
  // token itself is set up.
  const fetchAddress = async (v: AssetVariant) => {
    const id = ++reqIDRef.current
    setLoading(true); setErr(''); setAddress('')
    const r = await postJSON('/api/depositaddress', { assetID: v.assetID })
    if (id !== reqIDRef.current) return // a newer request superseded us
    setLoading(false)
    if (!r.ok) {
      setErr(r.msg || t('Failed to fetch address'))
      return
    }
    setAddress(r.address)
  }

  const gate = variant ? trustlineGate(variant) : null

  // Fetch the address whenever a variant is selected and nothing gates it.
  // A gated token fetches once its trust line confirms, which flips gate to
  // null through the wallet's trust-line note.
  useEffect(() => {
    if (variant && !gate && !address && !loading) fetchAddress(variant)
  }, [variant, gate === null])

  const pickAsset = (chosen: TickerAsset) => {
    setTa(chosen)
    setActivating(null)
    setActivateErr({})
    // Only skip the network picker for single-variant tickers (e.g. BTC,
    // native ETH). Multi-network tokens always show step 2 so the user
    // can see other networks and activate them inline if they want.
    if (chosen.variants.length === 1) setVariant(chosen.variants[0])
  }

  const pickVariant = (v: AssetVariant) => {
    setVariant(v)
  }

  const resetForBack = () => {
    reqIDRef.current++ // invalidate any in-flight request
    setAddress(''); setErr(''); clearCopied(); setLoading(false); setShowQR(false)
  }

  const backToAssets = () => {
    setTa(null); setVariant(null); setActivating(null); setActivateErr({}); setQuery(''); resetForBack()
  }

  // changeNetwork returns to the step-2 network picker. Only offered for
  // multi-variant tickers; single-variant tickers have nothing to change.
  const changeNetwork = () => {
    setVariant(null); resetForBack()
  }

  const copy = async () => {
    if (!address) return
    await navigator.clipboard.writeText(address)
    flashCopied()
  }

  // Inline activate: fires a single-network activation from the network
  // picker. Creating the network wallet creates its token wallets too. When
  // the network wallet already exists and only this token's is missing, the
  // token wallet is created on its own.
  const inlineActivate = async (v: AssetVariant) => {
    const netAsset = networkAsset(v)
    const tokenOnly = Boolean(v.token && netAsset?.wallet)
    const target = tokenOnly ? app.user?.assets[v.assetID] : netAsset
    const walletType = tokenOnly ? target?.token?.definition?.type : target?.info?.availablewallets?.[0]?.type
    if (!target || walletType === undefined) {
      setActivateErr(prev => ({ ...prev, [v.assetID]: t('No wallet definition available') }))
      return
    }
    setActivating(v.assetID)
    setActivateErr(prev => { const next = { ...prev }; delete next[v.assetID]; return next })
    const r = await postJSON('/api/newwallet', {
      assetID: target.id,
      walletType,
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
    pickVariant(v)
  }

  // Step 1: asset picker. Default order mirrors the portfolio table (fiat
  // value, descending); the search field filters by name or ticker.
  if (!ta) {
    const active = (app.tickerList ?? [])
      .filter(item => activeVariants(item).length > 0)
      .sort(compareByFiatValue)
    const q = query.trim().toLowerCase()
    const list = q === ''
      ? active
      : active.filter(item =>
        item.name.toLowerCase().includes(q) || item.ticker.toLowerCase().includes(q))
    if (active.length === 0) {
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
        <header><span>{t('Select asset to receive')}</span></header>
        <div className="px-3 pb-2">
          <div className="d-flex align-items-center form-input-bg br5">
            <span className="ico-search fs16 p-2" />
            <input
              type="text"
              value={query}
              placeholder={t('search')}
              onChange={e => setQuery(e.target.value)}
              autoFocus
            />
          </div>
        </div>
        <div className="receive-asset-list overflow-y-auto mb-2">
          {list.map(item => (
            <div key={item.ticker} className="d-flex align-items-center px-3 py-2 hoverbg pointer" onClick={() => pickAsset(item)}>
              <CoinLogo ticker={item.ticker} className="mini-icon me-2" />
              <span className="flex-grow-1">{item.name}</span>
              <span className="grey fs12">{item.ticker}</span>
            </div>
          ))}
          {list.length === 0 && (
            <div className="px-3 py-2 grey fs14 text-center">{t('No matching assets.')}</div>
          )}
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
          <span className="me-2">{t('Receive')}</span>
          <CoinLogo ticker={ta.ticker} className="mini-icon me-2" />
          <span>{ta.ticker}</span>
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

  // Step 3: address display, or the trust-line gate for a token whose account
  // cannot accept deposits yet.
  const netName = networkAsset(variant)?.name ?? ''
  const vWallet = app.user?.assets[variant.assetID]?.wallet
  const minDeposit = minDepositNote(variant, vWallet)
  return (
    <form className="modal form-width-300" onSubmit={noSubmit}>
      <header>
        <span className="me-2">{t('Receive')}</span>
        <CoinLogo ticker={ta.ticker} className="mini-icon me-2" />
        <span>{ta.ticker}</span>
      </header>
      <div className="px-3 pb-2 fs14 d-flex align-items-center justify-content-center grey">
        <span className="me-2">{t('Network:')}</span>
        <span>{netName}</span>
        {ta.variants.length > 1 && (
          <button
            type="button"
            className="icon-btn d-inline-flex align-items-center justify-content-center p-0 border-0 has-tip ico-edit fs14 ms-1"
            aria-label={t('Change network')}
            onClick={changeNetwork}
          />
        )}
      </div>
      {gate
        ? <TrustlineGate variant={variant} state={gate} />
        : (
          <>
            <div className="mx-3 mb-2 py-1 px-2 d-flex align-items-center form-input-bg br5" title={address || undefined}>
              {/* Clicking the address is a copy action (selection is disabled, so
                  select-to-copy is not an alternative here); the labeled copy
                  button remains the accessible control. */}
              <span
                ref={addrSpanRef}
                className={`mono fs16 flex-grow-1 text-center no-select ${address !== '' ? 'pointer' : ''} ${copied ? 'accent-green' : ''}`}
                onClick={copy}
              >
                {loading ? t('Loading...') : displayAddress()}
              </span>
              {address !== '' && (
                <button
                  type="button"
                  className="icon-btn d-inline-flex align-items-center justify-content-center p-0 border-0 has-tip ico-copy fs14 ms-2"
                  aria-label={copied ? t('Copied!') : t('Copy Address')}
                  onClick={copy}
                />
              )}
            </div>
            <div className={`flex-center errcolor px-3 pb-2 ${err ? '' : 'd-none'}`}>{err}</div>
            {minDeposit > 0 && (
              <div className="px-3 pb-2 fs13 gold text-center">
                {tvar('This account is not on the ledger yet. The first deposit must be at least {{ amount }} {{ ticker }}; the network rejects smaller amounts.',
                  { amount: Doc.formatCoinValue(minDeposit, variant.ui), ticker: variant.ticker })}
              </div>
            )}
            <div className="flex-center pb-2">
              <button type="button" className="noborder grey fs13 d-flex align-items-center p-1"
                onClick={() => setShowQR(!showQR)}>
                <span className="ico-qrcode me-1" />
                {showQR ? t('Hide QR code') : t('Show QR code')}
              </button>
            </div>
            {showQR && (
              <div className="flex-center px-3 pb-2">
                <div className="qr-tile flex-center">
                  {address
                    ? <QRCode data={address} size={132} />
                    : loading && <Spinner size={24} />}
                </div>
              </div>
            )}
          </>
          )}
    </form>
  )
}
