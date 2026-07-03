import { useState, useEffect, useCallback, ReactNode } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import Doc from '@bossw/shared/doc'
import { CoreNote, WalletState, UnitInfo } from '@bossw/shared/registry'
import { networkSummary, normalizedTicker, NetworkAssetRef } from '@bossw/shared/assets'
import CoinLogo from '@bossw/shared/components/CoinLogo'
import Spinner from './Spinner'

interface NetworkParams {
  assetID: number
}

const languages = navigator.languages as string[]

const usdFmt = new Intl.NumberFormat(languages, {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

const intFmt = new Intl.NumberFormat(languages, { maximumFractionDigits: 0 })

// usdString converts an atomic amount to a formatted USD string, or '' when the
// fiat rate is unknown.
function usdString (atomic: number, cFactor: number, rate: number): string {
  if (!rate) return ''
  return usdFmt.format((atomic / cFactor) * rate)
}

function BackButton ({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" className="subtle d-inline-flex align-items-center mb-4 fs14 py-1 px-2 hoverbg" onClick={onClick}>
      <span className="ico-arrowleft me-2 fs10" />{t('Networks')}
    </button>
  )
}

// Stat is a labeled key/value row used in the status and fee sections.
function Stat ({ label, value }: { label: string, value: ReactNode }) {
  return (
    <div className="network-stat">
      <span className="network-stat-label">{label}</span>
      <span className="network-stat-value">{value}</span>
    </div>
  )
}

function StatusSection ({ wallet }: { wallet: WalletState }) {
  const ss = wallet.syncStatus
  const status = wallet.synced
    ? <span style={{ color: 'var(--accent-green)' }}>{t('Synced')}</span>
    : (
      <span className="d-inline-flex align-items-center" style={{ color: 'var(--accent-syncing)' }}>
        <Spinner size={12} className="me-2" />
        {tvar('Syncing {{ pct }}%', { pct: String(Math.round((wallet.syncProgress ?? 0) * 100)) })}
      </span>
      )
  const blockHeight = wallet.synced
    ? intFmt.format(ss?.blocks ?? 0)
    : `${intFmt.format(ss?.blocks ?? 0)} / ${intFmt.format(ss?.targetHeight ?? 0)}`

  return (
    <div className="network-section">
      <div className="network-section-title">{t('NETWORK STATUS')}</div>
      <Stat label={t('Status')} value={status} />
      <Stat label={t('Network type')} value={wallet.class || '—'} />
      <Stat label={t('Peers')} value={intFmt.format(wallet.peerCount ?? 0)} />
      <Stat label={t('Block height')} value={blockHeight} />
    </div>
  )
}

function FeeSection ({ wallet, ui, ticker, rate }: { wallet: WalletState, ui: UnitInfo, ticker: string, rate: number }) {
  const fee = wallet.feeState

  const rateValue = fee
    ? `${intFmt.format(fee.rate)} ${ui.atomicUnit}/${ui.feeRateDenom}`
    : t('Unavailable')

  const sendUsd = fee ? usdString(fee.send, ui.conventional.conversionFactor, rate) : ''
  const sendValue = fee
    ? <>{Doc.formatCoinValue(fee.send, ui)} {ticker}{sendUsd && <span className="grey"> ({sendUsd})</span>}</>
    : t('Unavailable')

  return (
    <div className="network-section">
      <div className="network-section-title">{t('FEES')}</div>
      <Stat label={t('Network fee rate')} value={rateValue} />
      <Stat label={t('Cost of a standard send')} value={sendValue} />
    </div>
  )
}

function AssetBalanceRow ({ a }: { a: NetworkAssetRef }) {
  const asset = app.asset(a.assetID)
  const ui = asset.unitInfo
  const rate = app.idRate(a.assetID) ?? 0
  const available = asset.wallet?.balance?.available ?? 0
  const open = () => app.loadPage(`asset/${normalizedTicker(asset)}`)

  return (
    <div className="network-asset-row pointer hoverbg" onClick={open}>
      <CoinLogo ticker={a.ticker} className="network-asset-logo" />
      <div className="flex-grow-1">
        <div className="fs14">{asset.name}</div>
        <div className="grey fs12">{a.ticker}</div>
      </div>
      <div className="text-right">
        <div className="fs14 mono">{Doc.formatCoinValue(available, ui)} {a.ticker}</div>
        {rate > 0 && <div className="grey fs12">{usdString(available, ui.conventional.conversionFactor, rate)}</div>}
      </div>
    </div>
  )
}

// Network is the detail view for a single settlement network. It is reached by
// clicking an activated network's card in the Networks list.
export default function Network ({ assetID }: NetworkParams) {
  const [, setTick] = useState(0)
  const redraw = useCallback(() => setTick(x => x + 1), [])

  useEffect(() => {
    const balK = app.registerBalanceUpdater(redraw)
    const rateK = app.registerFiatRateUpdater(redraw)
    const noteK = app.registerNoteFeed((note: CoreNote) => {
      switch (note.type) {
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
          redraw()
          break
      }
    })
    return () => {
      app.unregisterBalanceUpdater(balK)
      app.unregisterFiatRateUpdater(rateK)
      app.unregisterNoteFeed(noteK)
    }
  }, [redraw])

  const n = networkSummary(assetID)
  const back = () => app.loadPage('networks')

  if (!n) {
    return (
      <div className="networks-view fill-abs">
        <div className="networks-inner">
          <BackButton onClick={back} />
          <div className="grey flex-center py-5">{t('Unknown network')}</div>
        </div>
      </div>
    )
  }

  const wallet = app.wallet(assetID)
  const ui = app.unitInfo(assetID)
  const assetWord = n.assetCount === 1 ? t('asset') : t('assets')

  return (
    <div className="networks-view fill-abs">
      <div className="networks-inner">
        <BackButton onClick={back} />
        <div className="network-detail-header">
          <CoinLogo ticker={n.ticker} className="network-detail-logo" />
          <div>
            <div className="fs22">{n.name}</div>
            <div className="grey fs13">
              {tvar('Supports {{ count }} {{ assetWord }}', { count: String(n.assetCount), assetWord })}
            </div>
          </div>
        </div>
        {wallet
          ? (
            <>
              <StatusSection wallet={wallet} />
              <FeeSection wallet={wallet} ui={ui} ticker={n.ticker} rate={app.idRate(assetID) ?? 0} />
            </>
            )
          : <div className="grey py-4">{t('This network is not activated.')}</div>}
        <div className="network-section">
          <div className="network-section-title">{t('ASSETS')}</div>
          {n.supportedAssets.map(a => <AssetBalanceRow key={a.assetID} a={a} />)}
        </div>
      </div>
    </div>
  )
}
