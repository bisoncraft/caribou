import { useCallback, useEffect, useRef, useState, FormEvent } from 'react'
import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { postJSON, getJSON } from '@caribou/shared/http'
import { CoreNote } from '@caribou/shared/registry'
import State from '@caribou/shared/state'
import CoinLogo from '@caribou/shared/components/CoinLogo'
import Spinner from './Spinner'
import PasswordInput from './PasswordInput'

// ---- Type definitions (mirroring Go structs) ----

interface AppFee {
  recipient: string
  fee: number
}

interface IntentsAsset {
  assetID: number
  symbol: string
  intentsID: string
  // decimals is the Intents-side scale; NOT used for atoms conversion.
  decimals: number
  // atomDecimals is the wallet atomic-unit exponent, which the atoms
  // fields (atomsIn/atomsOut/minAtomsOut, and the value sent to preview/
  // execute) are expressed in. It can differ from decimals (e.g. NEAR: 8
  // wallet drops vs 24 Intents decimals). All atoms conversions use this.
  atomDecimals: number
  priceUSD: number
  walletReady: boolean
}

interface IntentsPreview {
  atomsIn: number
  atomsOut: number
  minAtomsOut: number
  minAtomsIn: number
  amountInUSD: string
  amountOutUSD: string
  timeEstimate: number
  appFees: AppFee[]
  refundFee: string
  withdrawFee: string
}

interface IntentsSwap {
  depositAddress: string
  depositMemo?: string
  state: string
  originAssetID: number
  destAssetID: number
  originAsset: string
  destAsset: string
  atomsIn: number
  amountIn: string
  amountOut: string
  atomsOut: number
  recipient: string
  refundTo: string
  deadline: string
  timeEstimate: number
  depositTxID?: string
  nearTxHashes?: string[]
  destinationTxs?: string[]
  refundTxs?: string[]
  refundedAmount?: string
  refundReason?: string
  appFees?: AppFee[]
  reconstructed?: boolean
  createdAt: string
  updatedAt: string
}

// ---- Bridge trust data (data-driven per plan requirement) ----

// bridgeTrustMap classifies origin-chain symbols by their custody model.
// PoA = validator federation; Omni = distributed MPC; HOT = MPC.
// Updated here as the migration proceeds — the plan explicitly calls for
// this to be data, not baked copy, because the PoA→Omni migration is ongoing.
const bridgeTrustMap: Record<string, { model: string; description: string }> = {
  BTC: { model: 'Omni MPC', description: t('BTC: bridge custody by a distributed MPC signing network (Omni).') },
  ETH: { model: 'Omni MPC', description: t('ETH: bridge custody by a distributed MPC signing network (Omni).') },
  SOL: { model: 'Omni MPC', description: t('SOL: bridge custody by a distributed MPC signing network (Omni).') },
  BNB: { model: 'Omni MPC', description: t('BNB: bridge custody by a distributed MPC signing network (Omni).') },
  BASE: { model: 'Omni MPC', description: t('BASE: bridge custody by a distributed MPC signing network (Omni).') },
  POL: { model: 'HOT MPC', description: t('POL: bridge custody by a distributed MPC signing network (HOT).') },
  LTC: { model: 'PoA Federation', description: t('LTC: bridge custody by a validator federation operated for the service.') },
  DOGE: { model: 'PoA Federation', description: t('DOGE: bridge custody by a validator federation operated for the service.') },
  XRP: { model: 'PoA Federation', description: t('XRP: bridge custody by a validator federation operated for the service.') },
  ZEC: { model: 'PoA Federation', description: t('ZEC: bridge custody by a validator federation operated for the service.') },
  BCH: { model: 'PoA Federation', description: t('BCH: bridge custody by a validator federation operated for the service.') },
  DASH: { model: 'PoA Federation', description: t('DASH: bridge custody by a validator federation operated for the service.') }
}

function bridgeTrust (symbol: string): { model: string; description: string } {
  return bridgeTrustMap[symbol.toUpperCase()] ?? {
    model: t('Third-party bridge'),
    description: tvar('{{ symbol }}: bridge custody by a third-party bridge operator.', { symbol })
  }
}

// ---- Helpers ----

