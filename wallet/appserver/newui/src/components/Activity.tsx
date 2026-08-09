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

// sectionFor buckets a transaction by local calendar time. Mempool
// transactions count as today.
function sectionFor (tx: WalletTransaction, bounds: SectionBounds): Section {
  if (tx.timestamp === 0) return 'today'
  const ms = tx.timestamp * 1000
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

  useEffect(() => {
    fetchInitial()
    // Transaction notes arrive in bursts (syncing wallets, rescans), and
    // every refetch costs one request per active ticker, so a trailing
    // debounce coalesces a burst into a single refetch.
    let debounce = 0
    const noteK = app.registerNoteFeed((n: CoreNote) => {
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

  // computeFiltered merges the per-ticker streams, sorts newest-first, and
  // applies the watermark, pill, and txid filters. Reads sourcesRef so
  // nextPage can recompute after fetches without waiting on a render.
  const computeFiltered = (): TxRowItem[] => {
    const items: TxRowItem[] = []
    for (const s of Array.from(sourcesRef.current.values())) {
      for (const tx of s.txs) {
        const variant = variantForTx(s.ta, tx)
        if (variant) items.push({ tx, ta: s.ta, variant })
      }
    }
    items.sort((a, b) => effectiveTime(b.tx) - effectiveTime(a.tx))
    const q = query.trim().toLowerCase()
    // A transaction-ID search overrides the type pills and the watermark:
    // the searcher wants that transaction, whatever its type or position.
    if (q !== '') return items.filter(item => item.tx.id.toLowerCase().includes(q))
    const wm = watermark()
    return items.filter(item => {
      if (item.tx.timestamp !== 0 && item.tx.timestamp < wm) return false
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

  const buckets = new Map<Section, TxRowItem[]>()
  const bounds = sectionBounds()
  for (const item of pageItems) {
    const key = sectionFor(item.tx, bounds)
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
              <div className="section-label">{t(label)}</div>
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
