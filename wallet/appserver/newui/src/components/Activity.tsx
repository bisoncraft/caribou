import { useEffect, useRef, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { CoreNote, WalletNote, WalletTransaction } from '@caribou/shared/registry'
import { TickerAsset, AssetVariant, isVariantActive, networkID } from '@caribou/shared/assets'
import { TxType, txTypeLabel, txIsIncoming, txShowsAmount, txAmountSign } from '@caribou/shared/tx'
import Spinner from './Spinner'

const PAGE_SIZE = 20
// Rows shown per display page. Distinct from PAGE_SIZE, the per-source fetch
// batch: display pages slice the merged, filtered feed.
const PAGE_ROWS = 10

type Filter = 'all' | 'sent' | 'received' | 'swapped'

// The filter pills partition the common types; everything exotic (approvals,
// bridges, tickets, mixes, …) shows only under All.
const filterTypes: Record<Exclude<Filter, 'all'>, Set<number>> = {
  sent: new Set<number>([TxType.Send, TxType.SelfSend, TxType.SwapOrSend]),
  received: new Set<number>([TxType.Receive, TxType.Redeem, TxType.Refund]),
  swapped: new Set<number>([TxType.Swap])
}

const filters: { key: Filter, label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'sent', label: 'Sent' },
  { key: 'received', label: 'Received' },
  { key: 'swapped', label: 'Swapped' }
]

interface TxItem {
  tx: WalletTransaction
  ta: TickerAsset
  variant: AssetVariant
}

// variantForTx maps a merged-history transaction to its network variant (the
// same resolution the Asset view uses): ticker-history rows carry assetID,
// token rows resolve via tokenID, and native rows fall back to the group's
// native variant.
function variantForTx (ta: TickerAsset, tx: WalletTransaction): AssetVariant | undefined {
  if (tx.assetID != null) return ta.variantLookup[tx.assetID]
  if (tx.tokenID != null) return ta.variantLookup[tx.tokenID]
  return ta.variants.find(v => !v.token)
}

// effectiveTime orders the merged feed; mempool transactions (timestamp 0)
// sort newest.
function effectiveTime (tx: WalletTransaction): number {
  return tx.timestamp === 0 ? Number.MAX_SAFE_INTEGER : tx.timestamp
}

type Section = 'today' | 'yesterday' | 'week' | 'older'

const sections: { key: Section, label: string }[] = [
  { key: 'today', label: 'Today' },
  { key: 'yesterday', label: 'Yesterday' },
  { key: 'week', label: 'Earlier this week' },
  { key: 'older', label: 'Older' }
]

// sectionFor buckets a transaction by local calendar time: today, yesterday,
// earlier in the current (Monday-based) week, or older. Mempool transactions
// count as today.
function sectionFor (tx: WalletTransaction): Section {
  if (tx.timestamp === 0) return 'today'
  const ms = tx.timestamp * 1000
  const now = new Date()
  const dayMS = 86400 * 1000
  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
  if (ms >= todayStart) return 'today'
  if (ms >= todayStart - dayMS) return 'yesterday'
  const weekStart = todayStart - ((now.getDay() + 6) % 7) * dayMS
  if (ms >= weekStart) return 'week'
  return 'older'
}

// txTitle names a row: past tense for the common types, the generic type
// label otherwise.
function txTitle (tx: WalletTransaction, ticker: string): string {
  switch (tx.type) {
    case TxType.Send: return `${t('Sent')} ${ticker}`
    case TxType.Receive: return `${t('Received')} ${ticker}`
    case TxType.Swap: return `${t('Swapped')} ${ticker}`
    default: return `${txTypeLabel(tx.type)} ${ticker}`
  }
}

function txIcon (type: number): string {
  if (type === TxType.Swap || type === TxType.SwapOrSend) return 'ico-swap'
  if (txIsIncoming(type)) return 'ico-download'
  return 'ico-upload'
}

