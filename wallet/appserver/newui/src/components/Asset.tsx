import { useState, useEffect, useRef } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { useForceUpdate, useCoreSubscriptions } from '@caribou/shared/hooks'
import { ApprovalStatus, CoreNote, WalletNote, WalletTransaction } from '@caribou/shared/registry'
import { AssetVariant, networkAsset, networkID } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import NetworkedTokenLogo from '@caribou/shared/components/NetworkedTokenLogo'
import Spinner from './Spinner'
import TxRow, { TxRowItem, variantForTx } from './TxRow'
import { IntentsSwap, fetchSwaps, relateTxToSwaps } from '../intents'

interface AssetParams {
  ticker: string
}

// Transactions fetched per request. Distinct from TX_PAGE_ROWS, the display
// page size: display pages slice the fetched feed.
const TX_PAGE_SIZE = 25
const TX_PAGE_ROWS = 5

// variantTotal returns a variant's total balance across available, locked, and
// immature, in atomic units.
function variantTotal (v: AssetVariant): number {
  const { available, locked, immature } = v.bal
  return available + locked + immature
}

export default function Asset ({ ticker }: AssetParams) {
  const redraw = useForceUpdate()
  const [loadingTxns, setLoadingTxns] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [page, setPage] = useState(0)
  // The fetched history feed. A mutable ref rather than state so nextPage can
  // recompute after fetches without waiting on a render.
  const txSrcRef = useRef<{ txs: WalletTransaction[], more: boolean }>({ txs: [], more: false })

  const ta = app.tickerMap?.[ticker]

  // Stable list of network variants that have a wallet — the ones we can pull
  // transaction history and balances from.
  const walletVariants = ta ? ta.variants.filter(v => app.wallet(v.assetID)) : []
  const assetIDs = walletVariants.map(v => v.assetID)
  const assetIDsKey = assetIDs.join(',')

  // fetchHistory pulls one batch of transactions, resuming after last when
  // given. A multi-network ticker uses the ticker endpoint, which the master
  // txdb serves merged and time-ordered from its ticker index. A
  // single-network ticker uses the per-asset endpoint: it is one call either
  // way, and it also covers wallets that keep their own history store rather
  // than the master txdb (only xmr today), which the ticker index does not
  // see.
  const fetchHistory = (last: WalletTransaction | null) => {
    if (!ta) return Promise.resolve({ ok: false })
    if (ta.variants.length > 1) {
      const req: Record<string, unknown> = { ticker: ta.ticker, n: TX_PAGE_SIZE, past: true }
      if (last) {
        req.refID = last.id
        const v = variantForTx(ta, last)
        if (v) req.refNetwork = networkID(v)
      }
      return postJSON('/api/txhistoryticker', req)
    }
    const req: Record<string, unknown> = { assetID: ta.variants[0].assetID, n: TX_PAGE_SIZE, past: true }
    if (last) req.refID = last.id
    return postJSON('/api/txhistory', req)
  }

  // Initial fetches are fired from both the assetIDsKey effect and every
  // transaction walletnote, and responses can land out of order — switching
  // assets mid-flight would otherwise let the previous asset's slower
  // response overwrite this one's rows. fetchGenRef stamps each fetch and
  // only the newest is allowed to apply.
  const fetchTxnsRef = useRef<() => Promise<void>>()
  const fetchGenRef = useRef(0)
  fetchTxnsRef.current = async () => {
    if (!ta) return
    const gen = ++fetchGenRef.current
    const r = await fetchHistory(null)
    if (gen !== fetchGenRef.current) return
    txSrcRef.current = { txs: r.ok && r.txs ? r.txs : [], more: Boolean(r.moreAvailable) }
    setLoadingTxns(false)
    redraw()
  }

  const loadMore = async () => {
    const gen = fetchGenRef.current
    const src = txSrcRef.current
    const last = src.txs[src.txs.length - 1]
    if (!last || !src.more) return
    setLoadingMore(true)
    try {
      const r = await fetchHistory(last)
      // The gen check gates only the data application: a superseding refetch
      // mid-flight discards this batch, but loadingMore must clear regardless
      // or the pager wedges disabled.
      if (gen !== fetchGenRef.current) return
      if (r.ok) {
        src.txs = src.txs.concat(r.txs ?? [])
        src.more = Boolean(r.moreAvailable)
      } else {
        src.more = false
      }
    } finally {
      setLoadingMore(false)
      redraw()
    }
  }

  useEffect(() => {
    setLoadingTxns(true)
    setPage(0)
    fetchTxnsRef.current?.()
  }, [assetIDsKey])

  // Cross-chain swap records, for marking transactions that serve as swap
  // legs.
  const [swaps, setSwaps] = useState<IntentsSwap[]>([])
  useEffect(() => {
    // Merge rather than replace: a live intentsswap upsert (the note
    // handler below) can land before this initial fetch resolves, and
    // the note's record is newer than the fetch's snapshot.
    fetchSwaps().then(fetched => setSwaps(prev => {
      const have = new Set(prev.map(s => s.depositAddress))
      return [...prev, ...fetched.filter(s => !have.has(s.depositAddress))]
    }))
  }, [])

  useCoreSubscriptions({
    onBalance: redraw,
    onRate: redraw,
    onNote: (note) => {
      switch (note.type) {
        case 'balance':
        case 'walletstate':
        case 'walletconfig':
          redraw()
          break
        case 'walletnote': {
          const n = note as WalletNote
          if (!assetIDs.includes(n.payload.assetID)) break
          if (n.payload.route === 'transaction') {
            redraw()
            fetchTxnsRef.current?.()
          } else if (n.payload.route === 'trustline') {
            redraw()
          }
          break
        }
        case 'intentsswap': {
          const s = (note as CoreNote & { swap: IntentsSwap }).swap
          if (s) setSwaps(prev => [s, ...prev.filter(x => x.depositAddress !== s.depositAddress)])
          break
        }
      }
    }
  }, [assetIDsKey, redraw])

  const back = () => app.loadPage('portfolio')

  if (!ta) {
    return (
      <div className="networks-view fill-abs">
        <div className="view-inner mx-auto w-100">
          <button type="button" className="subtle d-inline-flex align-items-center mb-4 fs14 py-1 px-2 hoverbg" onClick={back}>
            <span className="ico-arrowleft me-2 fs10" />{t('Portfolio')}
          </button>
          <div className="grey flex-center py-5">{t('Unknown asset')}</div>
        </div>
      </div>
    )
  }

  const rate = ta.xcRate ?? 0
  const hasUnavailable = ta.immature > 0 || ta.locked > 0
  const isMultiNet = ta.variants.length > 1

  const usdValue = ta.total * rate
  const totals = app.portfolioTotals()
  const portfolioPct = rate > 0 && totals.total > 0 ? (usdValue / totals.total) * 100 : null
  const balanceText = `${Doc.formatConventionalValue(ta.total, ta.precision)} ${ta.ticker}`

  // computeTxItems reads txSrcRef so nextPage can recompute after fetches
  // without waiting on a render. Rows without a resolvable variant drop out.
  const computeTxItems = (): TxRowItem[] => {
    const items: TxRowItem[] = []
    for (const tx of txSrcRef.current.txs) {
      const variant = variantForTx(ta, tx)
      if (variant) items.push({ tx, ta, variant, swapRelation: relateTxToSwaps(swaps, tx) })
    }
    return items
  }

  // nextPage advances the pager, fetching more first when the local buffer
  // cannot fill the next page (dropped rows can thin a fetch batch, hence
  // the bounded loop rather than a single fetch).
  const nextPage = async () => {
    const needed = (curPage + 1) * TX_PAGE_ROWS + 1
    for (let i = 0; i < 10 && computeTxItems().length < needed && txSrcRef.current.more; i++) {
      await loadMore()
    }
    if (computeTxItems().length > (curPage + 1) * TX_PAGE_ROWS) setPage(curPage + 1)
  }

  const txItems = computeTxItems()
  // Clamp against shrinkage (refetches) rather than tracking resets in every
  // state setter.
  const maxPage = Math.max(0, Math.ceil(txItems.length / TX_PAGE_ROWS) - 1)
  const curPage = Math.min(page, maxPage)
  const pageItems = txItems.slice(curPage * TX_PAGE_ROWS, (curPage + 1) * TX_PAGE_ROWS)
  const hasNextTxns = txItems.length > (curPage + 1) * TX_PAGE_ROWS || txSrcRef.current.more

  return (
    <div className="networks-view fill-abs">
      <div className="view-inner mx-auto w-100">
        <button type="button" className="subtle d-inline-flex align-items-center mb-4 fs14 py-1 px-2 hoverbg" onClick={back}>
          <span className="ico-arrowleft me-2 fs10" />{t('Portfolio')}
        </button>

        <div className="asset-card">
          <div className="d-flex align-items-center px-4 pt-4 pb-3">
            <CoinLogo ticker={ta.ticker} className="icon-48 me-3" />
            <div className="flex-grow-1">
              <div className="fs16 demi">{ta.name}</div>
              <div className="grey fs13">{ta.ticker}</div>
            </div>
            <div className="text-right">
              {rate > 0
                ? (
                  <>
                    <div className="fs26 mono demi">{Doc.formatUSD(usdValue)}</div>
                    <div className="grey fs13 mono">{balanceText}</div>
                  </>
                  )
                : <div className="fs26 mono demi">{balanceText}</div>}
            </div>
          </div>
          <div className="border-bottom mx-4" />
          <div className="d-flex align-items-center px-4 py-3">
            <div className="flex-grow-1 fs13 mono">
              {rate > 0 && (
                <>
                  <span className="demi">{Doc.formatUSD(rate)}</span> <span className="grey">{t('price')}</span>
                  {portfolioPct !== null && (
                    <>
                      <span className="grey mx-1">·</span>
                      <span className="demi">{portfolioPct.toFixed(1)}%</span> <span className="grey">{t('of portfolio')}</span>
                    </>
                  )}
                </>
              )}
            </div>
            <button
              type="button" className="round-btn p-0 border-0 icon-40 flex-center has-tip" aria-label={t('Receive')}
              onClick={() => app.setForm({ form: 'receive', data: { ticker: ta.ticker } })}
            >
              <span className="ico-qrcode fs16" />
            </button>
            <button
              type="button" className="round-btn p-0 border-0 icon-40 flex-center has-tip ms-2" aria-label={t('Send')}
              onClick={() => app.setForm({ form: 'send', data: { ticker: ta.ticker } })}
            >
              <span className="ico-send fs16" />
            </button>
            <button
              type="button" className="round-btn p-0 border-0 icon-40 flex-center has-tip ms-2" aria-label={t('Swap')}
              onClick={() => app.setForm({ form: 'swap', data: { ticker: ta.ticker, assetIDs: ta.variants.map(v => v.assetID) } })}
            >
              <span className="ico-swap fs16" />
            </button>
          </div>
        </div>

        {ta.variants.map(v => {
          const tl = v.token ? app.user?.assets[v.assetID]?.wallet?.trustline : undefined
          if (!tl || tl.status === ApprovalStatus.Approved) return null
          const net = app.user?.assets[v.token!.networkID]
          return (
            <div key={v.assetID} className="asset-notice d-flex align-items-center fs13 px-3 py-2 mb-3">
              <span className="flex-grow-1">
                {tl.status === ApprovalStatus.Pending
                  ? tvar('The {{ token }} trust line on {{ network }} is being established.', { token: ta.ticker, network: net?.name ?? '' })
                  : tvar('{{ token }} on {{ network }} cannot receive deposits until a trust line is established.', { token: ta.ticker, network: net?.name ?? '' })}
              </span>
              {tl.status !== ApprovalStatus.Pending && (
                <button
                  type="button" className="receive-activate-btn fs12 ms-3"
                  onClick={() => app.setForm({ form: 'receive', data: { ticker: ta.ticker, assetID: v.assetID } })}
                >
                  {t('Set up')}
                </button>
              )}
            </div>
          )
        })}

        {hasUnavailable && (
          <div className="network-section">
            <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('BALANCE')}</div>
            <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
              <span className="grey fs13">{t('Available')}</span>
              <span className="mono">{Doc.formatConventionalValue(ta.avail, ta.precision)} {ta.ticker}</span>
            </div>
              {ta.locked > 0 && (
                <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
                  <span className="grey fs13">{t('Locked')}</span>
                  <span className="mono">{Doc.formatConventionalValue(ta.locked, ta.precision)} {ta.ticker}</span>
                </div>
              )}
              {ta.immature > 0 && (
                <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
                  <span className="grey fs13">{t('Immature')}</span>
                  <span className="mono">{Doc.formatConventionalValue(ta.immature, ta.precision)} {ta.ticker}</span>
                </div>
              )}
          </div>
        )}

        {isMultiNet && (
          <div className="network-section">
            <div className="network-section-title fs11 grey demi pb-2 mb-1 border-bottom">{t('NETWORK BALANCES')}</div>
            {ta.variants.map(v => {
              const netAsset = networkAsset(v)
              const vConvFactor = v.ui.conventional.conversionFactor
              const vTotalAtomic = variantTotal(v)
              // Portion of the ticker's total value that lives on this network.
              // ta.total is already conventional; convert this variant likewise
              // so the ratio is unit-consistent.
              const vConv = vTotalAtomic / vConvFactor
              const pct = ta.total > 0 ? (vConv / ta.total) * 100 : 0
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
            : txItems.length === 0
              ? <div className="grey flex-center py-4">{t('No transactions yet.')}</div>
              : pageItems.map(item => <TxRow key={`${item.variant.assetID}-${item.tx.id}`} item={item} />)}
          {!loadingTxns && (curPage > 0 || hasNextTxns) && (
            <div className="flex-center pt-3">
              <button
                type="button" className="pager-btn icon-32 p-0 flex-center" aria-label={t('Previous page')}
                onClick={() => setPage(curPage - 1)} disabled={curPage === 0 || loadingMore}
              >
                <span className="ico-chevron-left fs12" />
              </button>
              <span className="grey fs13 mx-3">{tvar('Page {{ n }}', { n: String(curPage + 1) })}</span>
              <button
                type="button" className="pager-btn icon-32 p-0 flex-center" aria-label={t('Next page')}
                onClick={nextPage} disabled={!hasNextTxns || loadingMore}
              >
                {loadingMore ? <Spinner size={12} /> : <span className="ico-chevron-left fs12 mirror-x" />}
              </button>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
