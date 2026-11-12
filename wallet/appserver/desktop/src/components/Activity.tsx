import { useEffect, useRef, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { useForceUpdate } from '@caribou/shared/hooks'
import { CoreNote, WalletNote } from '@caribou/shared/registry'
import Spinner from './Spinner'
import TxRow from './TxRow'
import SwapRow from './SwapRow'
import { IntentsSwap, swapIsActive, fetchSwaps } from '@caribou/shared/intents'
import {
  ActivityFilter, TxSource, activityFilters, sections, bucketBySection, fetchSources, loadMoreSources,
  anyMore as sourcesHaveMore, mergeFeed, mergeSwaps, upsertSwap
} from '@caribou/shared/activity'

const PAGE_SIZE = 20
// Rows shown per display page. Distinct from PAGE_SIZE, the per-source fetch
// batch: display pages slice the merged, filtered feed.
const PAGE_ROWS = 10

export default function Activity () {
  const redraw = useForceUpdate()
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [filter, setFilter] = useState<ActivityFilter>('all')
  const [query, setQuery] = useState('')
  const [page, setPage] = useState(0)
  const sourcesRef = useRef<Map<string, TxSource>>(new Map())
  const fetchGenRef = useRef(0)
  // Cross-chain swap records. The full list is small and served whole, so
  // it lives in state rather than the paged per-ticker source map.
  const [swaps, setSwaps] = useState<IntentsSwap[]>([])
  // The deposit address of the swap served by the hovered row, so every
  // row belonging to that swap can light up together.
  const [hoverSwap, setHoverSwap] = useState('')

  const fetchInitial = async () => {
    const gen = ++fetchGenRef.current
    const next = await fetchSources(PAGE_SIZE)
    if (gen !== fetchGenRef.current) return // superseded
    sourcesRef.current = next
    setLoading(false)
    redraw()
  }

  const loadMore = async () => {
    const gen = fetchGenRef.current
    setLoadingMore(true)
    try {
      // The gen check gates only the data application: a superseding refetch
      // mid-flight discards these batches, but loadingMore must clear
      // regardless or the pager wedges disabled.
      await loadMoreSources(sourcesRef.current, PAGE_SIZE, () => gen === fetchGenRef.current)
    } finally {
      setLoadingMore(false)
      redraw()
    }
  }

  // The initial fetch races the note feed: a live intentsswap upsert can
  // land while the GET is in flight, and a blind replace would revert the
  // record to the fetch's stale snapshot. Existing (note-delivered)
  // entries win the merge; the fetch only fills in what notes haven't
  // already delivered.
  const loadSwaps = async () => {
    const fetched = await fetchSwaps()
    setSwaps(prev => mergeSwaps(prev, fetched))
  }

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
        if (s) setSwaps(prev => upsertSwap(prev, s))
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

  // computeFiltered reads sourcesRef so nextPage can recompute after
  // fetches without waiting on a render.
  const computeFiltered = () => mergeFeed(sourcesRef.current, swaps, filter, query)

  const anyMore = () => sourcesHaveMore(sourcesRef.current)

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

  const buckets = bucketBySection(pageItems)

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
            {activityFilters.map(f => (
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
            {activeSwaps.map(s => (
              <SwapRow
                key={s.depositAddress} swap={s} onSwapHover={setHoverSwap}
                swapHighlight={hoverSwap !== '' && s.depositAddress === hoverSwap}
              />
            ))}
          </div>
        )}

        {!loading && sections.map(({ key, label }) => {
          const list = buckets.get(key)
          if (!list) return null
          return (
            <div key={key}>
              <div className="section-label fs11 grey demi pt-3 pb-2">{t(label)}</div>
              {list.map(entry => entry.tx
                ? (
                  <TxRow
                    key={`${entry.tx.variant.assetID}:${entry.tx.tx.id}`} item={entry.tx} onSwapHover={setHoverSwap}
                    swapHighlight={hoverSwap !== '' && entry.tx.swapRelation?.swap.depositAddress === hoverSwap}
                  />
                  )
                : (
                  <SwapRow
                    key={(entry.swap as IntentsSwap).depositAddress} swap={entry.swap as IntentsSwap} onSwapHover={setHoverSwap}
                    swapHighlight={hoverSwap !== '' && (entry.swap as IntentsSwap).depositAddress === hoverSwap}
                  />
                  ))}
            </div>
          )
        })}

        {/* Swap-record recovery entry point. Lives under the Swapped filter,
            which is where a post-seed-restore user looks for missing swaps. */}
        {!loading && filter === 'swapped' && (
          <div className="flex-center pt-3">
            <button type="button" className="small grey"
              onClick={() => app.setForm({ form: 'recoverswaps', data: {} })}>
              {t('Recover swaps')}
            </button>
          </div>
        )}

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