// txStatus renders confirmation progress while confirming, then age.
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
  const { tx, ta, variant } = item
  const open = () => app.setForm({ form: 'txdetail', data: { assetID: variant.assetID, tx } })

  const sign = txAmountSign(tx.type)
  const amountText = txShowsAmount(tx.type)
    ? `${sign}${Doc.formatCoinValue(tx.amount, variant.ui)} ${variant.ticker}`
    : '—'
  const rate = app.tickerRate(ta.ticker) ?? 0
  const usdText = txShowsAmount(tx.type) && rate > 0
    ? Doc.usdString(tx.amount, variant.ui.conventional.conversionFactor, rate)
    : ''
  const parts: string[] = []
  if (tx.recipient) parts.push(`${t('to')} ${Doc.shortenAddress(tx.recipient)}`)
  parts.push(txStatus(tx))
  const unconfirmed = !tx.confirmed

  return (
    <div
      className={`activity-row d-flex align-items-center pointer ${unconfirmed ? 'activity-row-unconfirmed' : ''}`}
      onClick={open}
    >
      <span className={`tx-type-icon flex-center me-3 ${txIcon(tx.type)}`} />
      <div className="flex-grow-1">
        <div className="fs14 demi">{txTitle(tx, variant.ticker)}</div>
        <div className="grey fs12 mono">{parts.join(' · ')}</div>
      </div>
      <div className="text-right">
        <div className={`fs14 mono ${txIsIncoming(tx.type) ? 'tx-amt-pos' : ''}`}>{amountText}</div>
        {usdText !== '' && <div className="grey fs12 mono">{usdText}</div>}
      </div>
    </div>
  )
}

// Per-ticker history source. The ticker endpoint serves a ticker group's
// networks merged and time-ordered from the master txdb's ticker index; the
// Activity feed merges the per-ticker streams client-side. (A wallet keeping
// its own history store rather than the master txdb — only xmr today — is
// not visible to the ticker index and so not to this feed.)
interface TxSource {
  ta: TickerAsset
  txs: WalletTransaction[]
  more: boolean
}

