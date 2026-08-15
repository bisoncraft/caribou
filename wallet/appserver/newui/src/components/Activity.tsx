import { useEffect, useRef, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { postJSON } from '@caribou/shared/http'
import { useForceUpdate } from '@caribou/shared/hooks'
import { CoreNote, WalletNote, WalletTransaction } from '@caribou/shared/registry'
import { TickerAsset, isVariantActive, networkID } from '@caribou/shared/assets'
import { TxType } from '@caribou/shared/tx'
import Spinner from './Spinner'
import TxRow, { TxRowItem, variantForTx } from './TxRow'
import SwapRow, { swapTime } from './SwapRow'
import { IntentsSwap, swapIsActive, fetchSwaps, relateTxToSwaps } from '../intents'

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

// effectiveTime orders the merged feed; mempool transactions (timestamp 0)
// sort newest.
function effectiveTime (tx: WalletTransaction): number {
  return tx.timestamp === 0 ? Number.MAX_SAFE_INTEGER : tx.timestamp
}

// FeedEntry is one row of the merged feed: an on-chain transaction or a
// completed cross-chain swap record. time carries the entry's ordering and
// bucketing timestamp in seconds.
interface FeedEntry {
  time: number
  tx?: TxRowItem
  swap?: IntentsSwap
}

// swapMatchesQuery reports whether a transaction-ID search hits any of the
// swap's identifying strings: its deposit address or any leg's txid.
function swapMatchesQuery (s: IntentsSwap, q: string): boolean {
  const ids = [s.depositAddress, s.depositTxID ?? '', ...(s.destinationTxs ?? []), ...(s.refundTxs ?? [])]
  return ids.some(id => id.toLowerCase().includes(q))
}

type Section = 'today' | 'yesterday' | 'week' | 'older'

const sections: { key: Section, label: string }[] = [
  { key: 'today', label: 'Today' },
  { key: 'yesterday', label: 'Yesterday' },
  { key: 'week', label: 'Earlier this week' },
  { key: 'older', label: 'Older' }
]

interface SectionBounds {
  todayStart: number
  yesterdayStart: number
  weekStart: number
}

// sectionBounds computes the local calendar boundaries — today, yesterday,
// and the current (Monday-based) week — once per bucketing pass.
function sectionBounds (): SectionBounds {
  const now = new Date()
  const dayMS = 86400 * 1000
  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
  return {
    todayStart,
    yesterdayStart: todayStart - dayMS,
    weekStart: todayStart - ((now.getDay() + 6) % 7) * dayMS
  }
}

// sectionFor buckets a feed timestamp (seconds) by local calendar time.
// Mempool transactions carry MAX_SAFE_INTEGER (see effectiveTime) and so
// land in today.
function sectionFor (time: number, bounds: SectionBounds): Section {
  const ms = time * 1000
  if (ms >= bounds.todayStart) return 'today'
  if (ms >= bounds.yesterdayStart) return 'yesterday'
  if (ms >= bounds.weekStart) return 'week'
  return 'older'
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
  const redraw = useForceUpdate()
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [filter, setFilter] = useState<Filter>('all')
  const [query, setQuery] = useState('')
  const [page, setPage] = useState(0)
  const sourcesRef = useRef<Map<string, TxSource>>(new Map())
  const fetchGenRef = useRef(0)
  // Cross-chain swap records. The full list is small and served whole, so
  // it lives in state rather than the paged per-ticker source map.
  const [swaps, setSwaps] = useState<IntentsSwap[]>([])

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
    try {
      // The gen checks gate only the data application: a superseding refetch
      // mid-flight discards these batches, but loadingMore must clear
      // regardless or the pager wedges disabled.
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
    } finally {
      setLoadingMore(false)
      redraw()
    }
  }

  const loadSwaps = async () => setSwaps(await fetchSwaps())

  useEffect(() => {
    fetchInitial()
    loadSwaps()
    // Transaction notes arrive in bursts (syncing wallets, rescans), and
    // every refetch costs one request per active ticker, so a trailing
    // debounce coalesces a burst into a single refetch.
    let debounce = 0
    const noteK = app.registerNoteFeed((n: CoreNote) => {
      if (n.type === 'intentsswap') {
        // The note carries the full updated record; upsert it in place.
        const s = (n as CoreNote & { swap: IntentsSwap }).swap
        if (s) {
          setSwaps(prev => [s, ...prev.filter(x => x.depositAddress !== s.depositAddress)])
        }
        return
      }
      if (n.type !== 'walletnote') return
      const payload = (n as WalletNote).payload
      if (payload?.route !== 'transaction') return
      window.clearTimeout(debounce)
      debounce = window.setTimeout(fetchInitial, 300)
    })
    return () => {
      window.clearTimeout(debounce)
      app.unregisterNoteFeed(noteK)
    }
  }, [])

  // watermark is the completeness boundary of the merged feed: the newest
  // oldest-loaded confirmed timestamp among sources with more history
  // server-side. Each source's loaded batch reaches back only to its own
  // boundary, so the merge is known-complete only for items at or above the
  // newest boundary — below it, a deeper source may still hold unfetched
  // items that belong earlier in the feed, and showing loaded items there
  // would misorder the feed and shift rows between pages when the gap is
  // later fetched. A source whose entire loaded batch is mempool (oldest has
  // no timestamp) contributes no confirmed coverage and is skipped rather
  // than blanking the whole feed.
  const watermark = (): number => {
    let wm = 0
    for (const s of Array.from(sourcesRef.current.values())) {
      if (!s.more || s.txs.length === 0) continue
      const oldest = s.txs[s.txs.length - 1]
      if (oldest.timestamp === 0) continue
      wm = Math.max(wm, oldest.timestamp)
    }
    return wm
  }

  // computeFiltered merges the per-ticker streams and the terminal swap
  // records, sorts newest-first, and applies the watermark, pill, and txid
  // filters. Reads sourcesRef so nextPage can recompute after fetches
  // without waiting on a render. Active swaps are excluded — they render in
  // the pinned section above the feed.
  const computeFiltered = (): FeedEntry[] => {
    const entries: FeedEntry[] = []
    for (const s of Array.from(sourcesRef.current.values())) {
      for (const tx of s.txs) {
        const variant = variantForTx(s.ta, tx)
        if (!variant) continue
        entries.push({ time: effectiveTime(tx), tx: { tx, ta: s.ta, variant, swapRelation: relateTxToSwaps(swaps, tx) } })
      }
    }
    for (const s of swaps) {
      if (!swapIsActive(s)) entries.push({ time: swapTime(s), swap: s })
    }
    entries.sort((a, b) => b.time - a.time)
    const q = query.trim().toLowerCase()
    // A transaction-ID search overrides the type pills and the watermark:
    // the searcher wants that transaction, whatever its type or position.
    if (q !== '') {
      return entries.filter(e => e.tx ? e.tx.tx.id.toLowerCase().includes(q) : swapMatchesQuery(e.swap as IntentsSwap, q))
    }
    const wm = watermark()
    return entries.filter(e => {
      if (e.swap) {
        if (swapTime(e.swap) < wm) return false
        return filter === 'all' || filter === 'swapped'
      }
      const tx = (e.tx as TxRowItem).tx
      if (tx.timestamp !== 0 && tx.timestamp < wm) return false
      return filter === 'all' || filterTypes[filter].has(tx.type)
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

  const buckets = new Map<Section, FeedEntry[]>()
  const bounds = sectionBounds()
  for (const entry of pageItems) {
    const key = sectionFor(entry.time, bounds)
    const list = buckets.get(key)
    if (list) list.push(entry)
    else buckets.set(key, [entry])
  }

  // Active swaps pin above the date-bucketed feed whenever their pill is
  // selected; a txid search hides them like everything else it misses.
  const showActive = query.trim() === '' && (filter === 'all' || filter === 'swapped')
  const activeSwaps = showActive ? swaps.filter(swapIsActive) : []

  return (
    <div className="fill-abs overflow-y-auto">
      <div className="view-inner mx-auto w-100">
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
                className={`filter-pill fs13 grey ms-2 ${filter === f.key ? 'selected' : ''}`}
                onClick={() => { setFilter(f.key); setPage(0) }}
              >
                {t(f.label)}
              </button>
            ))}
          </div>
        </div>

        {loading && <div className="flex-center py-5"><Spinner size={24} /></div>}

        {!loading && filtered.length === 0 && activeSwaps.length === 0 && (
          <div className="text-center grey py-5">
            <span className="ico-bell fs28 d-block mb-3" />
            {totalItems === 0 ? t('No transactions yet.') : t('No transactions found.')}
          </div>
        )}

        {!loading && activeSwaps.length > 0 && (
          <div>
            <div className="section-label fs11 grey demi pt-3 pb-2">{t('In progress')}</div>
            {activeSwaps.map(s => <SwapRow key={s.depositAddress} swap={s} />)}
          </div>
        )}

        {!loading && sections.map(({ key, label }) => {
          const list = buckets.get(key)
          if (!list) return null
          return (
            <div key={key}>
              <div className="section-label fs11 grey demi pt-3 pb-2">{t(label)}</div>
              {list.map(entry => entry.tx
                ? <TxRow key={`${entry.tx.variant.assetID}:${entry.tx.tx.id}`} item={entry.tx} />
                : <SwapRow key={(entry.swap as IntentsSwap).depositAddress} swap={entry.swap as IntentsSwap} />)}
            </div>
          )
        })}

        {!loading && (curPage > 0 || hasNext) && (
          <div className="flex-center pt-3">
            <button type="button" className="pager-btn icon-32 p-0 flex-center" aria-label={t('Previous page')}
              onClick={() => setPage(curPage - 1)} disabled={curPage === 0 || loadingMore}>
              <span className="ico-chevron-left fs12" />
            </button>
            <span className="grey fs13 mx-3">{tvar('Page {{ n }}', { n: String(curPage + 1) })}</span>
            <button type="button" className="pager-btn icon-32 p-0 flex-center" aria-label={t('Next page')}
              onClick={nextPage} disabled={!hasNext || loadingMore}>
              {loadingMore ? <Spinner size={12} /> : <span className="ico-chevron-left fs12 mirror-x" />}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
