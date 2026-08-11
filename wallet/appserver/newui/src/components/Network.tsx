import { ReactNode, useEffect, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { useForceUpdate, useCoreSubscriptions } from '@caribou/shared/hooks'
import { WalletState, UnitInfo, FeeState } from '@caribou/shared/registry'
import { networkBySymbol, normalizedTicker, NetworkAssetRef } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import NetworkedTokenLogo from '@caribou/shared/components/NetworkedTokenLogo'
import Spinner from './Spinner'

interface NetworkParams {
  symbol: string
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
    ? <span className="accent-green">{t('Synced')}</span>
    : (
      <span className="d-inline-flex align-items-center gold">
        <Spinner size={12} className="me-2" />
        {tvar('Syncing {{ pct }}%', { pct: String(Math.round((wallet.syncProgress ?? 0) * 100)) })}
      </span>
      )
  const blockHeight = wallet.synced
    ? Doc.formatInt(ss?.blocks ?? 0)
    : `${Doc.formatInt(ss?.blocks ?? 0)} / ${Doc.formatInt(ss?.targetHeight ?? 0)}`

  return (
    <div className="network-section">
      <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('NETWORK STATUS')}</div>
      <Stat label={t('Status')} value={status} />
      <Stat label={t('Network type')} value={wallet.class || '—'} />
      <Stat label={t('Peers')} value={Doc.formatInt(wallet.peerCount ?? 0)} />
      <Stat label={t('Block height')} value={blockHeight} />
    </div>
  )
}

// FeeSection shows the network fee rate and the cost of a standard send. Fee
// state is computed on demand — it is not part of WalletState — so it is
// fetched from /api/feestate when the section mounts.
function FeeSection ({ assetID, ui, ticker, rate }: { assetID: number, ui: UnitInfo, ticker: string, rate: number }) {
  const [fee, setFee] = useState<FeeState | null>(null)
  const [loaded, setLoaded] = useState(false)
  useEffect(() => {
    let stale = false
    const fetchFeeState = async () => {
      const r = await postJSON('/api/feestate', { assetID })
      if (stale) return
      setFee(r.ok && r.feeState?.rate ? r.feeState : null)
      setLoaded(true)
    }
    fetchFeeState()
    return () => { stale = true }
  }, [assetID])

  const rateValue = fee
    ? `${Doc.formatInt(fee.rate)} ${ui.atomicUnit}/${ui.feeRateDenom}`
    : loaded ? t('Unavailable') : '—'

  const sendUsd = fee ? Doc.usdString(fee.send, ui.conventional.conversionFactor, rate) : ''
  const sendValue = fee
    ? <>{Doc.formatCoinValue(fee.send, ui)} {ticker}{sendUsd && <span className="grey"> ({sendUsd})</span>}</>
    : loaded ? t('Unavailable') : '—'

  return (
    <div className="network-section">
      <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('FEES')}</div>
      <Stat label={t('Network fee rate')} value={rateValue} />
      <Stat label={t('Cost of a standard send')} value={sendValue} />
    </div>
  )
}

// TokenRow renders one token's logo, name, and balance as a row in the tokens
// table. Every token shares the same network badge (networkTicker) since they
// all settle on this view's network.
function TokenRow ({ a, networkTicker }: { a: NetworkAssetRef, networkTicker: string }) {
  const asset = app.asset(a.assetID)
  const ui = asset.unitInfo
  const available = asset.wallet?.balance?.available ?? 0
  const rate = app.idRate(a.assetID) ?? 0
  const usd = Doc.usdString(available, ui.conventional.conversionFactor, rate)
  const open = () => app.loadPage(`asset/${normalizedTicker(asset)}`)

  return (
    <tr className="asset-row pointer hoverbg" onClick={open}>
      <td className="asset-row-asset">
        <div className="d-flex align-items-center">
          <NetworkedTokenLogo token={a.ticker} network={networkTicker} className="icon-32 me-2" />
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
  const redraw = useForceUpdate()

  useCoreSubscriptions({
    onBalance: redraw,
    onRate: redraw,
    onNote: (note) => {
      switch (note.type) {
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
          redraw()
          break
      }
    }
  }, [redraw])

  const n = networkBySymbol(symbol)
  const back = () => app.loadPage('networks')

  if (!n) {
    return (
      <div className="networks-view fill-abs">
        <div className="view-inner mx-auto w-100">
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
  const nativeUsd = Doc.usdString(nativeAvailable, ui.conventional.conversionFactor, app.idRate(assetID) ?? 0)

  return (
    <div className="networks-view fill-abs">
      <div className="view-inner mx-auto w-100">
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
                {tokens.map(a => <TokenRow key={a.assetID} a={a} networkTicker={n.ticker} />)}
              </tbody>
            </table>
          </div>
        )}
        {wallet
          ? (
            <>
              <StatusSection wallet={wallet} />
              <FeeSection assetID={assetID} ui={ui} ticker={n.ticker} rate={app.idRate(assetID) ?? 0} />
            </>
            )
          : <div className="grey py-4">{t('This network is not activated.')}</div>}
      </div>
    </div>
  )
}
