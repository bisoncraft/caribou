import { useState, useEffect, useCallback, useRef } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import Doc from '@bossw/shared/doc'
import { postJSON } from '@bossw/shared/http'
import { CoreNote, WalletNote, WalletTransaction } from '@bossw/shared/registry'
import { AssetVariant, TickerAsset, networkAsset } from '@bossw/shared/assets'
import { txTypeLabel, txAmountSign, txShowsAmount, txIsIncoming } from '@bossw/shared/tx'
import CoinLogo from '@bossw/shared/components/CoinLogo'
import NetworkedTokenLogo from '@bossw/shared/components/NetworkedTokenLogo'
import Spinner from './Spinner'

interface AssetParams {
  ticker: string
}

// Number of transactions fetched per network wallet.
const TX_PAGE_SIZE = 25

// TxItem pairs a transaction with the network variant it belongs to, which
// carries the units and asset ID needed to render and link it.
interface TxItem {
  tx: WalletTransaction
  variant: AssetVariant
}

// variantTotal returns a variant's total balance across available, locked, and
// immature, in atomic units.
function variantTotal (v: AssetVariant): number {
  const { available, locked, immature } = v.bal
  return available + locked + immature
}

// variantForTx maps a merged-history transaction back to the network variant it
// belongs to. Ticker-history responses attribute every transaction exactly via
// assetID. Single-asset responses lack it, so token transactions resolve via
// their tokenID and native ones to the group's native variant.
function variantForTx (ta: TickerAsset, tx: WalletTransaction): AssetVariant | undefined {
  if (tx.assetID != null) return ta.variantLookup[tx.assetID]
  if (tx.tokenID != null) return ta.variantLookup[tx.tokenID]
  return ta.variants.find(v => !v.token)
}

// txStatus renders the age of a confirmed transaction, or its confirmation
// progress ("confs out of finality confs") while it is still confirming.
function txStatus (tx: WalletTransaction): string {
  if (!tx.confirmed && tx.confirms) {
    return tvar('{{ current }}/{{ target }} confs', {
      current: String(tx.confirms.current),
      target: String(tx.confirms.target)
    })
  }
  if (tx.timestamp > 0) return tvar('{{ age }} ago', { age: Doc.timeSince(tx.timestamp * 1000) })
  return t('pending')
}

function TxRow ({ item }: { item: TxItem }) {
  const { tx, variant } = item
  const open = () => app.setForm({ form: 'txdetail', data: { assetID: variant.assetID, tx } })

  const sign = txAmountSign(tx.type)
  const amountText = txShowsAmount(tx.type)
    ? `${sign}${Doc.formatCoinValue(tx.amount, variant.ui)} ${variant.ticker}`
    : '—'

  return (
    <div className="tx-row d-flex align-items-center justify-content-between border-bottom pointer" onClick={open}>
      <div>
        <div className="fs14">{txTypeLabel(tx.type)}</div>
        <div className="grey fs12">{variant.ticker}</div>
      </div>
      <div className="text-right">
        <div className={`fs14 mono ${txIsIncoming(tx.type) ? 'tx-amt-pos' : ''}`}>{amountText}</div>
        <div className="grey fs12">{txStatus(tx)}</div>
      </div>
    </div>
  )
}

