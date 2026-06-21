import { useState, useEffect, FormEvent } from 'react'
import t, { tvar } from '../js/intl'
import app from '../js/application'
import { postJSON } from '../js/http'
import { AssetVariant } from '../js/assets'
import CoinLogo from './CoinLogo'
import Spinner from './Spinner'
import Checkbox from './Checkbox'

// Wallet type string constants that match the Go-side constants in
// wallet/asset/btc and wallet/asset/ltc.
const walletTypeElectrum = 'electrumRPC'
const walletTypeSPV = 'SPV'

interface ActivateAssetParams {
  ticker: string
}

const noSubmit = (e: FormEvent) => e.preventDefault()

// networkVariants returns all variants that require a network-level wallet
// creation to activate. For tokens this is the parent network asset; for
// native assets it is the asset itself.
function activationNetworkID (v: AssetVariant): number {
  return v.token ? v.token.networkID : v.assetID
}

// isNetworkActive returns true if the network wallet for the given variant is
// already created.
function isNetworkActive (v: AssetVariant): boolean {
  const nid = activationNetworkID(v)
  return Boolean(app.user?.assets[nid]?.wallet)
}

type RowStatus = 'pending' | 'activating' | 'done' | 'failed'

interface NetworkRow {
  v: AssetVariant
  networkID: number
  networkName: string
  networkTicker: string
  checked: boolean
  status: RowStatus
  errMsg: string
}

export default function ActivateAsset ({ ticker }: ActivateAssetParams) {
  const ta = app.tickerMap?.[ticker]

  const close = () => app.setForm({ form: '', data: {} })

  if (!ta) {
    return (
      <form className="modal form-width-300" onSubmit={noSubmit}>
        <div className="p-3 errcolor flex-center">{t('Unknown asset')}</div>
      </form>
    )
  }

  // Determine whether this is a multi-network token. Note: this differs
  // from TickerAsset.isMultiNet (variants.length > 1) — we also treat a
  // single-token-on-one-network as multi-net so the picker UI confirms
  // the parent chain to activate (e.g. USDC.polygon needs the Polygon
  // wallet, and the user should see "Polygon" before we create it).
  const isMultiNet = ta.variants.length > 1 || ta.variants.some(v => v.token)

  if (!isMultiNet) {
    // Single native asset — simple confirm flow as before.
    return <NativeActivate ticker={ticker} close={close} />
  }

  return <MultiNetActivate ticker={ticker} close={close} />
}

// SpvInfoPopover is a small popover that explains the "Increased security and
// privacy mode" (SPV/BIP-157/158) option and its tradeoffs vs. Electrum.
function SpvInfoPopover ({ onClose }: { onClose: () => void }) {
  return (
    <div className="spv-info-popover" role="dialog" aria-label={t('spv_info_title')}>
      <div className="spv-info-popover-header">
        <span className="fs14 spv-info-title">{t('spv_info_title')}</span>
        <button type="button" className="spv-info-close" onClick={onClose} aria-label={t('Close')}>✕</button>
      </div>
      <div className="fs13 grey spv-info-body">
        <p>{t('spv_info_what')}</p>
        <p>{t('spv_info_tradeoffs')}</p>
      </div>
    </div>
  )
}

