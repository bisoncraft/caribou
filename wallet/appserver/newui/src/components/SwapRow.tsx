import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import AssetLogo from './AssetLogo'
import Spinner from './Spinner'
import { IntentsSwap, StateSuccess, StateRefunded, StateExpired, StateDepositFailed, swapIsActive, stateLabel, stateColorClass } from '../intents'

// swapTime is the ordering timestamp for a swap in the Activity feed, in
// seconds (matching WalletTransaction.timestamp).
export function swapTime (s: IntentsSwap): number {
  return Math.floor(new Date(s.createdAt).getTime() / 1000)
}

// SwapRow is a cross-chain swap's row in the Activity feed: paired coin
// logos, the pair and status, and both legs' amounts. Clicking opens the
// swap dialog on the record's progress view.
interface SwapRowProps {
  swap: IntentsSwap
  // See TxRow: shared-hover reporting and highlight for rows serving the
  // same swap.
  onSwapHover?: (depositAddress: string) => void
  swapHighlight?: boolean
}

export default function SwapRow ({ swap, onSwapHover, swapHighlight }: SwapRowProps) {
  const open = () => app.setForm({ form: 'swap', data: { depositAddress: swap.depositAddress } })

  const active = swapIsActive(swap)
  const originAsset = app.user?.assets[swap.originAssetID]
  const destAsset = app.user?.assets[swap.destAssetID]
  const originTicker = originAsset?.unitInfo.conventional.unit ?? swap.originAsset
  const destTicker = destAsset?.unitInfo.conventional.unit ?? swap.destAsset
  const amountIn = originAsset ? Doc.formatCoinValue(swap.atomsIn, originAsset.unitInfo) : swap.amountIn
  const amountOut = destAsset ? Doc.formatCoinValue(swap.atomsOut, destAsset.unitInfo) : swap.amountOut
  const usdOut = destAsset
    ? Doc.usdString(swap.atomsOut, destAsset.unitInfo.conventional.conversionFactor, app.idRate(swap.destAssetID) ?? 0)
    : ''
  const age = tvar('{{ age }} ago', { age: Doc.timeSince(swapTime(swap) * 1000) })

  // The amount column reflects the outcome: the received (or expected)
  // destination amount for successful and in-flight swaps, the amount
  // actually returned for refunds, nothing for an expired quote or a
  // rejected deposit (no funds moved either way), and an em dash for
  // failures.
  const amounts = () => {
    if (swap.state === StateSuccess || active) {
      const approx = swap.state === StateSuccess ? '' : '~'
      return (
        <>
          <div className="fs14 mono tx-amt-pos">
            +{approx}{amountOut} {destTicker}
            {usdOut !== '' && <span className="grey"> · {usdOut}</span>}
          </div>
          <div className="grey fs12 mono">−{amountIn} {originTicker}</div>
        </>
      )
    }
    if (swap.state === StateRefunded) {
      // The service refunds net of its fees; refundedAtoms is the recorded
      // net amount, matching the swap dialog's figure. Absent/zero (an older
      // or unresolvable record) falls back to the deposit amount.
      const returnedAtoms = swap.refundedAtoms || swap.atomsIn
      const returned = originAsset ? Doc.formatCoinValue(returnedAtoms, originAsset.unitInfo) : (swap.refundedAmount || swap.amountIn)
      const usdReturned = originAsset
        ? Doc.usdString(returnedAtoms, originAsset.unitInfo.conventional.conversionFactor, app.idRate(swap.originAssetID) ?? 0)
        : ''
      return (
        <div className="grey fs14 mono">
          {tvar('{{ amt }} {{ ticker }} returned', { amt: returned, ticker: originTicker })}
          {usdReturned !== '' && <span className="fs12"> · {usdReturned}</span>}
        </div>
      )
    }
    if (swap.state === StateExpired || swap.state === StateDepositFailed) return <div className="grey fs14 mono">—</div>
    return ( // FAILED: the deposit may be gone and nothing arrived.
      <>
        <div className="grey fs14 mono">—</div>
        <div className="grey fs12 mono">−{amountIn} {originTicker}</div>
      </>
    )
  }

  return (
    <div
      className={`activity-row d-flex align-items-center pointer mb-2 ${active ? 'activity-row-unconfirmed' : ''} ${swapHighlight ? 'swap-hover' : ''}`}
      role="button"
      tabIndex={0}
      onClick={open}
      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open() } }}
      onMouseEnter={() => onSwapHover?.(swap.depositAddress)}
      onMouseLeave={() => onSwapHover?.('')}
    >
      <span className="swap-row-logos position-relative icon-32 me-3">
        <AssetLogo assetID={swap.originAssetID} ticker={originTicker} className="icon-24 swap-row-logo-origin" />
        <AssetLogo assetID={swap.destAssetID} ticker={destTicker} className="icon-24 swap-row-logo-dest" />
      </span>
      <div className="flex-grow-1">
        <div className="fs14 demi">{tvar('Swap {{ from }} → {{ to }}', { from: originTicker, to: destTicker })}</div>
        <div className="grey fs12 d-flex align-items-center">
          {active && <Spinner size={10} className="me-1" />}
          <span className={stateColorClass(swap.state)}>{stateLabel(swap.state)}</span>
          <span className="mx-1">·</span>
          <span>{age}</span>
          {swap.reconstructed && <><span className="mx-1">·</span><span>{t('recovered')}</span></>}
        </div>
      </div>
      <div className="text-right">
        {amounts()}
      </div>
    </div>
  )
}
