import { useEffect, useRef, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { useForceUpdate, useCoreSubscriptions } from '@caribou/shared/hooks'
import { ApprovalStatus, CoreNote, WalletNote, WalletTransaction } from '@caribou/shared/registry'
import { AssetVariant, networkAsset, networkID } from '@caribou/shared/assets'
import { TxRowItem, variantForTx } from '@caribou/shared/tx'
import { IntentsSwap, fetchIntentsAssets, fetchSwaps, relateTxToSwaps } from '@caribou/shared/intents'
import { mergeSwaps, upsertSwap } from '@caribou/shared/activity'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import NetworkedTokenLogo from '@caribou/shared/components/NetworkedTokenLogo'
import { useNav } from '../nav'
import { NavHeader, Spinner } from './ui'
import { AssetActions, syncProgress } from './Portfolio'
import { TxRow, TxDetailSheet } from './TxRow'

// Transactions fetched for the asset's recent list.
const TX_FETCH = 10
// Rows shown before "See all" (the Activity tab holds the full feed).
const TX_ROWS = 5

function variantTotal (v: AssetVariant): number {
  const { available, locked, immature } = v.bal
  return available + locked + immature
}

// AssetScreen is the detail view for one ticker group: balance, price and
// portfolio share, the Receive / Send / Swap actions, per-network balances,
// any unspendable balance breakdown, trust-line notices, and recent
// transactions.
export default function AssetScreen ({ ticker }: { ticker: string }) {
  const nav = useNav()
  const redraw = useForceUpdate()
  const [loadingTxns, setLoadingTxns] = useState(true)
  const [txs, setTxs] = useState<WalletTransaction[]>([])
  const [swaps, setSwaps] = useState<IntentsSwap[]>([])
  const [swappableIDs, setSwappableIDs] = useState<Set<number> | null>(null)
  const [detail, setDetail] = useState<TxRowItem | null>(null)
  const fetchGenRef = useRef(0)

  const ta = app.tickerMap?.[ticker]
  const assetIDs = ta ? ta.variants.filter(v => app.wallet(v.assetID)).map(v => v.assetID) : []
  const assetIDsKey = assetIDs.join(',')

  // A multi-network ticker uses the ticker endpoint, which the master txdb
  // serves merged and time-ordered. A single-network ticker uses the
  // per-asset endpoint, which also covers wallets that keep their own history
  // store rather than the master txdb (only xmr today).
  const fetchTxns = async () => {
    if (!ta) return
    const gen = ++fetchGenRef.current
    const r = ta.variants.length > 1
      ? await postJSON('/api/txhistoryticker', { ticker: ta.ticker, n: TX_FETCH, past: true })
      : await postJSON('/api/txhistory', { assetID: ta.variants[0].assetID, n: TX_FETCH, past: true })
    if (gen !== fetchGenRef.current) return
    setTxs(r.ok && r.txs ? r.txs : [])
    setLoadingTxns(false)
  }
  const fetchRef = useRef(fetchTxns)
  fetchRef.current = fetchTxns

  useEffect(() => { setLoadingTxns(true); fetchRef.current() }, [assetIDsKey])

  useEffect(() => {
    fetchSwaps().then(fetched => setSwaps(prev => mergeSwaps(prev, fetched)))
    // Asset IDs with a cross-chain swap route gate the Swap action. null
    // (loading or engine unavailable) stays optimistic.
    fetchIntentsAssets().then(list => { if (list) setSwappableIDs(new Set(list.map(a => a.assetID))) })
  }, [])

  useCoreSubscriptions({
    onBalance: redraw,
    onRate: redraw,
    onNote: (note) => {
      switch (note.type) {
        case 'balance':
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
          redraw()
          break
        case 'walletnote': {
          const n = note as WalletNote
          if (!assetIDs.includes(n.payload.assetID)) break
          redraw()
          if (n.payload.route === 'transaction') fetchRef.current()
          break
        }
        case 'intentsswap': {
          const s = (note as CoreNote & { swap: IntentsSwap }).swap
          if (s) setSwaps(prev => upsertSwap(prev, s))
          break
        }
      }
    }
  }, [assetIDsKey, redraw])

  if (!ta) {
    return (
      <div className='frame'>
        <NavHeader title={t('Unknown asset')} onBack={nav.back} />
        <div className='empty'>{t('Unknown asset')}</div>
      </div>
    )
  }

  const rate = ta.xcRate ?? 0
  const usdValue = ta.total * rate
  const totals = app.portfolioTotals()
  const portfolioPct = rate > 0 && totals.total > 0 ? usdValue / totals.total * 100 : null
  const swapAvailable = swappableIDs === null || ta.variants.some(v => swappableIDs.has(v.assetID))
  const hasUnavailable = ta.immature > 0 || ta.locked > 0
  const isMultiNet = ta.variants.length > 1
  const sync = syncProgress(ta)
  const balance = `${Doc.formatConventionalValue(ta.total, ta.precision)} ${ta.ticker}`

  const items: TxRowItem[] = []
  for (const tx of txs) {
    const variant = variantForTx(ta, tx)
    if (variant) items.push({ tx, ta, variant, swapRelation: relateTxToSwaps(swaps, tx) })
  }

  return (
    <div className='frame'>
      <NavHeader title={ta.name} onBack={nav.back} />
      <div className='scroll'>
        <div className='body'>
          <section className='asset-hero' aria-label={t('Balance')}>
            <CoinLogo ticker={ta.ticker} alt='' className='coin-xl' />
            {rate > 0
              ? (
                <>
                  <div className='asset-val'>{Doc.formatUSD(usdValue)}</div>
                  <div className='asset-bal mono'>{balance}</div>
                </>
                )
              : <div className='asset-val mono'>{balance}</div>}
            {rate > 0 && (
              <div className='asset-meta mono'>
                <span className='demi'>{Doc.formatUSD(rate)}</span> <span className='grey'>{t('price')}</span>
                {portfolioPct !== null && (
                  <>
                    <span className='grey'> · </span>
                    <span className='demi'>{portfolioPct.toFixed(1)}%</span> <span className='grey'>{t('of portfolio')}</span>
                  </>
                )}
              </div>
            )}
            {sync !== null && (
              <div className='asset-sync gold'>
                <Spinner gold small />{tvar('syncing_pct', { pct: String(Math.round(sync * 100)) })}
              </div>
            )}
          </section>

          <AssetActions ticker={ta.ticker} swapDisabled={!swapAvailable} />

          {ta.variants.map(v => {
            const tl = v.token ? app.user?.assets[v.assetID]?.wallet?.trustline : undefined
            if (!tl || tl.status === ApprovalStatus.Approved) return null
            const net = app.user?.assets[networkID(v)]
            const pending = tl.status === ApprovalStatus.Pending
            return (
              <div key={v.assetID} className='callout caution strong mt20 notice'>
                {pending && <Spinner gold small />}
                <span className='grow'>
                  {pending
                    ? tvar('The {{ token }} trust line on {{ network }} is being established.', { token: ta.ticker, network: net?.name ?? '' })
                    : tvar('{{ token }} on {{ network }} cannot receive deposits until a trust line is established.', { token: ta.ticker, network: net?.name ?? '' })}
                </span>
                {!pending && (
                  <button type='button' className='chip' onClick={() => nav.push({ kind: 'receive', ticker: ta.ticker, assetID: v.assetID })}>
                    {t('Set up')}
                  </button>
                )}
              </div>
            )
          })}

          {hasUnavailable && (
            <>
              <h2 className='lbl sec'>{t('Balance')}</h2>
              <div className='list'>
                <div className='kv'><span>{t('Available')}</span><span className='mono'>{Doc.formatConventionalValue(ta.avail, ta.precision)} {ta.ticker}</span></div>
                {ta.locked > 0 && <div className='kv'><span>{t('Locked')}</span><span className='mono'>{Doc.formatConventionalValue(ta.locked, ta.precision)} {ta.ticker}</span></div>}
                {ta.immature > 0 && <div className='kv'><span>{t('Immature')}</span><span className='mono'>{Doc.formatConventionalValue(ta.immature, ta.precision)} {ta.ticker}</span></div>}
              </div>
            </>
          )}

          {isMultiNet && (
            <>
              <h2 className='lbl sec'>{t('Network balances')}</h2>
              <div className='list'>
                {ta.variants.map(v => {
                  const net = networkAsset(v)
                  const atomic = variantTotal(v)
                  const conv = atomic / v.ui.conventional.conversionFactor
                  const pct = ta.total > 0 ? conv / ta.total * 100 : 0
                  const vRate = app.idRate(v.assetID)
                  return (
                    <div key={v.assetID} className='row'>
                      <NetworkedTokenLogo token={ta.ticker} network={net?.unitInfo.conventional.unit ?? ''} className='coin' />
                      <div className='grow'>
                        <div className='t1'>{net?.name ?? ''}</div>
                        <div className='t2'>{tvar('{{ pct }}% of total', { pct: pct.toFixed(1) })}</div>
                      </div>
                      <div className='num'>
                        <div className='mono fs15'>{Doc.formatCoinValue(atomic, v.ui)} {v.ticker}</div>
                        {vRate > 0 && <div className='t2'>{Doc.usdString(atomic, v.ui.conventional.conversionFactor, vRate)}</div>}
                      </div>
                    </div>
                  )
                })}
              </div>
            </>
          )}

          <div className='sec-row'>
            <h2 className='lbl'>{t('Transactions')}</h2>
            {items.length > 0 && <button type='button' className='link' onClick={() => nav.setTab('activity')}>{t('See all')}</button>}
          </div>
          {loadingTxns
            ? <div className='empty'><Spinner /></div>
            : items.length === 0
              ? <div className='empty'>{t('No transactions yet.')}</div>
              : <div className='stack'>{items.slice(0, TX_ROWS).map(item => <TxRow key={`${item.variant.assetID}-${item.tx.id}`} item={item} onOpen={setDetail} showNetwork />)}</div>}
        </div>
      </div>
      {detail && <TxDetailSheet assetID={detail.variant.assetID} tx={detail.tx} onClose={() => setDetail(null)} />}
    </div>
  )
}
