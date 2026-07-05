import { useState, useEffect, useCallback, ReactNode } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import Doc from '@bossw/shared/doc'
import { CoreNote, WalletState, UnitInfo } from '@bossw/shared/registry'
import { networkBySymbol, NetworkAssetRef } from '@bossw/shared/assets'
import CoinLogo from '@bossw/shared/components/CoinLogo'
import Spinner from './Spinner'

interface NetworkParams {
  symbol: string
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
    <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
      <span className="grey fs13">{label}</span>
      <span>{value}</span>
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
      <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('NETWORK STATUS')}</div>
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
      <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('FEES')}</div>
      <Stat label={t('Network fee rate')} value={rateValue} />
      <Stat label={t('Cost of a standard send')} value={sendValue} />
    </div>
  )
}

// TokenRow renders one token's logo, name, and balance as a row in the tokens
// table.
function TokenRow ({ a }: { a: NetworkAssetRef }) {
  const asset = app.asset(a.assetID)
  const ui = asset.unitInfo
  const available = asset.wallet?.balance?.available ?? 0
  const rate = app.idRate(a.assetID) ?? 0
  const usd = usdString(available, ui.conventional.conversionFactor, rate)

  return (
    <tr className="asset-row">
      <td className="asset-row-asset">
        <div className="d-flex align-items-center">
          <CoinLogo ticker={a.ticker} className="icon-24 me-2" />
          <div>
            <div className="fs14">{asset.name}</div>
            <div className="grey fs12">{a.ticker}</div>
          </div>
        </div>
      </td>
      <td className="asset-row-num">
        <div className="mono">{Doc.formatCoinValue(available, ui)} {a.ticker}</div>
        {usd && <div className="grey fs12">{usd}</div>}
      </td>
    </tr>
  )
}

// Network is the detail view for a single settlement network. It is reached by
// clicking an activated network's card in the Networks list.
export default function Network ({ symbol }: NetworkParams) {
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

  const n = networkBySymbol(symbol)
  const back = () => app.loadPage('networks')

  if (!n) {
    return (
      <div className="networks-view fill-abs">
        <div className="networks-inner mx-auto w-100">
          <BackButton onClick={back} />
          <div className="grey flex-center py-5">{t('Unknown network')}</div>
        </div>
      </div>
    )
  }

  const assetID = n.nativeAssetID
  const wallet = app.wallet(assetID)
  const ui = app.unitInfo(assetID)
  const tokens = n.supportedAssets.filter(a => a.assetID !== n.nativeAssetID)
  const nativeAvailable = wallet?.balance?.available ?? 0
  const nativeUsd = usdString(nativeAvailable, ui.conventional.conversionFactor, app.idRate(assetID) ?? 0)

  return (
    <div className="networks-view fill-abs">
      <div className="networks-inner mx-auto w-100">
        <BackButton onClick={back} />
        <div className="network-detail-header d-flex align-items-center">
          <div className="fs28">{tvar('{{name}} Network', { name: n.name })}</div>
        </div>
        <div className="network-section">
          <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('NATIVE ASSET')}</div>
          <div className="d-flex align-items-center">
            <CoinLogo ticker={n.ticker} className="icon-40 me-3" />
            <div>
              <div className="fs26 mono demi">{Doc.formatCoinValue(nativeAvailable, ui)} {n.ticker}</div>
              {nativeUsd && <div className="grey">{nativeUsd}</div>}
            </div>
          </div>
        </div>
        {tokens.length > 0 && (
          <div className="network-section">
            <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('TOKENS')}</div>
            <table className="asset-table w-100">
              <thead>
                <tr className="asset-table-header">
                  <th className="asset-th-asset">{t('Token')}</th>
                  <th className="asset-th-num">{tvar('Balance on {{ network }}', { network: n.name })}</th>
                </tr>
              </thead>
              <tbody>
                {tokens.map(a => <TokenRow key={a.assetID} a={a} />)}
              </tbody>
            </table>
          </div>
        )}
        {wallet
          ? (
            <>
              <StatusSection wallet={wallet} />
              <FeeSection wallet={wallet} ui={ui} ticker={n.ticker} rate={app.idRate(assetID) ?? 0} />
            </>
            )
          : <div className="grey py-4">{t('This network is not activated.')}</div>}
      </div>
    </div>
  )
}
