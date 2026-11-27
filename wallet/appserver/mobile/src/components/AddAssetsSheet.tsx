import { useState } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { postJSON } from '@caribou/shared/http'
import { useForceUpdate, useCoreSubscriptions } from '@caribou/shared/hooks'
import { TickerAsset, AssetVariant, networkID } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import { Check, Icon, Sheet, Spinner } from './ui'

// Wallet type string constants that match the Go-side constants in
// wallet/asset/btc and wallet/asset/ltc.
const walletTypeElectrum = 'electrumRPC'
const walletTypeSPV = 'SPV'

// activateNetwork creates the wallet for a network asset, which also creates
// the wallets of the tokens that settle on it. It returns an error message,
// or '' on success.
export async function activateNetwork (netID: number, walletType?: string): Promise<string> {
  const def = walletType ?? app.user?.assets[netID]?.info?.availablewallets?.[0]?.type
  if (!def) return t('Activation for this asset is not yet supported.')
  const r = await postJSON('/api/newwallet', { assetID: netID, walletType: def, config: {} })
  return r.ok ? '' : (r.errMsg || r.msg || t('Failed to activate asset'))
}

// spvChoice returns the Electrum and SPV wallet definitions when a network
// offers both (BTC and LTC today), for the "Increased security and privacy
// mode" option.
function spvChoice (netID: number): { electrum: string, spv: string } | null {
  const defs = app.user?.assets[netID]?.info?.availablewallets ?? []
  const e = defs.find(d => d.type === walletTypeElectrum)
  const s = defs.find(d => d.type === walletTypeSPV)
  return e && s ? { electrum: e.type, spv: s.type } : null
}

interface SpvOptionProps {
  on: boolean
  onChange: (on: boolean) => void
  disabled?: boolean
}

// SpvOption is the opt-in for a BIP-157/158 wallet instead of Electrum, with
// an expandable explanation of the tradeoffs.
function SpvOption ({ on, onChange, disabled }: SpvOptionProps) {
  const [info, setInfo] = useState(false)
  return (
    <div className='spv-opt'>
      <div className='spv-row'>
        <button type='button' role='checkbox' aria-checked={on} className='check-row grow' disabled={disabled} onClick={() => onChange(!on)}>
          <Check on={on} small />
          <span className='fs14'>{t('spv_mode_label')}</span>
        </button>
        <button type='button' className='ib sm grey' onClick={() => setInfo(!info)} aria-label={t('spv_info_title')} aria-expanded={info}>
          <Icon name='info' />
        </button>
      </div>
      {info && (
        <div className='callout mt8'>
          <span>
            <span className='demi'>{t('spv_info_title')}</span>
            <p className='mt4'>{t('spv_info_what')}</p>
            <p className='mt4'>{t('spv_info_tradeoffs')}</p>
          </span>
        </div>
      )}
    </div>
  )
}

interface NetRow {
  netID: number
  name: string
  ticker: string
  active: boolean
}

// networkRows lists the distinct settlement networks of a ticker's variants.
function networkRows (ta: TickerAsset): NetRow[] {
  const seen = new Set<number>()
  const rows: NetRow[] = []
  for (const v of ta.variants) {
    const id = networkID(v)
    if (seen.has(id)) continue
    seen.add(id)
    const a = app.user?.assets[id]
    rows.push({ netID: id, name: a?.name ?? '', ticker: a?.unitInfo.conventional.unit ?? '', active: Boolean(a?.wallet) })
  }
  return rows
}

function nativeVariant (ta: TickerAsset): AssetVariant {
  return ta.variants.find(v => !v.token) ?? ta.variants[0]
}

interface InactiveRowProps {
  ta: TickerAsset
  busy: boolean
  setBusy: (b: boolean) => void
  // pin keeps the row listed while it activates and while it shows an error.
  // Activating a token's first network marks the ticker as held, which would
  // otherwise drop the row mid-run and lose any later network's error.
  pin: (ticker: string, on: boolean) => void
}

