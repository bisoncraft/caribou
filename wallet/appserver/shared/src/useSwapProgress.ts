import { useEffect, useState } from 'react'
import app from './application'
import { getJSON, postJSON } from './http'
import { CoreNote, WalletTransaction } from './registry'
import { IntentsSwap } from './intents'
import { fetchWalletTx } from './swap'

// seeded holds the record most recently handed over by seedSwapRecord, so a
// progress view opened right after an execute renders immediately instead of
// waiting on the record fetch.
let seeded: IntentsSwap | null = null

// seedSwapRecord hands a freshly executed swap to the next useSwapRecord
// mounted for its deposit address. The hook still fetches the persisted
// record and follows live updates.
export function seedSwapRecord (swap: IntentsSwap): void {
  seeded = swap
}

// newerSwap returns whichever of two versions of a swap record was updated
// later, preferring the incoming one on a tie. A slow record fetch must not
// revert a newer record already delivered by a note or a seed.
function newerSwap (prev: IntentsSwap | null, next: IntentsSwap): IntentsSwap {
  if (!prev || prev.depositAddress !== next.depositAddress) return next
  const p = Date.parse(prev.updatedAt)
  const n = Date.parse(next.updatedAt)
  return Number.isFinite(p) && Number.isFinite(n) && n < p ? prev : next
}

// useSwapRecord tracks one cross-chain swap record. With a depositAddress it
// loads the persisted record; either way it applies live 'intentsswap' note
// updates to the record on display, and refresh asks the engine to re-poll
// the service for it. setSwap installs a record directly (e.g. the result
// of an execute).
export function useSwapRecord (depositAddress?: string) {
  const [swap, setSwap] = useState<IntentsSwap | null>(() =>
    depositAddress && seeded?.depositAddress === depositAddress ? seeded : null)
  // loaded is true once the record lookup has finished (or none was asked
  // for); loadErr carries a failed lookup's message.
  const [loaded, setLoaded] = useState(!depositAddress)
  const [loadErr, setLoadErr] = useState('')

  // Opened on an existing swap: pull the record by deposit address.
  useEffect(() => {
    if (!depositAddress) return
    let stale = false
    const load = async () => {
      const r = await getJSON('/api/intents/swaps')
      if (stale) return
      setLoaded(true)
      if (!r.ok) {
        setLoadErr(r.msg || r.errMsg || '')
        return
      }
      const s = (r.swaps as IntentsSwap[] ?? []).find(s => s.depositAddress === depositAddress)
      if (s) setSwap(prev => newerSwap(prev, s))
    }
    load()
    return () => { stale = true }
  }, [depositAddress])

  // Live swap updates ride the notification feed; apply any that match the
  // record on display.
  useEffect(() => {
    const k = app.registerNoteFeed((n: CoreNote) => {
      if (n.type !== 'intentsswap') return
      const s = (n as CoreNote & { swap: IntentsSwap }).swap
      if (!s) return
      setSwap(prev => (prev && prev.depositAddress === s.depositAddress) ? newerSwap(prev, s) : prev)
    })
    return () => app.unregisterNoteFeed(k)
  }, [])

  const refresh = async () => {
    if (!swap) return
    const r = await postJSON('/api/intents/refresh', { depositAddress: swap.depositAddress })
    if (r.ok && r.swap) setSwap(prev => newerSwap(prev, r.swap))
  }

  return { swap, setSwap, loaded, loadErr, refresh }
}

// useDepositTx fetches a swap's deposit leg from the origin wallet's history
// (for its network fee and on-chain rejection status): undefined while
// loading, null when there is no deposit txid or the wallet has no record of
// it.
export function useDepositTx (originAssetID: number | undefined, depositTxID: string | undefined): WalletTransaction | null | undefined {
  const [depositTx, setDepositTx] = useState<WalletTransaction | null | undefined>(undefined)
  useEffect(() => {
    if (originAssetID === undefined || !depositTxID) {
      setDepositTx(null)
      return
    }
    let stale = false
    setDepositTx(undefined)
    fetchWalletTx(originAssetID, depositTxID).then(tx => {
      if (!stale) setDepositTx(tx)
    })
    return () => { stale = true }
  }, [originAssetID, depositTxID])
  return depositTx
}
