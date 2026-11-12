import { useEffect, useRef, useState } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { useForceUpdate } from '@caribou/shared/hooks'
import { CoreNote, WalletNote } from '@caribou/shared/registry'
import { TxRowItem } from '@caribou/shared/tx'
import { IntentsSwap, swapIsActive, fetchSwaps } from '@caribou/shared/intents'
import {
  ActivityFilter, TxSource, activityFilters, sections, bucketBySection, fetchSources, loadMoreSources,
  anyMore, mergeFeed, mergeSwaps, upsertSwap
} from '@caribou/shared/activity'
import { Icon, Spinner } from './ui'
import { TxRow, SwapRow, TxDetailSheet } from './TxRow'
import RecoverSwapsSheet from './RecoverSwaps'

// Per-ticker fetch batch.
const BATCH = 20
// Rows revealed per scroll step. The feed renders the merged list up to this
// count, growing it as the end of the list scrolls into view.
const STEP = 20
// Bound on fetch rounds per scroll step: filters can consume whole batches.
const MAX_ROUNDS = 10
// Cap on the per-ticker depth a refetch preserves.
const MAX_DEPTH = 200

// Activity is the transaction feed: every active ticker's history merged with
// cross-chain swap records, newest first, filterable by type and searchable
// by transaction ID. In-flight swaps pin above the feed. More history loads
// as the end of the list scrolls into view.
export default function Activity () {
  const redraw = useForceUpdate()
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [filter, setFilter] = useState<ActivityFilter>('all')
  const [query, setQuery] = useState('')
  const [shown, setShown] = useState(STEP)
  const [swaps, setSwaps] = useState<IntentsSwap[]>([])
  const [detail, setDetail] = useState<TxRowItem | null>(null)
  const [recovering, setRecovering] = useState(false)
  const sourcesRef = useRef<Map<string, TxSource>>(new Map())
  const fetchGenRef = useRef(0)
  const loadingMoreRef = useRef(false)
  const sentinelRef = useRef<HTMLDivElement | null>(null)

  // stalledRef stops automatic loading after a round that adds nothing: a
  // source that keeps failing still reports more history, and the scroll
  // sentinel would otherwise re-arm into an endless request loop. A refetch
  // clears it.
  const stalledRef = useRef(false)

  // fetchInitial (re)loads every source. A refetch triggered by a new
  // transaction keeps the depth already scrolled into, so the list does not
  // collapse under the reader.
  const fetchInitial = async () => {
    const gen = ++fetchGenRef.current
    const depth = Math.max(BATCH, ...Array.from(sourcesRef.current.values()).map(src => src.txs.length))
    const next = await fetchSources(Math.min(depth, MAX_DEPTH))
    if (gen !== fetchGenRef.current) return // superseded
    sourcesRef.current = next
    stalledRef.current = false
    setLoading(false)
    redraw()
  }

  useEffect(() => {
    fetchInitial()
    fetchSwaps().then(fetched => setSwaps(prev => mergeSwaps(prev, fetched)))
    // Transaction notes arrive in bursts (syncing wallets, rescans), and every
    // refetch costs one request per active ticker, so a trailing debounce
    // coalesces a burst into a single refetch.
    let debounce = 0
    const noteK = app.registerNoteFeed((n: CoreNote) => {
      if (n.type === 'intentsswap') {
        const s = (n as CoreNote & { swap: IntentsSwap }).swap
        if (s) setSwaps(prev => upsertSwap(prev, s))
        return
      }
      if (n.type !== 'walletnote') return
      if ((n as WalletNote).payload?.route !== 'transaction') return
      window.clearTimeout(debounce)
      debounce = window.setTimeout(fetchInitial, 300)
    })
    return () => {
      window.clearTimeout(debounce)
      app.unregisterNoteFeed(noteK)
    }
  }, [])

  const feed = mergeFeed(sourcesRef.current, swaps, filter, query)
  const hasMore = feed.length > shown || (anyMore(sourcesRef.current) && !stalledRef.current)

  const loadedCount = () => Array.from(sourcesRef.current.values()).reduce((n, src) => n + src.txs.length, 0)

  // growFeed reveals the next step of rows, fetching more history first when
  // the loaded feed cannot fill it.
  const growFeed = async () => {
    if (loadingMoreRef.current) return
    loadingMoreRef.current = true
    setLoadingMore(true)
    const target = shown + STEP
    const gen = fetchGenRef.current
    try {
      for (let i = 0; i < MAX_ROUNDS && mergeFeed(sourcesRef.current, swaps, filter, query).length < target && anyMore(sourcesRef.current); i++) {
        const before = loadedCount()
        await loadMoreSources(sourcesRef.current, BATCH, () => gen === fetchGenRef.current)
        if (gen !== fetchGenRef.current) break
        if (loadedCount() === before) {
          stalledRef.current = true
          break
        }
      }
    } finally {
      loadingMoreRef.current = false
      setLoadingMore(false)
      setShown(target)
    }
  }

  // Grow the feed when the sentinel below the last row scrolls into view.
  const growRef = useRef(growFeed)
  growRef.current = growFeed
  useEffect(() => {
    const el = sentinelRef.current
    if (!el || !hasMore || loading) return
    const io = new IntersectionObserver(entries => {
      if (entries.some(e => e.isIntersecting)) growRef.current()
    }, { rootMargin: '200px' })
    io.observe(el)
    return () => io.disconnect()
  }, [hasMore, loading, shown, filter, query])

  const resetPaging = () => setShown(STEP)

  const buckets = bucketBySection(feed.slice(0, shown))
  const showActive = query.trim() === '' && (filter === 'all' || filter === 'swapped')
  const activeSwaps = showActive ? swaps.filter(swapIsActive) : []
  const totalItems = loadedCount()

  return (
    <div className='scroll'>
      <div className='page'>
        <h1 className='page-title mb14'>{t('Activity')}</h1>
        <label className='field search'>
          <Icon name='search' />
          <input
            type='search'
            placeholder={t('Search transaction ID')}
            aria-label={t('Search transaction ID')}
            value={query}
            onChange={e => { setQuery(e.target.value); resetPaging() }}
            autoCapitalize='off'
            autoCorrect='off'
            spellCheck={false}
          />
        </label>
        <div className='pills' role='group' aria-label={t('Filter')}>
          {activityFilters.map(f => (
            <button
              key={f.key}
              type='button'
              className={filter === f.key ? 'fp on' : 'fp'}
              aria-pressed={filter === f.key}
              onClick={() => { setFilter(f.key); resetPaging() }}
            >
              {t(f.label)}
            </button>
          ))}
        </div>

        {loading && <div className='empty'><Spinner large /></div>}

        {!loading && feed.length === 0 && activeSwaps.length === 0 && (
          <div className='empty'>
            <Icon name='bell' className='empty-ico' />
            <div className='mt12'>{totalItems === 0 ? t('No transactions yet.') : t('No transactions found.')}</div>
          </div>
        )}

        {!loading && activeSwaps.length > 0 && (
          <>
            <h2 className='lbl sec-sm'>{t('In progress')}</h2>
            <div className='stack'>{activeSwaps.map(s => <SwapRow key={s.depositAddress} swap={s} />)}</div>
          </>
        )}

        {!loading && sections.map(({ key, label }) => {
          const list = buckets.get(key)
          if (!list) return null
          return (
            <div key={key}>
              <h2 className='lbl sec-sm'>{t(label)}</h2>
              <div className='stack'>
                {list.map(entry => entry.tx
                  ? <TxRow key={`${entry.tx.variant.assetID}:${entry.tx.tx.id}`} item={entry.tx} onOpen={setDetail} showNetwork />
                  : <SwapRow key={(entry.swap as IntentsSwap).depositAddress} swap={entry.swap as IntentsSwap} />)}
              </div>
            </div>
          )
        })}

        {/* Swap-record recovery lives under the Swapped filter, which is
            where a user looks for swaps missing after a seed restore. */}
        {!loading && filter === 'swapped' && !hasMore && (
          <div className='center pt16'>
            <button type='button' className='btn quiet inline' onClick={() => setRecovering(true)}>{t('Recover swaps')}</button>
          </div>
        )}

        {!loading && hasMore && (
          <div ref={sentinelRef} className='empty'>{loadingMore && <Spinner />}</div>
        )}
      </div>

      {detail && <TxDetailSheet assetID={detail.variant.assetID} tx={detail.tx} onClose={() => setDetail(null)} />}
      {recovering && <RecoverSwapsSheet onClose={() => setRecovering(false)} />}
    </div>
  )
}