// InactiveRow is one asset without a wallet. A plain native asset activates
// on tap. A token on several networks expands to a per-network picker, and a
// network with an SPV option expands to show it, before activating.
function InactiveRow ({ ta, busy, setBusy, pin }: InactiveRowProps) {
  const nets = networkRows(ta)
  const native = !nativeVariant(ta).token
  const spv = native ? spvChoice(nets[0]?.netID) : null
  const multi = nets.length > 1
  const expandable = multi || Boolean(spv)

  const [open, setOpen] = useState(false)
  const [spvOn, setSpvOn] = useState(false)
  const [picked, setPicked] = useState<Set<number>>(() => new Set(
    nets.filter(n => !n.active && app.user?.assets[n.netID]?.info?.network?.priority).map(n => n.netID)))
  const [running, setRunning] = useState<number | null>(null)
  const [err, setErr] = useState('')

  const run = async (ids: number[]) => {
    pin(ta.ticker, true)
    setBusy(true)
    setErr('')
    const errs: string[] = []
    for (const id of ids) {
      setRunning(id)
      const walletType = spv ? (spvOn ? spv.spv : spv.electrum) : undefined
      const e = await activateNetwork(id, walletType)
      if (e) errs.push(nets.length > 1 ? `${app.user?.assets[id]?.name ?? ''}: ${e}` : e)
    }
    setRunning(null)
    setBusy(false)
    setErr(errs.join('\n'))
    // On success the wallet note feed redraws the sheet and the row drops out
    // once core confirms the wallet. A failure keeps the row, and its error,
    // in view.
    if (errs.length === 0) pin(ta.ticker, false)
  }

  const togglePick = (id: number) => {
    const next = new Set(picked)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    setPicked(next)
  }

  const sub = multi
    ? tvar('{{ ticker }} · available on {{ n }} networks', { ticker: ta.ticker, n: String(nets.length) })
    : !native && nets[0]
        ? tvar('{{ ticker }} · token on {{ network }}', { ticker: ta.ticker, network: nets[0].name })
        : ta.ticker

  const head = (
    <>
      <CoinLogo ticker={ta.ticker} alt='' className='coin' />
      <div className='grow'><div className='t1'>{ta.name}</div><div className='t2'>{sub}</div></div>
    </>
  )

  const trailing = running !== null && !open
    ? <span className='activating'><Spinner />{t('Activating')}</span>
    : expandable
      ? <Icon name='chevron-left' className={open ? 'chev-v open' : 'chev-v'} />
      : <button type='button' className='chip' disabled={busy} onClick={() => run([nets[0].netID])}>{t('Activate')}</button>

  if (!expandable) {
    return (
      <div>
        <div className='row'>{head}{trailing}</div>
        {err && <div className='row-err'>{err}</div>}
      </div>
    )
  }

  const pickedIDs = nets.filter(n => !n.active && picked.has(n.netID)).map(n => n.netID)

  return (
    <div>
      <button type='button' className='row tappable' aria-expanded={open} onClick={() => setOpen(!open)} disabled={running !== null && !open}>
        {head}{trailing}
      </button>
      {open && (
        <div className='expand'>
          {multi && (
            <fieldset className='net-pick'>
              <legend>{t('Which networks would you like to activate?')}</legend>
              {nets.map(n => (
                <button
                  key={n.netID}
                  type='button'
                  role='checkbox'
                  aria-checked={n.active || picked.has(n.netID)}
                  className='net-opt'
                  disabled={n.active || running !== null}
                  onClick={() => togglePick(n.netID)}
                >
                  <Check on={n.active || picked.has(n.netID)} small />
                  <CoinLogo ticker={n.ticker} alt='' className='coin-sm' />
                  <span className='grow fs15'>{n.name}</span>
                  {n.active && <span className='grey fs12'>{t('Active')}</span>}
                  {running === n.netID && <Spinner />}
                </button>
              ))}
            </fieldset>
          )}
          {spv && <SpvOption on={spvOn} onChange={setSpvOn} disabled={running !== null} />}
          <button
            type='button'
            className='btn mt10'
            disabled={busy || (multi && pickedIDs.length === 0)}
            onClick={() => run(multi ? pickedIDs : [nets[0].netID])}
          >
            {running !== null
              ? <><Spinner />{t('Activating')}</>
              : multi
                ? tvar('Activate on {{ n }} networks', { n: String(pickedIDs.length) })
                : tvar('Activate {{ name }}', { name: ta.name })}
          </button>
          {err && <div className='row-err pre'>{err}</div>}
        </div>
      )}
    </div>
  )
}

// AddAssetsSheet lists the assets without a wallet, searchable, each with an
// Activate control (desktop ActivateAsset parity).
export default function AddAssetsSheet ({ onClose }: { onClose: () => void }) {
  const redraw = useForceUpdate()
  const [query, setQuery] = useState('')
  const [busy, setBusy] = useState(false)
  const [pinned, setPinned] = useState<Set<string>>(new Set())
  const pin = (ticker: string, on: boolean) => setPinned(prev => {
    const next = new Set(prev)
    if (on) next.add(ticker)
    else next.delete(ticker)
    return next
  })

  // The walletstate/walletconfig note that flips a ticker's hasWallets
  // arrives over the WS, racing the /api/newwallet response; redraw on wallet
  // notes so an activated row drops out as soon as core confirms it.
  useCoreSubscriptions({
    onNote: (n) => {
      switch (n.type) {
        case 'walletstate':
        case 'walletconfig':
        case 'walletsync':
          redraw()
      }
    }
  }, [redraw])

  const q = query.trim().toLowerCase()
  const inactive = (app.tickerList ?? [])
    .filter(ta => !ta.hasWallets || pinned.has(ta.ticker))
    .filter(ta => q === '' || ta.name.toLowerCase().includes(q) || ta.ticker.toLowerCase().includes(q))
    .sort((a, b) => a.name.localeCompare(b.name))

  return (
    <Sheet title={t('Add assets')} onClose={onClose} tall>
      <label className='field search'>
        <Icon name='search' />
        <input type='search' placeholder={t('Search assets')} aria-label={t('Search assets')} value={query} onChange={e => setQuery(e.target.value)} />
      </label>
      <h2 className='lbl sec'>{t('Available to activate')}</h2>
      {inactive.length === 0
        ? <div className='empty'>{q ? t('No matching assets.') : t('Every supported asset is active.')}</div>
        : (
          <div className='list'>
            {inactive.map(ta => <InactiveRow key={ta.ticker} ta={ta} busy={busy} setBusy={setBusy} pin={pin} />)}
          </div>
          )}
    </Sheet>
  )
}