export default function Asset ({ ticker }: AssetParams) {
  const [, setTick] = useState(0)
  const redraw = useCallback(() => setTick(x => x + 1), [])
  const [txns, setTxns] = useState<TxItem[]>([])
  const [loadingTxns, setLoadingTxns] = useState(true)

  const ta = app.tickerMap?.[ticker]

  // Stable list of network variants that have a wallet — the ones we can pull
  // transaction history and balances from.
  const walletVariants = ta ? ta.variants.filter(v => app.wallet(v.assetID)) : []
  const assetIDs = walletVariants.map(v => v.assetID)
  const assetIDsKey = assetIDs.join(',')

  // fetchTxns pulls the most recent transactions in a single request. A
  // multi-network ticker uses the ticker endpoint, which the master txdb serves
  // merged and time-ordered from its ticker index. A single-network ticker uses
  // the per-asset endpoint: it is one call either way, and it also covers
  // wallets that keep their own history store rather than the master txdb (only
  // xmr today), which the ticker index does not see.
  const fetchTxnsRef = useRef<() => Promise<void>>()
  fetchTxnsRef.current = async () => {
    if (!ta) return
    const r = ta.variants.length > 1
      ? await postJSON('/api/txhistoryticker', { ticker: ta.ticker, n: TX_PAGE_SIZE, past: true })
      : await postJSON('/api/txhistory', { assetID: ta.variants[0].assetID, n: TX_PAGE_SIZE, past: true })
    const txs: WalletTransaction[] = r.ok && r.txs ? r.txs : []
    const items: TxItem[] = []
    for (const tx of txs) {
      const variant = variantForTx(ta, tx)
      if (variant) items.push({ tx, variant })
    }
    setTxns(items)
    setLoadingTxns(false)
  }

  useEffect(() => {
    setLoadingTxns(true)
    fetchTxnsRef.current?.()

    const balK = app.registerBalanceUpdater(redraw)
    const rateK = app.registerFiatRateUpdater(redraw)
    const noteK = app.registerNoteFeed((note: CoreNote) => {
      switch (note.type) {
        case 'balance':
        case 'walletstate':
        case 'walletconfig':
          redraw()
          break
        case 'walletnote': {
          const n = note as WalletNote
          if (n.payload.route === 'transaction' && assetIDs.includes(n.payload.assetID)) {
            redraw()
            fetchTxnsRef.current?.()
          }
          break
        }
      }
    })
    return () => {
      app.unregisterBalanceUpdater(balK)
      app.unregisterFiatRateUpdater(rateK)
      app.unregisterNoteFeed(noteK)
    }
  }, [assetIDsKey, redraw])

  const back = () => app.loadPage('portfolio')

  if (!ta) {
    return (
      <div className="networks-view fill-abs">
        <div className="networks-inner mx-auto w-100">
          <button type="button" className="subtle d-inline-flex align-items-center mb-4 fs14 py-1 px-2 hoverbg" onClick={back}>
            <span className="ico-arrowleft me-2 fs10" />{t('Portfolio')}
          </button>
          <div className="grey flex-center py-5">{t('Unknown asset')}</div>
        </div>
      </div>
    )
  }

  const rate = ta.xcRate ?? 0
  const totalConvFactor = ta.cFactor
  const hasUnavailable = ta.immature > 0 || ta.locked > 0
  const isMultiNet = ta.variants.length > 1

  return (
    <div className="networks-view fill-abs">
      <div className="networks-inner mx-auto w-100">
        <button type="button" className="subtle d-inline-flex align-items-center mb-4 fs14 py-1 px-2 hoverbg" onClick={back}>
          <span className="ico-arrowleft me-2 fs10" />{t('Portfolio')}
        </button>

        <div className="network-detail-header d-flex align-items-center">
          <CoinLogo ticker={ta.ticker} className="icon-48" />
          <div>
            <div className="fs28">{ta.name}</div>
            <div className="grey fs18">{ta.ticker}</div>
          </div>
        </div>

        <div className="network-section">
          <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('BALANCE')}</div>
          <div className="d-flex justify-content-between align-items-start">
            <div className="fs28 mono demi">{Doc.formatCoinValue(ta.total, ta.ui)} {ta.ticker}</div>
            <div className="d-flex">
              <button type="button" className="d-inline-flex align-items-center me-2" onClick={() => app.setForm({ form: 'receive', data: { ticker: ta.ticker } })}>
                <span className="ico-qrcode me-2" />{t('Receive')}
              </button>
              <button type="button" className="feature d-inline-flex align-items-center" onClick={() => app.setForm({ form: 'send', data: { ticker: ta.ticker } })}>
                <span className="ico-send me-2" />{t('Send')}
              </button>
            </div>
          </div>
          {rate > 0 && <div className="grey mb-2">{Doc.usdString(ta.total, totalConvFactor, rate)}</div>}
          {hasUnavailable && (
            <>
              <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
                <span className="grey fs13">{t('Available')}</span>
                <span className="mono">{Doc.formatCoinValue(ta.avail, ta.ui)} {ta.ticker}</span>
              </div>
              {ta.locked > 0 && (
                <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
                  <span className="grey fs13">{t('Locked')}</span>
                  <span className="mono">{Doc.formatCoinValue(ta.locked, ta.ui)} {ta.ticker}</span>
                </div>
              )}
              {ta.immature > 0 && (
                <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
                  <span className="grey fs13">{t('Immature')}</span>
                  <span className="mono">{Doc.formatCoinValue(ta.immature, ta.ui)} {ta.ticker}</span>
                </div>
              )}
            </>
          )}
        </div>

        {isMultiNet && (
          <div className="network-section">
            <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('NETWORK BALANCES')}</div>
            {ta.variants.map(v => {
              const netAsset = networkAsset(v)
              const vConvFactor = v.ui.conventional.conversionFactor
              const vTotalAtomic = variantTotal(v)
              // Portion of the ticker's total value that lives on this network,
              // computed in conventional units to be robust to differing
              // per-variant conversion factors.
              const totalConv = ta.total / totalConvFactor
              const vConv = vTotalAtomic / vConvFactor
              const pct = totalConv > 0 ? (vConv / totalConv) * 100 : 0
              return (
                <div key={v.assetID} className="network-asset-row d-flex align-items-center border-bottom">
                  <NetworkedTokenLogo token={ta.ticker} network={netAsset?.unitInfo.conventional.unit ?? ''} className="icon-32" />
                  <div className="flex-grow-1">
                    <div className="fs14">{netAsset?.name ?? ''}</div>
                    <div className="grey fs12">{pct.toFixed(1)}% {t('of total')}</div>
                  </div>
                  <div className="text-right">
                    <div className="fs14 mono">{Doc.formatCoinValue(vTotalAtomic, v.ui)} {v.ticker}</div>
                    {app.idRate(v.assetID) > 0 && (
                      <div className="grey fs12">{Doc.usdString(vTotalAtomic, vConvFactor, app.idRate(v.assetID))}</div>
                    )}
                  </div>
                </div>
              )
            })}
          </div>
        )}

        <div className="network-section">
          <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('TRANSACTIONS')}</div>
          {loadingTxns
            ? <div className="flex-center py-4"><Spinner size={20} /></div>
            : txns.length === 0
              ? <div className="grey flex-center py-4">{t('No transactions yet.')}</div>
              : txns.map(item => <TxRow key={`${item.variant.assetID}-${item.tx.id}`} item={item} />)}
        </div>
      </div>
    </div>
  )
}
