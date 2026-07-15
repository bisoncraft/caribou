declare global {
  interface Window {
    log: (...args: any) => void
    enableLogger: (loggerID: string, enable: boolean) => void
    recordLogger: (loggerID: string, enable: boolean) => void
    dumpLogger: (loggerID: string) => void
    user: () => User
    isWebview?: () => boolean
    webkit: any | undefined
    electron: any | undefined
    openUrl: (url: string) => void
    open: (url?: string | URL, target?: string, features?: string) => WindowProxy | null
    sendOSNotification (title: string, body?: string): void
    clearLocale (): void
    recordMissingTranslations (enable?: boolean): void
    dumpMissingTranslations (): void
  }
}

export interface SupportedAsset {
  id: number
  symbol: string
  name: string
  wallet: WalletState
  info?: WalletInfo
  token?: Token
  unitInfo: UnitInfo
  // tickerGroup is the normalized ticker that groups this asset with its
  // deployments on other networks, with wrapped natives folded onto their base
  // asset (WETH -> ETH). Computed by the backend (core.normalizeTickerUnit),
  // which is the single source of the mapping.
  tickerGroup: string
}

export interface Token {
  networkID: number
  name: string
  unitInfo: UnitInfo
  contractAddress: string
  definition: WalletDefinition
  supportedAssetVersions: number[]
}

export enum ApprovalStatus {
  Approved = 0,
  Pending = 1,
  NotApproved = 2
}

// FeeState mirrors core.FeeState, the response shape of /api/feestate. It is
// fetched on demand; it is not part of WalletState.
export interface FeeState {
  rate: number
  send: number
  stampMS: number
}

export interface SyncStatus {
  synced: boolean
  targetHeight: number
  startingBlocks: number
  blocks: number
  txs: number | undefined
}

// traitWithdrawer marks wallets that subtract the network fee from the sent
// amount (e.g. account-based chains). Mirrors the asset.WalletTrait bit in Go;
// test against WalletState.traits.
export const traitWithdrawer = 1 << 6

export interface WalletState {
  symbol: string
  assetID: number
  version: number
  type: string
  class: string
  traits: number
  open: boolean
  running: boolean
  disabled: boolean
  balance: WalletBalance
  address: string
  encrypted: boolean
  peerCount: number
  synced: boolean
  syncProgress: number
  syncStatus: SyncStatus
  approved: Record<number, ApprovalStatus>
  pendingTxs: Record<string, WalletTransaction>
}

export interface NetworkInfo {
  id: number
  nativeAssetID: number
  priority: boolean
}

export interface WalletInfo {
  name: string
  version: number
  availablewallets: WalletDefinition[]
  versions: number[]
  unitinfo: UnitInfo
  network?: NetworkInfo
}

export interface WalletBalance {
  available: number
  immature: number
  locked: number
  stamp: string // time.Time
  other: Record<string, CustomBalance>
}

export interface CustomBalance {
  amt: number
  locked: boolean
}

export interface WalletDefinition {
  seeded: boolean
  type: string
  tab: string
  description: string
  configpath: string
  configopts: ConfigOption[]
  noauth: boolean
  guidelink: string
}

export interface ConfigOption {
  key: string
  displayname: string
  description: string
  default: any
  max: any
  min: any
  noecho: boolean
  isboolean: boolean
  isdate: boolean
  disablewhenactive: boolean
  isBirthdayConfig: boolean
  repeatable?: string
  repeatN?: number
  regAsset?: number
  required?: boolean
  dependsOn?: string
}

export interface UnitInfo {
  atomicUnit: string
  conventional: Denomination
  denominations: Denomination[]
  feeRateDenom: string
}

export interface Denomination {
  unit: string
  conversionFactor: number
}

export interface User {
  inited: boolean
  seedgentime: number
  assets: Record<number, SupportedAsset>
  fiatRates: Record<number, number>
  tickerRates: Record<string, number>
  net: number
}

export interface CoreNote {
  type: string
  topic: string
  subject: string
  details: string
  severity: number
  stamp: number
  acked: boolean
  id: string
  data: any
}