export default function Activity () {
  const [, setTick] = useState(0)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [filter, setFilter] = useState<Filter>('all')
  const [query, setQuery] = useState('')
  const [page, setPage] = useState(0)
  const sourcesRef = useRef<Map<string, TxSource>>(new Map())
  const fetchGenRef = useRef(0)

  const redraw = () => setTick(n => n + 1)

  const activeTickers = (): TickerAsset[] =>
    (app.tickerList ?? []).filter(ta => ta.variants.some(isVariantActive))

  const fetchInitial = async () => {
    const gen = ++fetchGenRef.current
    const next = new Map<string, TxSource>()
    await Promise.all(activeTickers().map(async ta => {
      const r = await postJSON('/api/txhistoryticker', { ticker: ta.ticker, n: PAGE_SIZE, past: true })
      if (!r.ok) return
      next.set(ta.ticker, { ta, txs: r.txs ?? [], more: Boolean(r.moreAvailable) })
    }))
    if (gen !== fetchGenRef.current) return // superseded
    sourcesRef.current = next
    setLoading(false)
    redraw()
  }

  const loadMore = async () => {
    const gen = fetchGenRef.current
    setLoadingMore(true)
    await Promise.all(Array.from(sourcesRef.current.values()).filter(s => s.more).map(async s => {
      const last = s.txs[s.txs.length - 1]
      if (!last) return
      const v = variantForTx(s.ta, last)
      if (!v) return
      const r = await postJSON('/api/txhistoryticker', {
        ticker: s.ta.ticker,
        n: PAGE_SIZE,
        refID: last.id,
        refNetwork: networkID(v),
        past: true
      })
      if (!r.ok || gen !== fetchGenRef.current) return
      s.txs = s.txs.concat(r.txs ?? [])
      s.more = Boolean(r.moreAvailable)
    }))
    if (gen === fetchGenRef.current) {
      setLoadingMore(false)
      redraw()
    }
  }

  useEffect(() => {
    fetchInitial()
    const noteK = app.registerNoteFeed((n: CoreNote) => {
      if (n.type !== 'walletnote') return
      const payload = (n as WalletNote).payload
      if (payload?.route === 'transaction') fetchInitial()
    })
    return () => app.unregisterNoteFeed(noteK)
  }, [])

  // computeFiltered merges the per-ticker streams, sorts newest-first, and
  // applies the pill and txid filters. Reads sourcesRef so next() can
  // recompute after fetches without waiting on a render.
  const computeFiltered = (): TxItem[] => {
    const items: TxItem[] = []
    for (const s of Array.from(sourcesRef.current.values())) {
      for (const tx of s.txs) {
        const variant = variantForTx(s.ta, tx)
        if (variant) items.push({ tx, ta: s.ta, variant })
      }
    }
    items.sort((a, b) => effectiveTime(b.tx) - effectiveTime(a.tx))
    const q = query.trim().toLowerCase()
    return items.filter(item => {
      // A transaction-ID search overrides the type pills: the searcher wants
      // that transaction, whatever its type.
      if (q !== '') return item.tx.id.toLowerCase().includes(q)
      return filter === 'all' || filterTypes[filter].has(item.tx.type)
    })
  }

  const anyMore = () => Array.from(sourcesRef.current.values()).some(s => s.more)

  // nextPage advances the pager, fetching more from the sources first when
  // the local buffer cannot fill the next page (filters can consume whole
  // fetch batches, hence the bounded loop rather than a single fetch).
  const nextPage = async () => {
    const needed = (curPage + 1) * PAGE_ROWS + 1
    for (let i = 0; i < 10 && computeFiltered().length < needed && anyMore(); i++) {
      await loadMore()
    }
    if (computeFiltered().length > (curPage + 1) * PAGE_ROWS) setPage(curPage + 1)
  }

  const filtered = computeFiltered()
  const totalItems = Array.from(sourcesRef.current.values()).reduce((n, s) => n + s.txs.length, 0)
  // Clamp against shrinkage (filter changes, refetches) rather than tracking
  // resets in every state setter.
  const maxPage = Math.max(0, Math.ceil(filtered.length / PAGE_ROWS) - 1)
  const curPage = Math.min(page, maxPage)
  const pageItems = filtered.slice(curPage * PAGE_ROWS, (curPage + 1) * PAGE_ROWS)
  const hasNext = filtered.length > (curPage + 1) * PAGE_ROWS || anyMore()

  const buckets = new Map<Section, TxItem[]>()
  for (const item of pageItems) {
    const key = sectionFor(item.tx)
    const list = buckets.get(key)
    if (list) list.push(item)
    else buckets.set(key, [item])
  }

  return (
    <div className="fill-abs overflow-y-auto">
      <div className="activity-inner">
        <div className="d-flex align-items-center justify-content-between pb-2">
          <div className="d-flex align-items-center form-input-bg br5 activity-search">
            <span className="ico-search fs14 p-2" />
            <input
              type="text"
              value={query}
              placeholder={t('Search transaction ID')}
              onChange={e => { setQuery(e.target.value); setPage(0) }}
            />
          </div>
          <div className="d-flex align-items-center">
            {filters.map(f => (
              <button
                key={f.key}
                type="button"
                className={`filter-pill ms-2 ${filter === f.key ? 'selected' : ''}`}
                onClick={() => { setFilter(f.key); setPage(0) }}
              >
                {t(f.label)}
              </button>
            ))}
          </div>
        </div>

        {loading && <div className="flex-center py-5"><Spinner size={24} /></div>}

        {!loading && filtered.length === 0 && (
          <div className="text-center grey py-5">
            <span className="ico-bell fs28 d-block mb-3" />
            {totalItems === 0 ? t('No transactions yet.') : t('No transactions found.')}
          </div>
        )}

        {!loading && sections.map(({ key, label }) => {
          const list = buckets.get(key)
          if (!list) return null
          return (
            <div key={key}>
              <div className="activity-section-label">{t(label)}</div>
              {list.map(item => <TxRow key={`${item.variant.assetID}:${item.tx.id}`} item={item} />)}
            </div>
          )
        })}

        {!loading && (curPage > 0 || hasNext) && (
          <div className="flex-center pt-3">
            <button type="button" className="pager-btn flex-center" aria-label={t('Previous page')}
              onClick={() => setPage(curPage - 1)} disabled={curPage === 0 || loadingMore}>
              <span className="ico-chevron-left fs12" />
            </button>
            <span className="grey fs13 mx-3">{tvar('Page {{ n }}', { n: String(curPage + 1) })}</span>
            <button type="button" className="pager-btn flex-center" aria-label={t('Next page')}
              onClick={nextPage} disabled={!hasNext || loadingMore}>
              {loadingMore ? <Spinner size={12} /> : <span className="ico-chevron-left fs12 mirror-x" />}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