// NativeActivate handles single-chain native assets: one click, done.
// For assets that support both Electrum and SPV (BIP-157/158) wallet types
// (currently BTC and LTC), it defaults to Electrum and provides an
// "Increased security and privacy mode" checkbox to opt into SPV.
function NativeActivate ({ ticker, close }: { ticker: string, close: () => void }) {
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState('')
  const [spvMode, setSpvMode] = useState(false)
  const [showInfo, setShowInfo] = useState(false)

  const ta = app.tickerMap![ticker]
  // The native variant is the one without a .token.
  const v = ta.variants.find(v => !v.token) ?? ta.variants[0]
  const networkID = activationNetworkID(v)
  const asset = app.user?.assets[networkID]
  const availableWallets = asset?.info?.availablewallets ?? []

  // Detect whether this asset supports both Electrum and SPV wallet types.
  // When it does, we show the simplified "Increased security and privacy mode"
  // checkbox rather than a raw wallet-type selector.
  const electrumDef = availableWallets.find(d => d.type === walletTypeElectrum)
  const spvDef = availableWallets.find(d => d.type === walletTypeSPV)
  const hasElectrumSpvChoice = Boolean(electrumDef && spvDef)

  // Pick the active wallet definition: if the asset has Electrum+SPV choice,
  // use the checkbox state; otherwise fall back to the first available wallet.
  let walletDef = availableWallets[0]
  if (hasElectrumSpvChoice) {
    walletDef = spvMode ? spvDef! : electrumDef!
  }

  if (!walletDef) {
    return (
      <form className="modal form-width-300" onSubmit={noSubmit}>
        <header>
          <CoinLogo ticker={ticker} className="mini-icon me-2" />
          <span>{ta.name}</span>
        </header>
        <div className="px-3 pb-3 grey text-center">
          {t('Activation for this asset is not yet supported.')}
        </div>
        <div className="flex-stretch-column p-3">
          <button type="button" onClick={close}>{t('Close')}</button>
        </div>
      </form>
    )
  }

  const submit = async () => {
    setErr('')
    setSubmitting(true)
    const r = await postJSON('/api/newwallet', {
      assetID: networkID,
      walletType: walletDef.type,
      config: {}
    })
    setSubmitting(false)
    if (!r.ok) {
      setErr(r.errMsg || r.msg || t('Failed to activate asset'))
      return
    }
    close()
  }

  const toggleInfo = () => setShowInfo(v => !v)

  return (
    <form className="modal form-width-300" onSubmit={noSubmit}>
      <header>
        <CoinLogo ticker={ticker} className="mini-icon me-2" />
        <span>{tvar('Activate {{ name }}', { name: ta.name })}</span>
      </header>
      {hasElectrumSpvChoice
        ? (
          <div className="px-3 pb-2 fs13 grey text-center">
            {t('activate_electrum_desc')}
          </div>
          )
        : (
          <div className="px-3 pb-2 fs14 grey">
            {walletDef.description}
          </div>
          )
      }
      {hasElectrumSpvChoice && (
        <div className="px-3 pb-3">
          <label className="checkbox-row spv-mode-row">
            <Checkbox
              checked={spvMode}
              onChange={checked => { setSpvMode(checked) }}
              disabled={submitting}
              ariaLabel={t('spv_mode_label')}
            />
            <span className="flex-grow-1 fs13">{t('spv_mode_label')}</span>
            <button
              type="button"
              className="spv-info-btn"
              onClick={toggleInfo}
              aria-label={t('spv_info_title')}
              aria-expanded={showInfo}
            >
              i
            </button>
          </label>
          {showInfo && (
            <SpvInfoPopover onClose={() => setShowInfo(false)} />
          )}
        </div>
      )}
      <div aria-live="polite">
        {err && (
          <div key={err} className="form-err flex-center errcolor px-3 pb-2 text-center fs13">
            {err}
          </div>
        )}
      </div>
      <div className="flex-stretch-column p-3">
        <button type="button" className="feature" onClick={submit} disabled={submitting}>
          {submitting ? <><Spinner className="me-2" />{t('Activating...')}</> : t('Activate Now')}
        </button>
        <button type="button" className="mt-2" onClick={close} disabled={submitting}>{t('Cancel')}</button>
      </div>
    </form>
  )
}

