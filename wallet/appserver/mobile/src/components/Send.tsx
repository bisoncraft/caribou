import { useEffect, useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import { postJSON } from '@caribou/shared/http'
import { WalletTransaction } from '@caribou/shared/registry'
import { TickerAsset, AssetVariant, compareByFiatValue, networkAsset, networkID, isVariantActive } from '@caribou/shared/assets'
import { TxType } from '@caribou/shared/tx'
import { fetchSwaps } from '@caribou/shared/intents'
import { useSendForm } from '@caribou/shared/useSendForm'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import { useNav } from '../nav'
import { readClipboard } from '../bridge'
import { Icon, NavHeader, PasswordField, PasteIcon, Sheet, SlideToConfirm, Spinner, Toggle } from './ui'
import { AssetList, NetworkList, PickerSheet, variantBalance } from './Pickers'

interface SendProps {
  ticker?: string
  assetID?: number
}

interface Recent {
  address: string
  amount: number
  timestamp: number
}

// Number of past sends to scan, and recipients to offer, on the recipient
// step.
const recentScan = 50
const recentShown = 3

// useRecentRecipients loads the wallet's latest distinct send recipients,
// newest first. Wallets record Send entries only from their own signing paths,
// so an incoming dust transaction cannot plant a look-alike address here.
// Cross-chain swap deposit addresses are excluded: each is single-use, and a
// repeat payment to one is not a send to the original counterparty.
function useRecentRecipients (variant: AssetVariant | null): Recent[] {
  const [recent, setRecent] = useState<Recent[]>([])
  useEffect(() => {
    setRecent([])
    if (!variant) return
    let cancelled = false
    Promise.all([
      postJSON('/api/txhistory', { assetID: variant.assetID, n: recentScan, past: true }),
      fetchSwaps()
    ]).then(([r, swaps]) => {
      if (cancelled || !r.ok) return
      const deposits = new Set(swaps.map(s => s.depositAddress.toLowerCase()))
      const seen = new Set<string>()
      const out: Recent[] = []
      for (const tx of (r.txs ?? []) as WalletTransaction[]) {
        if (tx.type !== TxType.Send || !tx.recipient || seen.has(tx.recipient)) continue
        if (deposits.has(tx.recipient.toLowerCase())) continue
        seen.add(tx.recipient)
        out.push({ address: tx.recipient, amount: tx.amount, timestamp: tx.timestamp })
        if (out.length === recentShown) break
      }
      setRecent(out)
    })
    return () => { cancelled = true }
  }, [variant?.assetID])
  return recent
}

function shortDate (unixSecs: number): string {
  if (!unixSecs) return ''
  return new Date(unixSecs * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

// fiatFor returns the USD value of an atomic amount of the given asset, or ''
// without a rate.
function fiatFor (atomic: number, assetID: number): string {
  const rate = app.idRate(assetID)
  if (!(rate > 0)) return ''
  return Doc.usdString(atomic, app.unitInfo(assetID).conventional.conversionFactor, rate)
}

type Step = 'to' | 'amount'

// sendableAssets lists the assets with a spendable balance, highest fiat
// value first.
function sendableAssets (): TickerAsset[] {
  return (app.tickerList ?? []).filter(item => item.avail > 0).sort(compareByFiatValue)
}

// Send walks asset -> network -> recipient -> amount -> review. The shared
// useSendForm hook owns the state, address validation, fee estimation, Max
// and submit; this component is the mobile layout over it.
export default function Send ({ ticker, assetID }: SendProps) {
  const nav = useNav()
  // Without a caller-chosen asset, the flow opens on the recipient step with
  // the highest-value spendable asset preselected; the From row changes it.
  const [defaultTicker] = useState(() => ticker ?? sendableAssets()[0]?.ticker)
  const f = useSendForm({ ticker: defaultTicker, onSent: () => nav.setTab('activity') })
  const { ta, variant } = f
  const [step, setStep] = useState<Step>('to')
  const [picker, setPicker] = useState(false)
  const [reviewing, setReviewing] = useState(false)
  const recent = useRecentRecipients(variant)

  // An exact network from the caller (an asset's network row) preselects it.
  useEffect(() => {
    if (ta && assetID !== undefined && !variant) {
      const v = ta.variantLookup[assetID]
      if (v && isVariantActive(v)) f.selectVariant(v)
    }
  }, [])

  const sendable = sendableAssets()
  const activeVariants = (x: TickerAsset) => x.variants.filter(isVariantActive)

  const pickAsset = (chosen: TickerAsset) => {
    setPicker(false)
    setStep('to')
    f.setVariant(null)
    f.pickAsset(chosen)
  }

  const pickVariant = (v: AssetVariant) => {
    if (v.bal.available <= 0) { f.pickVariant(v); return }
    setPicker(false)
    setStep('to')
    f.pickVariant(v)
  }

  const balanceDetail = (x: TickerAsset) => (
    <span className='mono grey fs14'>{Doc.formatConventionalValue(x.avail, x.precision)} {x.ticker}</span>
  )

  const pickers = (
    <>
      {picker && (
        <PickerSheet title={t('Send from')} onClose={() => setPicker(false)}>
          <AssetList assets={sendable} onPick={pickAsset} detail={balanceDetail} empty={t('No funds available to send.')} />
        </PickerSheet>
      )}
    </>
  )

  // Asset and network steps, before a variant is chosen.
  if (!ta || !variant) {
    let body
    let title = t('Send')
    if (!ta) {
      body = <AssetList assets={sendable} onPick={pickAsset} detail={balanceDetail} empty={t('No funds available to send.')} />
    } else {
      title = tvar('Send {{ name }}', { name: ta.name })
      const vs = activeVariants(ta)
      body = vs.length === 0
        ? <div className='empty'>{t('No active network for this asset. Activate one first.')}</div>
        : (
          <>
            <button type='button' className='from-row' onClick={() => setPicker(true)}>
              <CoinLogo ticker={ta.ticker} alt='' className='coin' />
              <span className='grow'>
                <span className='from-lbl'>{t('From')}</span>
                <span className='from-name'>{ta.name}</span>
              </span>
              <Icon name='chevron-left' className='chev-down' />
            </button>
            <h2 className='lbl sec'>{t('Choose the network to send from.')}</h2>
            <NetworkList variants={vs} onPick={pickVariant} detail={variantBalance} />
            {vs.map(v => f.netErr[v.assetID] && <div key={v.assetID} className='field-err'>{f.netErr[v.assetID]}</div>)}
          </>
          )
    }
    return (
      <div className='frame'>
        <NavHeader title={title} onBack={nav.back} close />
        <div className='scroll'><div className='body'>{body}</div></div>
        {pickers}
      </div>
    )
  }

  const net = networkAsset(variant)
  const feeAssetID = networkID(variant)
  const feeText = f.feeKnown && f.feeUI ? `${Doc.formatCoinValue(f.txFee, f.feeUI)} ${f.feeUI.conventional.unit}` : ''
  const feeFiat = f.feeKnown ? fiatFor(f.txFee, feeAssetID) : ''
  const amountFiat = fiatFor(f.value, variant.assetID)
  const sendsFrom = activeVariants(ta).length > 1 ? tvar('{{ name }} on {{ network }}', { name: ta.name, network: net?.name ?? '' }) : ta.name

  if (step === 'to') {
    const canContinue = Boolean(f.addrValid) && f.destTagValid
    const paste = async () => {
      const s = (await readClipboard()).trim()
      if (s) f.setAddress(s)
    }
    return (
      <div className='frame'>
        <NavHeader title={t('Send')} onBack={nav.back} close />
        <div className='scroll'>
          <div className='body body-col'>
            <button type='button' className='from-row' onClick={() => setPicker(true)}>
              <CoinLogo ticker={ta.ticker} alt='' className='coin' />
              <span className='grow'>
                <span className='from-lbl'>{t('From')}</span>
                <span className='from-name'>{sendsFrom}</span>
              </span>
              <span className='mono grey fs14'>{Doc.formatCoinValue(f.available, variant.ui)} {variant.ticker}</span>
              <Icon name='chevron-left' className='chev-down' />
            </button>

            <label htmlFor='send-to' className='lbl sec'>{t('To')}</label>
            <div className={f.addrValid === false ? 'area bad' : 'area'}>
              <textarea
                id='send-to'
                className='mono'
                rows={2}
                placeholder={tvar('{{ name }} address', { name: net?.name ?? ta.name })}
                value={f.address}
                onChange={e => f.setAddress(e.target.value.replace(/\s+/g, ''))}
                autoCapitalize='off'
                autoCorrect='off'
                autoComplete='off'
                spellCheck={false}
              />
              <div className='area-tools'>
                {f.address && f.addrValid === null && <Spinner small />}
                {f.addrValid === true && <span className='pos fs13 demi'><Icon name='check' /> {t('Valid address')}</span>}
                <span className='grow' />
                <button type='button' className='chip tile' onClick={paste}><PasteIcon />{t('Paste')}</button>
              </div>
            </div>
            {f.addrValid === false && <div className='field-err'>{t('This is not a valid address for this network.')}</div>}

            {f.supportsDestTag && (
              <>
                <div className='sec-row'>
                  <label htmlFor='send-tag' className='lbl'>{t('Destination tag')}</label>
                  <span className='grey fs12'>{t('optional')}</span>
                </div>
                <div className={f.destTagValid ? 'field' : 'field bad'}>
                  <input id='send-tag' type='text' inputMode='numeric' className='mono' placeholder={t('tag')} value={f.destTag} onChange={f.destTagChanged} />
                </div>
                {!f.destTagValid && <div className='field-err'>{t('Destination tag is too large.')}</div>}
              </>
            )}

            {recent.length > 0 && (
              <>
                <h2 className='lbl sec'>{t('Recent')}</h2>
                <div className='list'>
                  {recent.map(r => (
                    <button key={r.address} type='button' className='row tappable' onClick={() => f.setAddress(r.address)}>
                      <span className='av'><Icon name='upload' /></span>
                      <span className='grow'>
                        <span className='mono recent-addr'>{Doc.shortenAddress(r.address)}</span>
                        <span className='t2 block'>
                          {tvar('Sent {{ amount }} {{ ticker }}', { amount: Doc.formatCoinValue(r.amount, variant.ui), ticker: variant.ticker })}
                          {r.timestamp > 0 && ` · ${shortDate(r.timestamp)}`}
                        </span>
                      </span>
                    </button>
                  ))}
                </div>
              </>
            )}

            <div className='mt-auto pt16'>
              <button type='button' className='btn' disabled={!canContinue} onClick={() => setStep('amount')}>{t('Continue')}</button>
            </div>
          </div>
        </div>
        {pickers}
      </div>
    )
  }

  const reviewDisabled = f.value <= 0 || f.overBalance || !f.feeKnown
  const recipientGets = f.subtract ? Math.max(0, f.value - f.nativeFee) : f.value
  const balanceAfter = f.available - f.totalDebit
  const amountLabel = `${Doc.formatCoinValue(f.value, variant.ui)} ${variant.ticker}`

  return (
    <div className='frame'>
      <NavHeader title={tvar('Send {{ name }}', { name: ta.name })} onBack={() => setStep('to')} />
      <div className='scroll'>
        <div className='body body-col'>
          <div className='center'>
            <button type='button' className='to-chip' onClick={() => setStep('to')}>
              <span className='grey'>{t('To')}</span>
              <span className='mono fs15'>{Doc.shortenAddress(f.address)}</span>
              <Icon name='edit' className='grey fs13' />
            </button>
          </div>

          <label htmlFor='send-amt' className='lbl sec'>{t('Amount')}</label>
          <div className={f.overBalance ? 'amt-box bad' : 'amt-box'}>
            <div className='amt-line'>
              <input
                id='send-amt'
                className='amt-input'
                type='text'
                inputMode='decimal'
                placeholder='0'
                autoComplete='off'
                value={f.amount}
                onChange={f.amountChanged}
                aria-label={tvar('Amount in {{ ticker }}', { ticker: variant.ticker })}
                autoFocus
              />
              <span className='amt-unit'>{variant.ticker}</span>
            </div>
            <div className='amt-sub'>
              <span className='grey fs14'>{amountFiat ? `≈ ${amountFiat}` : ''}</span>
              <button type='button' className='chip sel' onClick={f.setMax}>{t('Max')}</button>
            </div>
          </div>
          {f.overBalance && <div className='field-err'>{t('Amount exceeds available balance.')}</div>}

          <div className='kvs fs13 mt8'>
            <div className='kv'><span>{t('Available')}</span><span className='mono'>{Doc.formatCoinValue(f.available, variant.ui)} {variant.ticker}</span></div>
            <div className='kv'>
              <span>{t('Network fee')}</span>
              <span className='mono'>
                {f.estimating ? <Spinner small /> : feeText || '—'}
                {feeFiat && <span className='grey'> · {feeFiat}</span>}
              </span>
            </div>
            {f.isWithdrawer && (
              <div className='kv'>
                <span>{t('Subtract fee from amount')}</span>
                <Toggle on={f.subtract} onChange={f.setSubtract} label={t('Subtract fee from amount')} />
              </div>
            )}
          </div>
          {f.feeErr && <div className='err-box mt12'>{f.feeErr}</div>}

          <div className='mt-auto pt16'>
            <button type='button' className='btn' disabled={reviewDisabled} onClick={() => { f.setErr(''); setReviewing(true) }}>{t('Review')}</button>
          </div>
        </div>
      </div>

      {reviewing && (
        <Sheet
          title={t('Review send')}
          onClose={() => { if (!f.sending) setReviewing(false) }}
          foot={
            <>
              {f.err && <div className='err-box' aria-live='polite'>{f.err}</div>}
              {f.requirePW && (
                <PasswordField value={f.pw} onChange={f.setPW} placeholder={t('App password')} disabled={f.sending} autoFocus />
              )}
              <SlideToConfirm
                label={tvar('Slide to send {{ amount }}', { amount: amountLabel })}
                busyLabel={t('Sending...')}
                onCommit={f.submit}
                disabled={!f.canSubmit}
                busy={f.sending}
              />
            </>
          }
        >
          <div className='review-hero'>
            <CoinLogo ticker={ta.ticker} alt='' className='coin-lg' />
            <div className='review-amt'>{amountLabel}</div>
            {amountFiat && <div className='grey fs15 mt4'>{amountFiat}</div>}
          </div>
          <div className='list'>
            <div className='kv'><span>{t('To')}</span><span className='mono addr-full'>{f.address}</span></div>
            {f.supportsDestTag && f.destTag !== '' && <div className='kv'><span>{t('Destination tag')}</span><span className='mono'>{f.destTag}</span></div>}
            <div className='kv'><span>{t('Network')}</span><span>{net?.name ?? ''}</span></div>
            <div className='kv'>
              <span>{t('Network fee')}</span>
              <span className='mono'>{feeText}{feeFiat && <><br /><span className='grey'>{feeFiat}</span></>}</span>
            </div>
            <div className='kv'><span>{t('Recipient gets')}</span><span className='mono'>{Doc.formatCoinValue(recipientGets, variant.ui)} {variant.ticker}</span></div>
            <div className='kv'><span>{t('Balance after')}</span><span className='mono'>{Doc.formatCoinValue(Math.max(0, balanceAfter), variant.ui)} {variant.ticker}</span></div>
          </div>
        </Sheet>
      )}
    </div>
  )
}