export interface BalanceNote extends CoreNote {
  assetID: number
  balance: WalletBalance
}

export interface RateNote extends CoreNote {
  fiatRates: Record<number, number>
  tickerRates: Record<string, number>
}

export interface WalletConfigNote extends CoreNote {
  wallet: WalletState
}

export interface WalletSyncNote extends CoreNote {
  assetID: number
  syncStatus: SyncStatus
  syncProgress: number
}

export type WalletStateNote = WalletConfigNote

export interface WalletCreationNote extends CoreNote {
  assetID: number
}

export interface BaseWalletNote {
  route: string
  assetID: number
}

export interface TipChangeNote extends BaseWalletNote {
  tip: number
  data: any
}

export interface CustomWalletNote extends BaseWalletNote {
  payload: any
}

export interface TransactionNote extends BaseWalletNote {
  transaction: WalletTransaction
  new: boolean
}

export interface ActionRequiredNote extends BaseWalletNote {
  uniqueID: string
  actionID: string
  payload: any
}

export interface ActionResolvedNote extends BaseWalletNote {
  uniqueID: string
}

export interface TransactionActionNote {
  tx: WalletTransaction
  nonce: number
  newFees: number
}

export interface WalletNote extends CoreNote {
  payload: BaseWalletNote
}

export interface CoreActionRequiredNote extends CoreNote {
  payload: ActionRequiredNote
}

export interface APIResponse {
  requestSuccessful: boolean
  ok: boolean
  msg: string
  err?: string
}

export interface LogMessage {
  time: string
  msg: string
}

export interface NoteElement extends HTMLElement {
  note: CoreNote
}

export interface BalanceResponse extends APIResponse {
  balance: WalletBalance
}

export interface LayoutMetrics {
  bodyTop: number
  bodyLeft: number
  width: number
  height: number
  centerX: number
  centerY: number
}

export interface PageElement extends HTMLElement {
  value?: string
  src?: string
  files?: FileList
  checked?: boolean
  href?: string
  htmlFor?: string
  name?: string
  options?: HTMLOptionElement[]
  selectedIndex?: number
  disabled?: boolean
}

// changing the order of the elements in this enum will affect
// the sorting of the peers table in wallets.ts.
export enum PeerSource {
  WalletDefault,
  UserAdded,
  Discovered,
}

export interface WalletPeer {
  addr: string
  source: PeerSource
  connected: boolean
}

export interface WalletTransaction {
  type: number
  id: string
  amount: number
  fees: number
  timestamp: number
  blockNumber: number
  tokenID?: number
  // assetID identifies the transaction's exact asset (token ID for tokens,
  // the network's native asset otherwise). Only populated on cross-network
  // ticker-history responses, where it attributes each transaction to its
  // network.
  assetID?: number
  recipient?: string
  additionalData: Record<string, string>
  isUserOp: boolean
  userOpTxID: string
  confirmed: boolean
  confirms?: {
    current: number
    target: number
  }
  rejected: boolean
}

export interface TxHistoryRequest {
  n: number
  refID?: string
  past: boolean
  ignoreTypes?: number[]
}

export interface TxHistoryResult {
  txs: WalletTransaction[]
  moreAvailable: boolean
}

// AppSettings holds user-configurable application settings surfaced on the
// Settings page and returned with the /api/user response.
export interface AppSettings {
  sendRequiresPassword: boolean
}

export interface AppState {
  user: User
  lang: string
  langs: string[]
  inited: boolean
  ok: boolean
  onionUrl: string
  appSettings: AppSettings
}

export interface PageData {
  pageParts: string[]
  pageRoot: string
  data: any
  query: URLSearchParams
}

// PortfolioTotals is a fiat-valued breakdown of the portfolio, priced per
// ticker group. The parts decompose the whole: total = available + locked +
// immature.
export interface PortfolioTotals {
  total: number
  available: number
  locked: number
  immature: number
  numWallets: number
  ratesComplete: boolean
}

export interface FormData {
  form: string
  data: any
}