// MultiNetActivate handles tokens that exist on multiple networks.
function MultiNetActivate ({ ticker, close }: { ticker: string, close: () => void }) {
  const ta = app.tickerMap![ticker]

  // Build initial row state: one row per unique network that has a distinct variant.
  // Deduplicate by networkID so we don't show the same network twice.
  const seen = new Set<number>()
  const initialRows: NetworkRow[] = []
  for (const v of ta.variants) {
    const networkID = activationNetworkID(v)
    if (seen.has(networkID)) continue
    seen.add(networkID)
    const netAsset = app.user?.assets[networkID]
    const netTicker = netAsset?.unitInfo.conventional.unit ?? ''
    const netName = netAsset?.name ?? ''
    const active = isNetworkActive(v)
    // Default checked: active networks are non-selectable; inactive ones are
    // checked iff the network has priority: true.
    const priority = netAsset?.info?.network?.priority ?? false
    initialRows.push({
      v,
      networkID,
      networkName: netName,
      networkTicker: netTicker,
      checked: !active && priority,
      status: 'pending',
      errMsg: ''
    })
  }

  const [rows, setRows] = useState<NetworkRow[]>(initialRows)
  const [phase, setPhase] = useState<'pick' | 'running' | 'done'>('pick')

  const toggleRow = (networkID: number) => {
    if (phase !== 'pick') return
    setRows(prev => prev.map(r =>
      r.networkID === networkID && !isNetworkActive(r.v)
        ? { ...r, checked: !r.checked }
        : r
    ))
  }

  const anyChecked = rows.some(r => !isNetworkActive(r.v) && r.checked)

  const activate = async () => {
    setPhase('running')

    const toActivate = rows.filter(r => !isNetworkActive(r.v) && r.checked)
    for (const row of toActivate) {
      const netAsset = app.user?.assets[row.networkID]
      const walletDef = netAsset?.info?.availablewallets?.[0]

      setRows(prev => prev.map(r =>
        r.networkID === row.networkID ? { ...r, status: 'activating' } : r
      ))

      if (!walletDef) {
        setRows(prev => prev.map(r =>
          r.networkID === row.networkID
            ? { ...r, status: 'failed', errMsg: t('No wallet definition available') }
            : r
        ))
        continue
      }

      const res = await postJSON('/api/newwallet', {
        assetID: row.networkID,
        walletType: walletDef.type,
        config: {}
      })

      setRows(prev => prev.map(r => {
        if (r.networkID !== row.networkID) return r
        if (res.ok) return { ...r, status: 'done' }
        return { ...r, status: 'failed', errMsg: res.errMsg || res.msg || t('Failed to activate') }
      }))
    }

    setPhase('done')
  }

  const allSucceeded = phase === 'done' && rows
    .filter(r => !isNetworkActive(r.v) && r.checked)
    .every(r => r.status === 'done')

  // Close after a successful batch via effect — calling close() (which
  // does app.setForm) directly in render mutates a parent during a
  // child's render, which React rejects.
  useEffect(() => {
    if (allSucceeded) close()
  }, [allSucceeded, close])

  if (allSucceeded) return null

  const isRunning = phase === 'running'

  return (
    <form className="modal form-width-300" onSubmit={noSubmit}>
      <header>
        <CoinLogo ticker={ticker} className="mini-icon me-2" />
        <span>{tvar('Activate {{ name }}', { name: ta.name })}</span>
      </header>
      <div className="px-3 pb-2 fs14 grey text-center">
        {tvar('{{ name }} is available on multiple networks. Which networks would you like to activate?', { name: ta.name })}
      </div>
      <div className="pb-2">
        {rows.map(row => {
          const active = isNetworkActive(row.v)
          const rowCls = [
            'checkbox-row',
            active && 'checkbox-row--readonly',
            !active && row.checked && 'checkbox-row--selected',
            !active && isRunning && 'checkbox-row--disabled'
          ].filter(Boolean).join(' ')
          const trailing = (
            <>
              {active && <span className="grey fs12">{t('Active')}</span>}
              {!active && row.status === 'activating' && <Spinner size={14} />}
              {!active && row.status === 'done' && (
                <span className="fs12" style={{ color: 'var(--accent-syncing)' }}>✓</span>
              )}
              {!active && row.status === 'failed' && (
                <span className="errcolor fs12 has-tip" aria-label={row.errMsg}>{t('Failed')}</span>
              )}
            </>
          )
          const body = (
            <>
              <CoinLogo ticker={row.networkTicker} className="mini-icon" />
              <span className="flex-grow-1 fs14">{row.networkName}</span>
              {trailing}
            </>
          )
          if (active) {
            return (
              <div key={row.networkID} className={rowCls}>
                <span className="status-pip-on" aria-hidden>
                  <svg viewBox="0 0 24 24"><polyline points="20 6 9 17 4 12" /></svg>
                </span>
                {body}
              </div>
            )
          }
          return (
            <label key={row.networkID} className={rowCls}>
              <Checkbox
                checked={row.checked}
                onChange={() => toggleRow(row.networkID)}
                disabled={isRunning}
                ariaLabel={row.networkName}
              />
              {body}
            </label>
          )
        })}
      </div>
      {phase === 'done' && rows.some(r => r.status === 'failed') && (
        <div className="px-3 pb-2 fs12 errcolor text-center">
          {t('Some networks failed to activate. See above for details.')}
        </div>
      )}
      <div className="flex-stretch-column p-3">
        {phase !== 'done'
          ? <>
              <button
                type="button"
                className="feature"
                onClick={activate}
                disabled={!anyChecked || isRunning}
              >
                {isRunning ? <><Spinner className="me-2" />{t('Activating...')}</> : t('Activate')}
              </button>
              <button type="button" className="mt-2" onClick={close} disabled={isRunning}>{t('Cancel')}</button>
            </>
          : <button type="button" onClick={close}>{t('Close')}</button>
        }
      </div>
    </form>
  )
}
