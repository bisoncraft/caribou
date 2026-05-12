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

export enum ConnectionStatus {
  Disconnected = 0,
  Connected = 1,
  InvalidCert = 2,
}

export interface Asset {
  id: number
  symbol: string
  version: number
  maxFeeRate: number
  swapSize: number
  swapSizeBase: number
  redeemSize: number
  swapConf: number
  unitInfo: UnitInfo
}

export interface SupportedAsset {
  id: number
  symbol: string
  name: string
  wallet: WalletState
  info?: WalletInfo
  token?: Token
  unitInfo: UnitInfo
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

export interface FeeState {
  rate: number
  send: number
  swap: number
  redeem: number
  refund: number
  stampMS: number
}

export interface SyncStatus {
  synced: boolean
  targetHeight: number
  startingBlocks: number
  blocks: number
  txs: number | undefined
}

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
  bridgeApproved?: Record<string, ApprovalStatus>
  feeState?: FeeState
  pendingTxs: Record<string, WalletTransaction>
}

export interface WalletInfo {
  name: string
  version: number
  availablewallets: WalletDefinition[]
  versions: number[]
  emptyidx: number
  unitinfo: UnitInfo
}

export interface WalletBalance {
  available: number
  immature: number
  locked: number
  stamp: string // time.Time
  orderlocked?: number
  contractlocked?: number
  bondlocked?: number
  bondReserves?: number
  reservesDeficit?: number
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
  multifundingopts: OrderOption[]
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

export interface ExtensionConfiguredWallet {
  hiddenFields: string[]
  disableWalletType: boolean
  disablePassword: boolean
  disableStaking: boolean
  disablePrivacy: boolean
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

export interface BridgeNote extends CoreNote {
  sourceAssetID: number
  destAssetID: number
  txID: string
  completionTxIDs: string[]
  amount: number
  complete: boolean
}

export interface BalanceNote extends CoreNote {
  assetID: number
  balance: WalletBalance
}

export interface RateNote extends CoreNote {
  fiatRates: Record<number, number>
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

export interface RejectedTxData {
  assetID: number
  orderID: string
  coinID: string
  coinFmt: string
  txType: string
}

export interface RecentMatch {
  rate: number
  qty: number
  stamp: number
  sell: boolean
}

export interface EpochNote extends CoreNote {
  host: string
  marketID: string
  epoch: number
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

export interface BooleanConfig {
  reason: string
}

export interface XYRangePoint {
  label: string
  x: number
  y: number
}

export interface XYRange {
  start: XYRangePoint
  end: XYRangePoint
  xUnit: string
  yUnit: string
  roundX?: boolean
  roundY?: boolean
}

export interface OrderOption extends ConfigOption {
  boolean?: BooleanConfig
  xyRange?: XYRange
  showByDefault?: boolean
  quoteAssetOnly?: boolean
}

export interface SwapEstimate {
  lots: number
  value: number
  maxFees: number
  realisticWorstCase: number
  realisticBestCase: number
  feeReservesPerLot: number
}

export interface RedeemEstimate {
  realisticBestCase: number
  realisticWorstCase: number
}

export interface PreSwap {
  estimate: SwapEstimate
  options: OrderOption[]
}

export interface PreRedeem {
  estimate: RedeemEstimate
  userOpRequired: boolean
  options: OrderOption[]
}

export interface OrderEstimate {
  swap: PreSwap
  redeem: PreRedeem
}

export interface MaxOrderEstimate {
  swap: SwapEstimate
  redeem: RedeemEstimate
}

export interface MaxSell {
  maxSell: MaxOrderEstimate
}

export interface MaxBuy {
  maxBuy: MaxOrderEstimate
}

export interface LotFees {
  swap: number
  redeem: number
  refund: number
}

export interface LotFeeRange {
  max: LotFees
  estimated: LotFees
}

export interface AssetBookingFees extends LotFeeRange {
  bookingFeesPerLot: number
  bookingFeesPerCounterLot: number
  bookingFees: number
  swapReservesFactor: number // (1 + orderReservesFactor)
  redeemReservesFactor: number
  tokenFeesPerSwap: number
}

export interface BookingFees {
  base: AssetBookingFees
  quote: AssetBookingFees
}

export interface MarketReport {
  price: number
  oracles: OracleReport[]
  baseFiatRate: number
  quoteFiatRate: number
  baseFees: LotFeeRange
  quoteFees: LotFeeRange
}

export interface Candle {
  startStamp: number
  endStamp: number
  matchVolume: number
  quoteVolume: number
  highRate: number
  lowRate: number
  startRate: number
  endRate: number
}

export interface CandlesPayload {
  dur: string
  ms: number
  candles: Candle[]
}

export interface OracleReport {
  host: string
  usdVol: number
  bestBuy: number
  bestSell: number
}

export interface ExchangeBalance {
  available: number
  locked: number
}

// changing the order of the elements in this enum will affect
// the sorting of the peers table in wallets.ts.
export enum PeerSource {
  WalletDefault,
  UserAdded,
  Discovered,
}

export interface Amount {
  atoms: number
  conventional: number
  fmt: string
  usd: number
  fmtUSD: string
}

export interface ProfitLoss {
  initial: Record<number, Amount>
  initialUSD: number
  mods: Record<number, Amount>
  modsUSD: number
  final: Record<number, Amount>
  finalUSD: number
  diffs: Record<number, Amount>
  profit: number
  profitRatio: number
}

export interface WalletPeer {
  addr: string
  source: PeerSource
  connected: boolean
}

export interface TicketTransaction {
  hash: string
  ticketPrice: number
  fees: number
  stamp: number
  blockHeight: number
}

export interface Ticket {
  tx: TicketTransaction
  status: number
  spender: string
}

export interface TBChoice {
  id: string
  description: string
}

export interface TBAgenda {
  id: string
  description: string
  currentChoice: string
  choices: TBChoice[]
}

export interface TKeyPolicyResult {
  key: string
  policy: string
  ticket?: string
}

export interface TBTreasurySpend {
  hash: string
  value: number
  currentPolicy: string
}

export interface Stances {
  agendas: TBAgenda[]
  tspends: TBTreasurySpend[]
  treasuryKeys: TKeyPolicyResult[]
}

export interface TicketStats {
  totalRewards: number
  ticketCount: number
  votes: number
  revokes: number
  mempool: number
  queued: number
}

export interface TicketStakingStatus {
  ticketPrice: number
  votingSubsidy: number
  vsp: string
  isRPC: boolean
  tickets: Ticket[]
  stances: Stances
  stats: TicketStats
}

// VotingServiceProvider is information about a voting service provider.
export interface VotingServiceProvider {
  url: string
  network: number
  launched: number
  lastUpdated: number
  apiVersions: number[]
  feePercentage: number
  closed: boolean
  voting: number
  voted: number
  revoked: number
  vspdVersion: string
  blockHeight: number
  netShare: number
}

export interface BondTxInfo {
  bondID: string
  lockTime: number
  accountID: string
}

export interface BridgeCounterpartTx {
  assetID: number
  ids: string[]
  complete: boolean
  amountReceived: number
  fees: number
}

export interface BridgeFeesAndLimits {
  fees: Record<number, number>
  minLimit: number
  maxLimit: number
  hasLimits: boolean
}

export interface BridgeResult {
  ok: boolean
  txID?: string
  msg?: string
}

export interface BridgeApprovalStatusResult {
  ok: boolean
  status: number // 0=Approved, 1=Pending, 2=NotApproved
  msg?: string
}

export interface BridgeApprovalResult {
  ok: boolean
  txID?: string
  msg?: string
}

// Bridge approval status constants
export const BridgeApprovalApproved = 0
export const BridgeApprovalPending = 1
export const BridgeApprovalNotApproved = 2

export interface WalletTransaction {
  type: number
  id: string
  amount: number
  fees: number
  timestamp: number
  blockNumber: number
  tokenID?: number
  recipient?: string
  bondInfo?: BondTxInfo
  additionalData: Record<string, string>
  isUserOp: boolean
  userOpTxID: string
  bridgeCounterpartTx?: BridgeCounterpartTx
  bridgeName?: string
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

export const PrepaidBondID = 2147483647

export interface AppState {
  user: User
  lang: string
  langs: string[]
  inited: boolean
  ok: boolean
  onionUrl: string
}

export interface PageData {
  pageParts: string[]
  pageRoot: string
  data: any
  query: URLSearchParams
}

export interface TotalUSDBalance {
  ok: boolean
  total: number
  numContribs: number
}

export interface PortfolioTotals {
  total: number
  available: number
  locked: number
  numWallets: number
  ratesComplete: boolean
}

export interface FormData {
  form: string
  data: any
}
