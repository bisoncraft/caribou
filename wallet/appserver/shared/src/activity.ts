import { postJSON } from './http'
import app from './application'
import { WalletTransaction } from './registry'
import { TickerAsset, isVariantActive, networkID } from './assets'
import { TxType, TxRowItem, variantForTx } from './tx'
import { IntentsSwap, swapIsActive, relateTxToSwaps } from './intents'

// The Activity feed merges each active ticker's transaction history (served
// per ticker, time-ordered, by /api/txhistoryticker) with the persisted
// cross-chain swap records, newest first. The helpers here are the
// framework-free core shared by the desktop and mobile Activity views.

export type ActivityFilter = 'all' | 'sent' | 'received' | 'swapped'

// The filter pills partition the common types; everything exotic (approvals,
// bridges, tickets, mixes, ...) shows only under All.
const filterTypes: Record<Exclude<ActivityFilter, 'all'>, Set<number>> = {
  sent: new Set<number>([TxType.Send, TxType.SelfSend, TxType.SwapOrSend]),
  received: new Set<number>([TxType.Receive, TxType.Redeem, TxType.Refund]),
  swapped: new Set<number>([TxType.Swap])
}

// English filter labels, translated at render.
export const activityFilters: { key: ActivityFilter, label: string }[] = [
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

// swapTime is the ordering timestamp for a swap in the Activity feed, in
// seconds (matching WalletTransaction.timestamp).
export function swapTime (s: IntentsSwap): number {
  return Math.floor(new Date(s.createdAt).getTime() / 1000)
}

// FeedEntry is one row of the merged feed: an on-chain transaction or a
// completed cross-chain swap record. time carries the entry's ordering and
// bucketing timestamp in seconds.
export interface FeedEntry {
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

export type Section = 'today' | 'yesterday' | 'week' | 'older'

// English section labels, translated at render.
export const sections: { key: Section, label: string }[] = [
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

// sectionBounds computes the local calendar boundaries (today, yesterday,
// and the current Monday-based week) once per bucketing pass.
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

// bucketBySection groups feed entries (already newest first) by section,
// preserving order within each.
export function bucketBySection (entries: FeedEntry[]): Map<Section, FeedEntry[]> {
  const buckets = new Map<Section, FeedEntry[]>()
  const bounds = sectionBounds()
  for (const entry of entries) {
    const key = sectionFor(entry.time, bounds)
    const list = buckets.get(key)
    if (list) list.push(entry)
    else buckets.set(key, [entry])
  }
  return buckets
}

// TxSource is one ticker's loaded history. The ticker endpoint serves a
// ticker group's networks merged and time-ordered from the master txdb's
// ticker index; the feed merges the per-ticker streams client-side. (A wallet
// keeping its own history store rather than the master txdb, only xmr today,
// is not visible to the ticker index and so not to this feed.)
export interface TxSource {
  ta: TickerAsset
  txs: WalletTransaction[]
  more: boolean
}

// activeTickers lists the ticker groups with at least one active network.
function activeTickers (): TickerAsset[] {
  return (app.tickerList ?? []).filter(ta => ta.variants.some(isVariantActive))
}

// fetchSources loads the first batch of every active ticker's history.
export async function fetchSources (batch: number): Promise<Map<string, TxSource>> {
  const next = new Map<string, TxSource>()
  await Promise.all(activeTickers().map(async ta => {
    const r = await postJSON('/api/txhistoryticker', { ticker: ta.ticker, n: batch, past: true })
    if (!r.ok) return
    next.set(ta.ticker, { ta, txs: r.txs ?? [], more: Boolean(r.moreAvailable) })
  }))
  return next
}

// loadMoreSources fetches the next batch for every source with more history,
// appending in place. isCurrent is checked before each batch is applied, so a
// superseding refetch can discard in-flight batches.
export async function loadMoreSources (sources: Map<string, TxSource>, batch: number, isCurrent: () => boolean): Promise<void> {
  await Promise.all(Array.from(sources.values()).filter(s => s.more).map(async s => {
    const last = s.txs[s.txs.length - 1]
    if (!last) return
    const v = variantForTx(s.ta, last)
    if (!v) return
    const r = await postJSON('/api/txhistoryticker', {
      ticker: s.ta.ticker,
      n: batch,
      refID: last.id,
      refNetwork: networkID(v),
      past: true
    })
    if (!r.ok || !isCurrent()) return
    s.txs = s.txs.concat(r.txs ?? [])
    s.more = Boolean(r.moreAvailable)
  }))
}

// anyMore reports whether any source has unfetched history.
export function anyMore (sources: Map<string, TxSource>): boolean {
  return Array.from(sources.values()).some(s => s.more)
}

// watermark is the completeness boundary of the merged feed: the newest
// oldest-loaded confirmed timestamp among sources with more history
// server-side. Each source's loaded batch reaches back only to its own
// boundary, so the merge is known-complete only for items at or above the
// newest boundary. Below it, a deeper source may still hold unfetched items
// that belong earlier in the feed, and showing loaded items there would
// misorder the feed and shift rows when the gap is later fetched. A source
// whose entire loaded batch is mempool (oldest has no timestamp) contributes
// no confirmed coverage and is skipped rather than blanking the whole feed.
function watermark (sources: Map<string, TxSource>): number {
  let wm = 0
  for (const s of Array.from(sources.values())) {
    if (!s.more || s.txs.length === 0) continue
    const oldest = s.txs[s.txs.length - 1]
    if (oldest.timestamp === 0) continue
    wm = Math.max(wm, oldest.timestamp)
  }
  return wm
}

// mergeFeed merges the per-ticker streams and the terminal swap records,
// sorts newest first, and applies the watermark, filter, and txid query.
// Active swaps are excluded; views pin them above the feed.
export function mergeFeed (sources: Map<string, TxSource>, swaps: IntentsSwap[], filter: ActivityFilter, query: string): FeedEntry[] {
  const entries: FeedEntry[] = []
  for (const s of Array.from(sources.values())) {
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
  // A transaction-ID search overrides the type filter and the watermark: the
  // searcher wants that transaction, whatever its type or position.
  if (q !== '') {
    return entries.filter(e => e.tx ? e.tx.tx.id.toLowerCase().includes(q) : swapMatchesQuery(e.swap as IntentsSwap, q))
  }
  const wm = watermark(sources)
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

// mergeSwaps folds a fetched swap list into the current one. The initial
// fetch races the note feed: a live intentsswap upsert can land while the
// GET is in flight, and a blind replace would revert the record to the
// fetch's stale snapshot. Existing (note-delivered) entries win.
export function mergeSwaps (prev: IntentsSwap[], fetched: IntentsSwap[]): IntentsSwap[] {
  const have = new Set(prev.map(s => s.depositAddress))
  return [...prev, ...fetched.filter(s => !have.has(s.depositAddress))]
}

// upsertSwap places an updated swap record (from an intentsswap note) first.
export function upsertSwap (prev: IntentsSwap[], s: IntentsSwap): IntentsSwap[] {
  return [s, ...prev.filter(x => x.depositAddress !== s.depositAddress)]
}