const languages = navigator.languages as string[]
const usdFmt = new Intl.NumberFormat(languages, {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

// atomsToConventional converts from atomic units (integer) to conventional
// display units. Callers pass the wallet atomic-unit exponent
// (IntentsAsset.atomDecimals), which the API's atoms fields use.
function atomsToConventional (atoms: number, decimals: number): number {
  return atoms / Math.pow(10, decimals)
}

// conventionalToAtoms converts conventional display units to atoms.
function conventionalToAtoms (conventional: number, decimals: number): number {
  return Math.round(conventional * Math.pow(10, decimals))
}

// formatConventional displays a value with up to `decimals` decimal places.
function formatConventional (atoms: number, decimals: number): string {
  const v = atomsToConventional(atoms, decimals)
  const fmt = new Intl.NumberFormat(languages, {
    minimumFractionDigits: 2,
    maximumFractionDigits: Math.min(decimals, 8)
  })
  return fmt.format(v)
}

// formatIntentsAmount displays an Intents-side base-unit amount string — the
// string amount/fee fields (amountIn/amountOut, refundFee/withdrawFee,
// refundedAmount) as returned by the service — using the asset's Intents-side
// `decimals`. The numeric atoms* fields use atomDecimals with
// formatConventional instead. Returns the raw string if it does not parse.
function formatIntentsAmount (baseUnits: string, decimals: number): string {
  const n = Number(baseUnits)
  if (!Number.isFinite(n)) return baseUnits
  return formatConventional(n, decimals)
}

// formatDuration renders a duration in seconds as a human-friendly string.
function formatDuration (secs: number): string {
  if (secs <= 0) return t('a few minutes')
  if (secs < 60) return tvar('~{{ n }} seconds', { n: String(secs) })
  const mins = Math.round(secs / 60)
  if (mins < 60) return tvar('~{{ n }} minutes', { n: String(mins) })
  const hrs = Math.round(mins / 60)
  return tvar('~{{ n }} hours', { n: String(hrs) })
}

// swapStatusLabel returns a plain-English status label for an IntentsSwap.
function swapStatusLabel (state: string): string {
  switch (state) {
    case 'QUOTED':
    case 'DEPOSIT_SENT':
    case 'PENDING_DEPOSIT':
    case 'KNOWN_DEPOSIT_TX':
      return t('Waiting for deposit to confirm')
    case 'PROCESSING':
      return t('Swapping — funds with the service')
    case 'SUCCESS':
      return t('Complete')
    case 'REFUNDED':
      return t('Refunded — funds returned to your wallet')
    case 'FAILED':
    case 'INCOMPLETE_DEPOSIT':
      return t('Needs attention')
    case 'EXPIRED':
      return t('Expired — no funds moved')
    default:
      return state
  }
}

function swapStatusClass (state: string): string {
  switch (state) {
    case 'SUCCESS': return 'swap-status-success'
    case 'REFUNDED': return 'swap-status-refunded'
    case 'FAILED':
    case 'INCOMPLETE_DEPOSIT': return 'swap-status-failed'
    case 'EXPIRED': return 'swap-status-expired'
    default: return 'swap-status-active'
  }
}

// isTerminal returns true when a swap has reached a final state.
function isTerminal (state: string): boolean {
  return state === 'SUCCESS' || state === 'REFUNDED' || state === 'FAILED' || state === 'EXPIRED'
}

// isAttention returns true when a swap is in a state that needs user attention.
function isAttention (state: string): boolean {
  return state === 'FAILED' || state === 'INCOMPLETE_DEPOSIT' || state === 'REFUNDED'
}

// nearExplorerURL returns the NEAR Intents explorer URL for a deposit address.
function nearExplorerURL (depositAddress: string): string {
  return `https://explorer.near-intents.org/swap/${encodeURIComponent(depositAddress)}`
}

// ---- Local storage key for first-use disclosure ----
const INTENTS_DISCLOSURE_SEEN_KEY = 'intentsDisclosureSeen'

// ---- Sub-components ----

// CustodyModal is the first-use disclosure shown before the first swap.
function CustodyModal ({ onClose }: { onClose: () => void }) {
  return (
    <div className="swap-custody-modal-scrim flex-center">
      <div className="swap-custody-modal">
        <div className="swap-custody-modal-header">
          <span className="demi fs16">{t('How swaps work')}</span>
        </div>
        <div className="swap-custody-modal-body fs13">
          <ul className="swap-custody-list">
            <li>{t('Swaps are executed by NEAR Intents, a third-party service operated by Defuse Labs Ltd (Gibraltar).')}</li>
            <li>{t('A swap is NOT an atomic swap. While it completes — typically a few minutes — funds are held by the service and its bridge operators.')}</li>
            <li>{t('You send to a single-use deposit address the service controls. Swapped coins arrive at your own wallet address derived from your seed.')}</li>
            <li>{t("If a swap can't complete, funds are returned automatically to your wallet, less any network costs incurred.")}</li>
            <li>{t('Trust during transit varies by chain: some chains use a distributed MPC signing network (stronger); others use a validator federation (weaker). Details are shown per pair.')}</li>
            <li>{t('The service screens transactions for sanctions compliance. A deposit may rarely be delayed or refused.')}</li>
            <li>{t('The quoted rate includes all fees. Caribou never holds your funds; your seed-derived addresses are always the refund and destination recipients.')}</li>
          </ul>
        </div>
        <div className="p-3">
          <button type="button" className="feature w-100" onClick={onClose}>{t('I understand — continue')}</button>
        </div>
      </div>
    </div>
  )
}

// CustodyNotice is the persistent one-line notice shown on the swap form.
function CustodyNotice ({ originAsset, destAsset, showDetails, onToggleDetails }: {
  originAsset?: IntentsAsset
  destAsset?: IntentsAsset
  showDetails: boolean
  onToggleDetails: () => void
}) {
  return (
    <div className="swap-custody-notice">
      <div className="d-flex align-items-start">
        <span className="flex-grow-1 fs12 grey">
          {t('Executed by NEAR Intents (third party). Funds are in the service\'s custody while the swap completes — typically a few minutes.')}
        </span>
        <button
          type="button"
          className="swap-info-btn noborder ms-2 fs13"
          onClick={onToggleDetails}
          aria-label={t('Trust and custody details')}
        >
          ⓘ
        </button>
      </div>
      {showDetails && originAsset && destAsset && (
        <div className="swap-trust-popover mt-2">
          <div className="d-flex align-items-center justify-content-between mb-2">
            <span className="demi fs13">{t('Custody and trust details')}</span>
            <button type="button" className="swap-info-close noborder fs11" onClick={onToggleDetails}>✕</button>
          </div>
          <div className="fs12 grey mb-2">
            <span className="demi">{t('Operator:')}</span> {t('Defuse Labs Ltd, Gibraltar')}
          </div>
          <div className="fs12 mb-1">
            <span className="demi">{originAsset.symbol}:</span> {bridgeTrust(originAsset.symbol).description}
          </div>
          <div className="fs12 mb-1">
            <span className="demi">{destAsset.symbol}:</span> {bridgeTrust(destAsset.symbol).description}
          </div>
          <div className="fs12 grey mt-2">
            {t('All refund and destination addresses are derived from your seed — funds at rest are never in third-party custody.')}
          </div>
          <div className="fs12 grey mt-1">
            {t('Service status:')} <a
              href="https://status.near-intents.org"
              target="_blank"
              rel="noopener noreferrer"
              className="swap-link"
            >status.near-intents.org</a>
          </div>
        </div>
      )}
    </div>
  )
}

// ---- Fee breakdown display ----

// friendlyFeeName maps a fee recipient address to a human-readable label.
// The app fee is split by the service: our configured recipient gets one
// entry; the protocol surcharge goes to a separate account. We show both.
function friendlyFeeName (recipient: string, index: number): string {
  // If the fee recipient is the configured treasury, call it "Caribou fee".
  // Otherwise it's the protocol's collection account (the keyless surcharge).
  // We distinguish by position: index 0 is the app fee, subsequent entries
  // are the service's injected surcharge.
  if (index === 0) return t('Caribou fee')
  return t('Network service fee')
}

function FeeBreakdown ({ preview, originAsset, destAsset }: {
  preview: IntentsPreview
  originAsset: IntentsAsset
  destAsset: IntentsAsset
}) {
  return (
    <div className="swap-fee-section">
      <div className="fs12 grey demi mb-1">{t('Fee breakdown')}</div>
      {preview.appFees && preview.appFees.map((af, i) => (
        <div key={af.recipient + i} className="d-flex justify-content-between fs12 py-1">
          <span className="grey">{friendlyFeeName(af.recipient, i)}</span>
          <span>{af.fee} {t('bps')}</span>
        </div>
      ))}
      {preview.refundFee && (
        <div className="d-flex justify-content-between fs12 py-1">
          <span className="grey">{t('Refund fee (if refund needed)')}</span>
          <span>{formatIntentsAmount(preview.refundFee, originAsset.decimals)} {originAsset.symbol}</span>
        </div>
      )}
      {preview.withdrawFee && (
        <div className="d-flex justify-content-between fs12 py-1">
          <span className="grey">{t('Withdrawal fee')}</span>
          <span>{formatIntentsAmount(preview.withdrawFee, destAsset.decimals)} {destAsset.symbol}</span>
        </div>
      )}
    </div>
  )
}

// ---- Swap history item ----

function SwapHistoryItem ({ swap, assets, onRefresh }: {
  swap: IntentsSwap
  assets: IntentsAsset[]
  onRefresh: (depositAddress: string) => void
}) {
  const [refreshing, setRefreshing] = useState(false)
  const [expanded, setExpanded] = useState(isAttention(swap.state))

  const originAsset = assets.find(a => a.assetID === swap.originAssetID)
  const destAsset = assets.find(a => a.assetID === swap.destAssetID)

  const doRefresh = async () => {
    setRefreshing(true)
    await onRefresh(swap.depositAddress)
    setRefreshing(false)
  }

  const statusCls = swapStatusClass(swap.state)
  const needsAttention = isAttention(swap.state)
  const label = swapStatusLabel(swap.state)
  const createdDate = new Date(swap.createdAt).toLocaleString(languages)

  return (
    <div className={`swap-history-item ${needsAttention ? 'swap-history-attention' : ''}`}>
      <div
        className="d-flex align-items-center pointer"
        onClick={() => setExpanded(e => !e)}
      >
        <div className="d-flex align-items-center flex-grow-1">
          {originAsset && <CoinLogo ticker={originAsset.symbol} className="mini-icon me-1" style={{ objectFit: 'contain' }} />}
          <span className="fs13 demi me-1">{originAsset ? formatConventional(swap.atomsIn, originAsset.atomDecimals) : swap.amountIn} {originAsset?.symbol ?? String(swap.originAssetID)}</span>
          <span className="grey fs12 me-1">→</span>
          {destAsset && <CoinLogo ticker={destAsset.symbol} className="mini-icon me-1" style={{ objectFit: 'contain' }} />}
          <span className="fs13 demi">{destAsset ? formatConventional(swap.atomsOut, destAsset.atomDecimals) : swap.amountOut} {destAsset?.symbol ?? String(swap.destAssetID)}</span>
          {swap.reconstructed && (
            <span className="swap-recovered-badge ms-2 fs11">{t('recovered')}</span>
          )}
        </div>
        <div className="d-flex flex-column align-items-end ms-2">
          <span className={`fs12 ${statusCls}`}>{label}</span>
          <span className="fs11 grey">{createdDate}</span>
        </div>
        <span className={`ms-2 fs12 grey ico-${expanded ? 'arrowup' : 'arrowdown'}`} />
      </div>

      {expanded && (
        <div className="swap-history-details mt-2">
          <div className="d-flex justify-content-between fs12 py-1">
            <span className="grey">{t('Status')}</span>
            <span className={statusCls}>{label}</span>
          </div>
          <div className="d-flex justify-content-between fs12 py-1">
            <span className="grey">{t('Deposit address')}</span>
            <span className="mono swap-addr-wrap">{swap.depositAddress}</span>
          </div>
          {swap.depositMemo && (
            <div className="d-flex justify-content-between fs12 py-1">
              <span className="grey">{t('Deposit memo')}</span>
              <span className="mono">{swap.depositMemo}</span>
            </div>
          )}
          <div className="d-flex justify-content-between fs12 py-1">
            <span className="grey">{t('Recipient')}</span>
            <span className="mono swap-addr-wrap">{swap.recipient}</span>
          </div>
          {swap.depositTxID && (
            <div className="d-flex justify-content-between fs12 py-1">
              <span className="grey">{t('Deposit TX')}</span>
              <span className="mono swap-addr-wrap">{swap.depositTxID}</span>
            </div>
          )}
          {swap.nearTxHashes && swap.nearTxHashes.length > 0 && (
            <div className="fs12 py-1">
              <div className="grey mb-1">{t('NEAR transactions')}</div>
              {swap.nearTxHashes.map(h => (
                <div key={h} className="mono swap-addr-wrap">{h}</div>
              ))}
            </div>
          )}
          {swap.destinationTxs && swap.destinationTxs.length > 0 && (
            <div className="fs12 py-1">
              <div className="grey mb-1">{t('Destination transactions')}</div>
              {swap.destinationTxs.map(h => (
                <div key={h} className="mono swap-addr-wrap">{h}</div>
              ))}
            </div>
          )}
          {swap.refundReason && (
            <div className="d-flex justify-content-between fs12 py-1">
              <span className="grey">{t('Refund reason')}</span>
              <span>{swap.refundReason}</span>
            </div>
          )}
          {swap.refundedAmount && originAsset && (
            <div className="d-flex justify-content-between fs12 py-1">
              <span className="grey">{t('Refunded amount')}</span>
              <span>{formatIntentsAmount(swap.refundedAmount, originAsset.decimals)} {originAsset.symbol}</span>
            </div>
          )}
          {swap.refundTxs && swap.refundTxs.length > 0 && (
            <div className="fs12 py-1">
              <div className="grey mb-1">{t('Refund transactions')}</div>
              {swap.refundTxs.map(h => (
                <div key={h} className="mono swap-addr-wrap">{h}</div>
              ))}
            </div>
          )}
          {(needsAttention || !isTerminal(swap.state)) && (
            <div className="swap-escalation-links mt-2 fs12">
              <a
                href={nearExplorerURL(swap.depositAddress)}
                target="_blank"
                rel="noopener noreferrer"
                className="swap-link me-3"
              >
                {t('View on NEAR Intents Explorer')}
              </a>
              <a
                href="https://status.near-intents.org"
                target="_blank"
                rel="noopener noreferrer"
                className="swap-link"
              >
                {t('Service status')}
              </a>
            </div>
          )}
          {!isTerminal(swap.state) && (
            <div className="mt-2">
              <button
                type="button"
                className="small"
                onClick={doRefresh}
                disabled={refreshing}
              >
                {refreshing
                  ? <><Spinner size={11} className="me-1" />{t('Refreshing...')}</>
                  : t('Refresh status')}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

// ---- Confirmation step ----

function ConfirmSwap ({
  originAsset,
  destAsset,
  preview,
  submitting,
  err,
  requirePW,
  pw,
  setPW,
  onConfirm,
  onBack
}: {
  originAsset: IntentsAsset
  destAsset: IntentsAsset
  preview: IntentsPreview
  submitting: boolean
  err: string
  requirePW: boolean
  pw: string
  setPW: (pw: string) => void
  onConfirm: () => void
  onBack: () => void
}) {
  return (
    <form
      className="modal form-width-300"
      onSubmit={(e: FormEvent) => { e.preventDefault(); onConfirm() }}
    >
      <header>
        <span className="ico-swap me-2 fs18" />
        <span>{t('Confirm swap')}</span>
      </header>

      <div className="px-3 pb-2">
        <div className="swap-confirm-pair d-flex align-items-center justify-content-center mb-3">
          <CoinLogo ticker={originAsset.symbol} className="mini-icon" style={{ objectFit: 'contain' }} />
          <div className="ms-2 me-3">
            <div className="fs16 demi">{formatConventional(preview.atomsIn, originAsset.atomDecimals)} {originAsset.symbol}</div>
            <div className="fs12 grey">{preview.amountInUSD ? usdFmt.format(parseFloat(preview.amountInUSD)) : '—'}</div>
          </div>
          <span className="ico-arrowright fs18 grey me-3" />
          <CoinLogo ticker={destAsset.symbol} className="mini-icon" style={{ objectFit: 'contain' }} />
          <div className="ms-2">
            <div className="fs16 demi">{formatConventional(preview.atomsOut, destAsset.atomDecimals)} {destAsset.symbol}</div>
            <div className="fs12 grey">{preview.amountOutUSD ? usdFmt.format(parseFloat(preview.amountOutUSD)) : '—'}</div>
          </div>
        </div>

        <div className="swap-summary fs13">
          <div className="d-flex justify-content-between py-1">
            <span className="grey">{t('Min received')}</span>
            <span>{formatConventional(preview.minAtomsOut, destAsset.atomDecimals)} {destAsset.symbol}</span>
          </div>
          <div className="d-flex justify-content-between py-1">
            <span className="grey">{t('Estimated time')}</span>
            <span>{formatDuration(preview.timeEstimate)}</span>
          </div>
          {preview.appFees && preview.appFees.map((af, i) => (
            <div key={af.recipient + i} className="d-flex justify-content-between py-1">
              <span className="grey">{friendlyFeeName(af.recipient, i)}</span>
              <span>{af.fee} {t('bps')}</span>
            </div>
          ))}
          {preview.refundFee && (
            <div className="d-flex justify-content-between py-1">
              <span className="grey">{t('Refund fee')}</span>
              <span>{formatIntentsAmount(preview.refundFee, originAsset.decimals)} {originAsset.symbol}</span>
            </div>
          )}
          {preview.withdrawFee && (
            <div className="d-flex justify-content-between py-1">
              <span className="grey">{t('Withdrawal fee')}</span>
              <span>{formatIntentsAmount(preview.withdrawFee, destAsset.decimals)} {destAsset.symbol}</span>
            </div>
          )}
        </div>

        <div className="swap-custody-notice mt-2">
          <span className="fs12 grey">
            {t('Funds are in the custody of NEAR Intents while the swap completes. Your seed-derived addresses are always the refund and destination recipients.')}
          </span>
        </div>
      </div>

      {requirePW && (
        <div className="px-3 pb-2">
          <label className="fs12 grey">{t('App password')}</label>
          <PasswordInput
            value={pw}
            onChange={e => setPW(e.target.value)}
            disabled={submitting}
            autoFocus
          />
        </div>
      )}

      {err && (
        <div key={err} className="form-err errcolor fs12 px-3 pb-1 text-center">{err}</div>
      )}

      <div className="flex-stretch-column p-3">
        <button type="submit" className="feature" disabled={submitting || (requirePW && !pw)}>
          {submitting
            ? <span className="d-inline-flex align-items-center justify-content-center">
                <Spinner className="me-2" />{t('Sending deposit...')}
              </span>
            : t('Confirm swap')}
        </button>
        <button type="button" className="mt-2" onClick={onBack} disabled={submitting}>{t('Back')}</button>
      </div>
    </form>
  )
}

// ---- Main Swap component ----

type SwapStep = 'form' | 'confirm' | 'done'

export default function Swap () {
  const close = () => app.setForm({ form: '', data: {} })

  // First-use disclosure state
  const [showDisclosure, setShowDisclosure] = useState(
    !State.fetchLocal(INTENTS_DISCLOSURE_SEEN_KEY)
  )

  const dismissDisclosure = () => {
    State.storeLocal(INTENTS_DISCLOSURE_SEEN_KEY, true)
    setShowDisclosure(false)
  }

  // Assets from /api/intents/assets
  const [assets, setAssets] = useState<IntentsAsset[]>([])
  const [assetsLoading, setAssetsLoading] = useState(true)
  const [assetsErr, setAssetsErr] = useState('')

  // Pair selection
  const [originID, setOriginID] = useState<number | null>(null)
  const [destID, setDestID] = useState<number | null>(null)

  // Amount in conventional display units
  const [amount, setAmount] = useState('')

  // Preview (dry quote)
  const [preview, setPreview] = useState<IntentsPreview | null>(null)
  const [previewing, setPreviewing] = useState(false)
  const [previewErr, setPreviewErr] = useState('')
  // Bumped to force a fresh preview with unchanged inputs, e.g. after the
  // server rejects an execute because the rate moved past the previewed
  // minimum.
  const [previewNonce, setPreviewNonce] = useState(0)

  // Trust popover toggle
  const [showTrust, setShowTrust] = useState(false)

  // Swap step
  const [step, setStep] = useState<SwapStep>('form')

  // Execution state
  const [submitting, setSubmitting] = useState(false)
  const [execErr, setExecErr] = useState('')
  const [doneSwap, setDoneSwap] = useState<IntentsSwap | null>(null)

  // Password
  const requirePW = app.appSettings.sendRequiresPassword
  const [pw, setPW] = useState('')

  // History
  const [swaps, setSwaps] = useState<IntentsSwap[]>([])
  const [swapsLoading, setSwapsLoading] = useState(true)

  // Monotonic guard for debounced preview
  const previewRef = useRef(0)

  // Slippage — fixed at 100 bps (1%) matching the engine default
  const slippageBps = 100

  // Load assets on mount
  useEffect(() => {
    let cancelled = false
    const load = async () => {
      setAssetsLoading(true)
      const r = await getJSON('/api/intents/assets')
      if (cancelled) return
      setAssetsLoading(false)
      if (!r.ok) {
        setAssetsErr(r.errMsg || r.msg || t('Failed to load swap assets.'))
        return
      }
      setAssets(r.assets ?? [])
    }
    load()
    return () => { cancelled = true }
  }, [])

  // Load swap history on mount
  useEffect(() => {
    let cancelled = false
    const load = async () => {
      const r = await getJSON('/api/intents/swaps')
      if (cancelled) return
      setSwapsLoading(false)
      if (r.ok) setSwaps(r.swaps ?? [])
    }
    load()
    return () => { cancelled = true }
  }, [])

  // Subscribe to intentsswap notifications
  useEffect(() => {
    const k = app.registerNoteFeed((note: CoreNote) => {
      if (note.type !== 'intentsswap') return
      const swap: IntentsSwap = (note as any).swap
      if (!swap) return
      setSwaps(prev => {
        const idx = prev.findIndex(s => s.depositAddress === swap.depositAddress)
        if (idx >= 0) {
          const next = [...prev]
          next[idx] = swap
          return next
        }
        return [swap, ...prev]
      })
    })
    return () => app.unregisterNoteFeed(k)
  }, [])

  // Debounced preview: fires when origin, dest, or amount changes
  useEffect(() => {
    setPreview(null)
    setPreviewErr('')
    if (originID === null || destID === null || originID === destID) return
    const originAsset = assets.find(a => a.assetID === originID)
    if (!originAsset) return
    const v = parseFloat(amount)
    if (!Number.isFinite(v) || v <= 0) return
    const atoms = conventionalToAtoms(v, originAsset.atomDecimals)
    if (atoms <= 0) return

    const id = ++previewRef.current
    setPreviewing(true)
    const h = setTimeout(async () => {
      const r = await postJSON('/api/intents/preview', {
        originAssetID: originID,
        destAssetID: destID,
        value: atoms,
        slippageBps
      })
      if (id !== previewRef.current) return
      setPreviewing(false)
      if (!r.ok) {
        setPreviewErr(r.errMsg || r.msg || t('Could not get a quote. The pair may not be available or the amount may be too small.'))
        return
      }
      setPreview(r.preview)
    }, 500)
    return () => { clearTimeout(h); setPreviewing(false) }
  }, [originID, destID, amount, assets, slippageBps, previewNonce])

  const originAsset = assets.find(a => a.assetID === originID) ?? null
  const destAsset = assets.find(a => a.assetID === destID) ?? null

  // atoms for the current amount (0 if invalid)
  const atomsIn = (() => {
    if (!originAsset) return 0
    const v = parseFloat(amount)
    if (!Number.isFinite(v) || v <= 0) return 0
    return conventionalToAtoms(v, originAsset.atomDecimals)
  })()

  const canPreview = originID !== null && destID !== null && originID !== destID && atomsIn > 0
  const canConfirm = canPreview && preview !== null && !previewing && !previewErr

  // Confirm → execute. The previewed minimum output is echoed so the
  // server rejects a committed quote that pays less than the terms shown
  // here. On failure, return to the form (not the confirm modal) with the
  // error shown and a fresh preview requested, so a retry is a deliberate
  // review-and-confirm against current numbers rather than a stray click on
  // a modal that re-materializes when the refreshed quote arrives.
  const executeSwap = async () => {
    if (!originAsset || !destAsset || atomsIn <= 0 || submitting || !preview) return
    setSubmitting(true)
    setExecErr('')
    const r = await postJSON('/api/intents/execute', {
      originAssetID: originID,
      destAssetID: destID,
      value: atomsIn,
      slippageBps,
      minAtomsOut: preview.minAtomsOut,
      pass: pw
    })
    setSubmitting(false)
    if (!r.ok) {
      setExecErr(r.errMsg || r.msg || t('Swap failed. Please try again.'))
      setStep('form')
      setPreviewNonce(n => n + 1)
      return
    }
    const swap: IntentsSwap = r.swap
    setDoneSwap(swap)
    setSwaps(prev => {
      const idx = prev.findIndex(s => s.depositAddress === swap.depositAddress)
      if (idx >= 0) {
        const next = [...prev]; next[idx] = swap; return next
      }
      return [swap, ...prev]
    })
    setStep('done')
  }

  // Refresh one swap
  const refreshSwap = useCallback(async (depositAddress: string) => {
    const r = await postJSON('/api/intents/refresh', { depositAddress })
    if (r.ok && r.swap) {
      setSwaps(prev => {
        const idx = prev.findIndex(s => s.depositAddress === r.swap.depositAddress)
        if (idx >= 0) {
          const next = [...prev]; next[idx] = r.swap; return next
        }
        return [r.swap, ...prev]
      })
    }
  }, [])

  // ---- Render: first-use disclosure ----
  if (showDisclosure) {
    return (
      <form className="modal form-width-300" onSubmit={e => e.preventDefault()}>
        <header>
          <span className="ico-swap me-2 fs18" />
          <span>{t('Swap')}</span>
        </header>
        <CustodyModal onClose={dismissDisclosure} />
        <div className="flex-stretch-column p-3">
          <button type="button" onClick={close}>{t('Cancel')}</button>
        </div>
      </form>
    )
  }

  // ---- Render: confirmation step ----
  if (step === 'confirm' && originAsset && destAsset && preview) {
    return (
      <ConfirmSwap
        originAsset={originAsset}
        destAsset={destAsset}
        preview={preview}
        submitting={submitting}
        err={execErr}
        requirePW={requirePW}
        pw={pw}
        setPW={setPW}
        onConfirm={executeSwap}
        onBack={() => { setStep('form'); setExecErr('') }}
      />
    )
  }

  // ---- Render: done step ----
  if (step === 'done' && doneSwap) {
    const orig = assets.find(a => a.assetID === doneSwap.originAssetID)
    const dest = assets.find(a => a.assetID === doneSwap.destAssetID)
    return (
      <form className="modal form-width-300" onSubmit={e => e.preventDefault()}>
        <header>
          <span className="ico-swap me-2 fs18" />
          <span>{t('Swap sent')}</span>
        </header>
        <div className="px-3 pb-3 text-center">
          <div className="fs14 mb-3">
            {tvar('Your {{ origin }} deposit has been sent. Swapped {{ dest }} will arrive at your wallet address when the swap completes.', {
              origin: orig?.symbol ?? String(doneSwap.originAssetID),
              dest: dest?.symbol ?? String(doneSwap.destAssetID)
            })}
          </div>
          <div className="swap-custody-notice mb-3">
            <span className="fs12 grey">
              {t('Funds are in the custody of NEAR Intents during the swap — typically a few minutes. Your seed-derived address is always the recipient.')}
            </span>
          </div>
          <div className="fs12 grey mb-2">
            <span className="demi">{t('Deposit address:')}</span>
            <div className="mono swap-addr-wrap mt-1">{doneSwap.depositAddress}</div>
          </div>
          <a
            href={nearExplorerURL(doneSwap.depositAddress)}
            target="_blank"
            rel="noopener noreferrer"
            className="swap-link fs12"
          >
            {t('Track on NEAR Intents Explorer')}
          </a>
        </div>
        <div className="flex-stretch-column p-3">
          <button type="button" className="feature" onClick={() => {
            setStep('form')
            setAmount('')
            setPreview(null)
            setPW('')
            setDoneSwap(null)
          }}>{t('New swap')}</button>
          <button type="button" className="mt-2" onClick={close}>{t('Close')}</button>
        </div>
      </form>
    )
  }

  // ---- Render: main swap form ----
  const readyOriginAssets = assets.filter(a => a.walletReady)
  const availableDestAssets = assets.filter(a => a.assetID !== originID)

  return (
    <form
      className="modal swap-modal"
      onSubmit={e => e.preventDefault()}
    >
      <header>
        <span className="ico-swap me-2 fs18" />
        <span>{t('Swap')}</span>
      </header>

      {assetsLoading && (
        <div className="flex-center p-4">
          <Spinner size={20} />
        </div>
      )}

      {assetsErr && (
        <div className="errcolor fs13 px-3 pb-3 text-center">{assetsErr}</div>
      )}

      {!assetsLoading && !assetsErr && (
        <>
          {/* Custody notice */}
          <div className="px-3 pb-2">
            <CustodyNotice
              originAsset={originAsset ?? undefined}
              destAsset={destAsset ?? undefined}
              showDetails={showTrust}
              onToggleDetails={() => setShowTrust(s => !s)}
            />
          </div>

          {/* Origin picker */}
          <div className="px-3 pb-2">
            <div className="fs12 grey mb-1">{t('From')}</div>
            <div className="form-input-bg br5 d-flex align-items-center swap-asset-pick">
              <select
                value={originID ?? ''}
                onChange={e => {
                  const id = parseInt(e.target.value, 10)
                  setOriginID(isNaN(id) ? null : id)
                  setPreview(null)
                  setPreviewErr('')
                  setAmount('')
                  if (destID === id) setDestID(null)
                }}
              >
                <option value="">{t('Select asset')}</option>
                {readyOriginAssets.map(a => (
                  <option key={a.assetID} value={a.assetID}>{a.symbol}</option>
                ))}
                {assets.filter(a => !a.walletReady).map(a => (
                  <option key={a.assetID} value={a.assetID} disabled>
                    {a.symbol} — {t('wallet not available')}
                  </option>
                ))}
              </select>
              {originAsset && (
                <CoinLogo
                  ticker={originAsset.symbol}
                  className="mini-icon ms-2"
                  style={{ objectFit: 'contain', flexShrink: 0 }}
                />
              )}
            </div>
          </div>

          {/* Amount input */}
          {originAsset && (
            <div className="px-3 pb-2">
              <div className="fs12 grey mb-1">{t('Amount')}</div>
              <div className="form-input-bg br5 d-flex align-items-center send-amt">
                <input
                  type="text"
                  inputMode="decimal"
                  value={amount}
                  placeholder="0.00"
                  onChange={e => {
                    setAmount(e.target.value)
                    setPreviewErr('')
                    setPreview(null)
                  }}
                />
                <span className="grey fs14 px-2">{originAsset.symbol}</span>
              </div>
              {originAsset.priceUSD > 0 && atomsIn > 0 && (
                <div className="fs12 grey pt-1">
                  {usdFmt.format(atomsToConventional(atomsIn, originAsset.atomDecimals) * originAsset.priceUSD)}
                </div>
              )}
            </div>
          )}

          {/* Destination picker */}
          <div className="px-3 pb-2">
            <div className="fs12 grey mb-1">{t('To')}</div>
            <div className="form-input-bg br5 d-flex align-items-center swap-asset-pick">
              <select
                value={destID ?? ''}
                onChange={e => {
                  const id = parseInt(e.target.value, 10)
                  setDestID(isNaN(id) ? null : id)
                  setPreview(null)
                  setPreviewErr('')
                }}
              >
                <option value="">{t('Select asset')}</option>
                {availableDestAssets.map(a => (
                  <option key={a.assetID} value={a.assetID}>{a.symbol}</option>
                ))}
              </select>
              {destAsset && (
                <CoinLogo
                  ticker={destAsset.symbol}
                  className="mini-icon ms-2"
                  style={{ objectFit: 'contain', flexShrink: 0 }}
                />
              )}
            </div>
          </div>

          {/* Preview panel */}
          {canPreview && (
            <div className="px-3 pb-2 swap-preview">
              {previewing && (
                <div className="d-flex align-items-center grey fs12">
                  <Spinner size={12} className="me-2" />{t('Getting quote...')}
                </div>
              )}
              {previewErr && (
                <div className="errcolor fs12">{previewErr}</div>
              )}
              {preview && destAsset && originAsset && (
                <>
                  <div className="d-flex justify-content-between fs13 py-1">
                    <span className="grey">{t('You receive')}</span>
                    <span className="demi">
                      {formatConventional(preview.atomsOut, destAsset.atomDecimals)} {destAsset.symbol}
                    </span>
                  </div>
                  {preview.amountOutUSD && (
                    <div className="d-flex justify-content-between fs12 py-1">
                      <span className="grey">{t('Value')}</span>
                      <span>{usdFmt.format(parseFloat(preview.amountOutUSD))}</span>
                    </div>
                  )}
                  <div className="d-flex justify-content-between fs12 py-1">
                    <span className="grey">{t('Min received (1% slippage)')}</span>
                    <span>{formatConventional(preview.minAtomsOut, destAsset.atomDecimals)} {destAsset.symbol}</span>
                  </div>
                  <div className="d-flex justify-content-between fs12 py-1">
                    <span className="grey">{t('Estimated time')}</span>
                    <span>{formatDuration(preview.timeEstimate)}</span>
                  </div>
                  <FeeBreakdown preview={preview} originAsset={originAsset} destAsset={destAsset} />
                </>
              )}
            </div>
          )}

          <div className="flex-stretch-column px-3 pb-3">
            {execErr && (
              <div className="errcolor fs12 text-center mb-2">{execErr}</div>
            )}
            <button
              type="button"
              className="feature"
              disabled={!canConfirm}
              onClick={() => { setStep('confirm'); setExecErr(''); setPW('') }}
            >
              {t('Review swap')}
            </button>
            <button type="button" className="mt-2" onClick={close}>{t('Cancel')}</button>
          </div>
        </>
      )}

      {/* Swap history */}
      {!assetsLoading && (
        <div className="swap-history">
          <div className="swap-history-header fs11 grey demi px-3 py-2">
            {t('SWAP HISTORY')}
          </div>
          {swapsLoading && (
            <div className="flex-center py-3">
              <Spinner size={14} />
            </div>
          )}
          {!swapsLoading && swaps.length === 0 && (
            <div className="fs12 grey text-center py-3">{t('No swaps yet.')}</div>
          )}
          {swaps.map(s => (
            <SwapHistoryItem
              key={s.depositAddress}
              swap={s}
              assets={assets}
              onRefresh={refreshSwap}
            />
          ))}
        </div>
      )}
    </form>
  )
}
