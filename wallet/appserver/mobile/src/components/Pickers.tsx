import { ReactNode, useState } from 'react'
import t from '@caribou/shared/intl'
import Doc from '@caribou/shared/doc'
import { TickerAsset, AssetVariant, networkAsset } from '@caribou/shared/assets'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import { Icon, Sheet } from './ui'

interface AssetListProps {
  assets: TickerAsset[]
  onPick: (ta: TickerAsset) => void
  // detail renders the right-hand column of a row (e.g. a balance).
  detail?: (ta: TickerAsset) => ReactNode
  empty: string
}

// AssetList is a searchable list of ticker assets, used as the first step of
// the receive and send flows and inside the asset picker sheet.
export function AssetList ({ assets, onPick, detail, empty }: AssetListProps) {
  const [query, setQuery] = useState('')
  const q = query.trim().toLowerCase()
  const shown = assets.filter(ta => q === '' || ta.name.toLowerCase().includes(q) || ta.ticker.toLowerCase().includes(q))
  if (assets.length === 0) return <div className='empty'>{empty}</div>
  return (
    <>
      {assets.length > 6 && (
        <label className='field search mb12'>
          <Icon name='search' />
          <input type='search' placeholder={t('Search assets')} aria-label={t('Search assets')} value={query} onChange={e => setQuery(e.target.value)} />
        </label>
      )}
      <div className='list'>
        {shown.map(ta => (
          <button key={ta.ticker} type='button' className='row tappable' onClick={() => onPick(ta)}>
            <CoinLogo ticker={ta.ticker} alt='' className='coin' />
            <div className='grow'><div className='t1'>{ta.name}</div><div className='t2'>{ta.ticker}</div></div>
            {detail?.(ta)}
          </button>
        ))}
      </div>
      {shown.length === 0 && <div className='empty'>{t('No matching assets.')}</div>}
    </>
  )
}

interface NetworkListProps {
  variants: AssetVariant[]
  onPick: (v: AssetVariant) => void
  selected?: AssetVariant | null
  detail?: (v: AssetVariant) => ReactNode
}

// NetworkList lists a ticker's network deployments, each with the network's
// logo and name.
export function NetworkList ({ variants, onPick, selected, detail }: NetworkListProps) {
  return (
    <div className='list'>
      {variants.map(v => {
        const net = networkAsset(v)
        return (
          <button key={v.assetID} type='button' className='row tappable' onClick={() => onPick(v)}>
            <CoinLogo ticker={net?.unitInfo.conventional.unit ?? ''} alt='' className='coin' />
            <div className='grow'><div className='t1'>{net?.name ?? ''}</div></div>
            {detail?.(v)}
            {selected?.assetID === v.assetID && <Icon name='check' className='accent' />}
          </button>
        )
      })}
    </div>
  )
}

// variantBalance renders a variant's available balance for a row's right
// column.
export function variantBalance (v: AssetVariant) {
  return <span className='mono grey fs14'>{Doc.formatCoinValue(v.bal.available, v.ui)} {v.ticker}</span>
}

// AssetChip is the tappable "logo name ▾" control that opens an asset picker.
export function AssetChip ({ ta, onClick }: { ta: TickerAsset, onClick: () => void }) {
  return (
    <button type='button' className='sel-chip' onClick={onClick} aria-label={t('Change asset')}>
      <CoinLogo ticker={ta.ticker} alt='' className='coin-chip' />
      {ta.name}
      <Icon name='chevron-left' className='chev-down' />
    </button>
  )
}

interface PickerSheetProps {
  title: string
  onClose: () => void
  children: ReactNode
}

// PickerSheet wraps a picker list in a bottom sheet.
export function PickerSheet ({ title, onClose, children }: PickerSheetProps) {
  return <Sheet title={title} onClose={onClose}>{children}</Sheet>
}
