# NEAR Wallet Hardening and NEAR Intents Swap Integration

Two-track plan: bring the NEAR wallet to parity with the most complete
account-based wallets in the codebase, and integrate cross-chain swaps
through [NEAR Intents](https://docs.near-intents.org/) via the 1Click API.
Work lands on the `near-phase-two` branch.

## Background

NEAR Intents settles swaps atomically inside the `intents.near` verifier
contract on NEAR mainnet. Assets from other chains enter and exit through
bridges, and the 1Click REST service (`https://1click.chaindefuser.com/v0`)
wraps the whole pipeline behind a deposit-address flow:

1. `POST /v0/quote` returns a quoted rate and a single-use deposit address
   on the origin chain.
2. The user sends an ordinary transaction to the deposit address.
3. The service bridges, settles against a solver, and delivers funds to the
   recipient address on the destination chain. `GET /v0/status` tracks
   progress through `PENDING_DEPOSIT → KNOWN_DEPOSIT_TX → PROCESSING →
   SUCCESS | REFUNDED | FAILED | INCOMPLETE_DEPOSIT`.

No NEAR account, gas, or signature is required from the user for any
origin/destination pair; the only user actions are an origin-chain send and
(implicitly) owning the destination address. This makes the integration
primarily a core-level feature rather than a NEAR-wallet feature.

### Architecture decisions

- **Keyless operation.** The API works unauthenticated at the cost of a
  platform surcharge priced into quotes (documented as 20 bps; measured at
  10 bps on 2026-07-15, injected as an `appFees` entry paying the
  protocol's collection account on every keyless quote). A partner API key
  would remove the surcharge and improve fee share terms, but ties the
  project to an identified Gibraltar-law business relationship (Defuse Labs
  Ltd), creates a revocable dependency, and an API key cannot be kept secret
  inside an open-source client with no server component. Keyless first;
  revisit with volume data. Switching later is additive.
- **Application fee: 20 bps configured, netting 10 bps (0.1%).** The
  quote request's `appFees: [{recipient, fee}]` parameter is honored on
  unauthenticated quotes. Measured mechanics (paired live dry quotes,
  2026-07-15): the service rewrites the fee array — a configured fee of N
  bps becomes N/2 to the configured recipient plus N/2 on top of the
  surcharge to the protocol account, so the user's total app-fee cost is N
  plus the surcharge, and the marginal cost of the application fee to the
  user is exactly N. The revenue target is 10 bps net, hence
  `DefaultAppFeeBps = 20` (user marginal cost 20 bps; ~30 bps all-in with
  the current surcharge). The rewritten array is echoed on every quote,
  giving the UI an exact, displayable fee breakdown. Fees accrue in the
  traded token as NEP-245 balances credited to the recipient account
  inside `intents.near` (denomination to confirm in Phase 5, along with
  the 10 bps net-receipt assumption).
- **Treasury: 0x-style secp256k1 account.** The verifier derives account
  identities from public keys with no registration; an ECDSA key yields an
  0x-style identity whose withdrawals are signed with ERC-191
  (`personal_sign`). The key lives entirely outside the repo and build
  system; only the derived identity string appears in configuration.
  Accrued fees are swept periodically (converted and withdrawn in bulk,
  e.g. to USDC on Polygon), paying bridge costs once per sweep rather than
  per trade. Sweep cadence bounds the treasury's exposure to verifier and
  bridge custody risk. Any funded NEAR account can submit the sweep's
  `execute_intents` transaction; authorization comes from the treasury
  key's signature, not the submitter.
- **Chain set: runtime intersection.** Eligible pairs are computed from the
  user's enabled wallets intersected with the live `GET /v0/tokens`
  catalog — no hardcoded chain list. As of 2026-07 the intersection covers
  BTC, LTC, DOGE, ZEC (transparent), BCH, DASH, ETH + ERC-20 stablecoins,
  Polygon, BNB + BEP-20, SOL + SPL, XRP, NEAR + NEP-141, and Hyperliquid.
  Not supported by Intents: DCR, XMR, FIRO, DGB.
- **Code isolation.** All Intents functionality lands in dedicated files
  and packages: a new `wallet/intents` package for the client and engine,
  new `Core` methods in `wallet/core/near-intents.go`, appserver handlers
  in a dedicated file. Existing files are touched only for registration and
  routing.
- **Custody honesty in UX.** 1Click is custodial-in-transit: the deposit
  address is service-controlled, and bridge legs are custodied by either a
  validator federation (legacy PoA bridge: LTC, DOGE, XRP, ZEC, BCH, Tron,
  Cardano, Sui, …) or MPC networks (Omni: BTC, ETH, SOL, BNB, Base,
  Arbitrum; HOT: Polygon, TON, Stellar, …). The UI states this plainly —
  operator identity, custody window (typically minutes), per-chain bridge
  trust — and never describes the feature as trustless. Per-chain trust
  labels are data-driven because the PoA→Omni migration is ongoing. All
  refund and recipient addresses are seed-derived, so funds at rest are
  never in third-party custody; the disclosure copy leans on that
  guarantee.

### Verified protocol behavior (2026-07-14/15)

- `appFees` is honored without authentication and rewritten by the service
  (split + surcharge injection; see above). It is excluded from the quote
  signature's signed payload.
- The `/tokens` catalog carries no min/max bounds; limits surface at quote
  time (`minAmountIn`/`minAmountOut`), and quotes disclose `refundFee`
  (origin asset) and `withdrawFee` (destination asset) upfront.
- The service normalizes asset IDs across its schemes (`nep141:` ↔
  `1cs_v1:`), so echoed asset IDs cannot be compared byte-wise against the
  request; the nep141 form is preferred for requests.
- Quote signatures verify only against the original `/quote` response
  bytes. The `/status` endpoint re-serializes the stored request (asset
  IDs come back un-normalized), so status-embedded quote responses do NOT
  re-verify. Quotes are verified once at quote time and persisted.
- Deposit addresses are unique per quote: two identical non-dry quote
  requests returned distinct BTC addresses. The deposit address is the
  swap's identity and primary key. Memo-mode chains use a shared address
  plus memo instead.
- The service may override the requested `deadline` (a request for +1 day
  came back +4 days). Exact request-vs-imposed semantics need pinning down
  during engine work; per-chain deadline defaults must cover origin-chain
  confirmation times.
- Refunds are automatic, service-side, to the `refundTo` in the quote.
  There is no client-initiated refund except BTC's permissionless
  timelocked path through `btc-connector.bridge.near` (2-day timelock when
  a refund address is set).
- There is no testnet deployment of the protocol. Live validation happens
  on mainnet with small amounts.

## Phase 1 — NEAR wallet hardening (complete)

Commit `ed64f6632`.

- [x] Ranked multi-provider RPC failover (FastNear, Lava, dRPC, near.org
  last; all live-verified). Transport/HTTP failures cool the endpoint for
  a minute; method-not-found fails over; other JSON-RPC errors are
  authoritative. `rpcprovider` setting accepts a comma-separated list.
- [x] `send_tx` with `wait_until: EXECUTED_OPTIMISTIC` replaces
  `broadcast_tx_async`; on-chain execution failures surface as `Send`
  errors, wait-timeouts store a pending transaction for the poller.
- [x] Access-key nonce watermark plus one retry on `InvalidNonce` via the
  optimistic view.
- [x] Dynamic fees: `gas_price` RPC (cached 60 s) × ~450 Ggas transfer
  cost; `asset.TxFeeEstimator` implemented; recorded fees use actual
  tokens burnt.
- [x] `asset.Withdrawer`: fee subtracted from value, amount capped to
  retain the account's storage stake plus a fee margin.
- [x] NEP-413 off-chain message signing (`SignMessageNEP413`), groundwork
  for later verifier-native flows.

Deferred until PR #113 (NEP-141 token layer) merges:

- [ ] `token.go` unknown-token panic → error return.
- [ ] Token-send fee estimation (storage-deposit action doubles gas).
- [ ] Native USDT (`usdt.tether-token.near`).

## Phase 2 — `wallet/intents` package: 1Click client (complete)

- [x] REST client for the v0 endpoints used by the wallet: `/tokens`,
  `/quote` (dry and committed), `/status` (404 → `ErrSwapNotFound`, for
  restore-time probing), `/deposit/submit`. Hand-written; the official Go
  SDK is stale codegen pinned to an old spec.
- [x] Token catalog: fetch and index Intents assets; per-chain native
  resolution (`nep141:` preferred over `1cs_v1:` duplicates, `wrap.near`
  special-cased for NEAR) and case-insensitive contract lookup for tokens;
  chain-slug → BIP-44 ID mapping for implemented chains. Unknown catalog
  entries are retained but unmapped, never fatal.
- [x] Quote signature verification, ported from the reference SDK's
  canonicalization (whitelisted flat payload with JavaScript-falsy
  omission, stable stringify, SHA-256, base58 digest string as the Ed25519
  message). Validated against captured production-signed responses in
  `wallet/intents/testdata/`. Key pinned with configuration override.
- [x] Application fee injection via client configuration, plus an echo
  cross-check of recipient/refundTo/amount/swapType after signature
  verification (the signature proves what the service signed, not what
  the client sent).
- [x] Dry quotes for price preview (dry quotes skip the service's
  compliance screening; committed quotes run it).

## Phase 3 — swap engine in `wallet/core/near-intents.go` (complete)

- [x] Persistent swap records keyed by deposit address, in a table of the
  core database indexed by creation time (newest-first listing) and by
  activity (the poller reads only swaps that still need polling): quote
  terms, the signed quote envelope (verified at quote time; the status
  endpoint's copy is not re-verifiable), state, assets, and transaction
  references. States: `QUOTED → DEPOSIT_SENT →` service statuses `→
  SUCCESS | REFUNDED | FAILED`, plus local `EXPIRED` for quotes never
  funded.
- [x] Execution (`IntentsExecute`): committed quote → persist → origin
  wallet `Send()` to the deposit address; `refundTo`/`recipient` are
  always the wallets' own seed-derived addresses; `/deposit/submit` hint
  with the txid; memo-mode deposits refused. A failed send leaves the
  record `QUOTED` to expire unfunded. The caller echoes the displayed
  preview's minimum output (`minAtomsOut`, required); a committed quote
  paying less — the rate moved past the slippage tolerance since the
  preview — is rejected before anything is persisted or sent.
- [x] Status poller with restart resumption (persisted records are polled
  from engine start); service-reported origin/destination/NEAR
  transaction hashes attached, including refund transactions on
  `REFUNDED`; balance refresh on terminal states; notifications on every
  state change.
- [x] Per-chain deadline defaults (2 h BTC, 1 h other UTXO chains, 45 min
  account chains); slippage parameter with a 1% default; quote-time
  min-amount surfacing in previews (`IntentsPreview`, dry quotes).
- [x] Swap-state reconstruction (`IntentsReconstruct`): probes `/status`
  with recipients of recent outgoing transactions across wallets,
  rate-limited, rebuilding records flagged `Reconstructed`. (No key-level
  recovery is possible or needed: deposit addresses are
  service-controlled, and all value paths terminate at seed-derived
  addresses.)
- [x] Capability gating (`IntentsAssets`): catalog ∩ chain mapping,
  flagged by wallet availability. Native assets only until the token
  layer (PR #113) lands.

## Phase 4 — API and UI (complete)

- [x] Appserver endpoints in a dedicated handler file
  (`wallet/appserver/near-intents.go`): assets, preview, execute, swaps,
  refresh, reconstruct. Execute mirrors the send handler's password flow.
- [x] Filled the `Swap.tsx` skeleton: pair selection from the eligible set
  (origin restricted to available wallets), live debounced dry-quote
  preview with the full itemized fee breakdown, password-gated confirmation,
  and a swaps activity list live-updated via the `intentsswap`
  notification. `IntentsAsset.atomDecimals` (the wallet atomic-unit
  exponent) carries the scale the UI must use to convert the API's atoms
  fields — distinct from the Intents catalog decimals (e.g. NEAR 8 vs 24).
- [x] Custody-honesty framing: one-time first-use disclosure modal;
  persistent third-party-custody notice on the swap form; per-pair
  bridge-trust popover driven by a data map (PoA federation vs Omni/HOT
  MPC); plain-language status labels.
- [x] Failure/attention states surface the deposit address, transaction
  hashes, the NEAR Intents explorer, and the service status page.
- [ ] README/site principles carve-out describing third-party in-transit
  services (still open).

## Review and remediation (2026-07-15) — complete

An 8-angle high-effort code review of the branch produced 17 confirmed
findings. All are fixed and committed (`34d811b68` near, `9f0a1cf2a`
intents, `cad3f2875` core; the engine shutdown-ordering fix landed with
Phase 4). This section is a record of that closed work, not outstanding
items. Full build, race, lint, and tests are green.

- Fund-safety (NEAR send path): a broadcast is no longer failed over or
  retried on an ambiguous outcome (transport failure or the server's
  `TIMEOUT_ERROR`) — it is recorded pending, eliminating the double-send
  and the "no funds moved" misreport. The `InvalidNonce` retry runs only
  for an authoritative rejection.
- RPC robustness: failover try-order fixed so a recovered cooling endpoint
  is retried; the client is read atomically through a guard that closes a
  nil-client panic (via `/txfee` on a never-connected wallet) and a data
  race; the gas-price cache no longer holds its mutex across the RPC and
  backs off on failure.
- `EstimateSendTxFee`'s fee-inclusive branch caps like `Withdraw`, so the
  UI's Max flow is no longer rejected for NEAR.
- Privacy: both the NEAR RPC client and the 1Click client now honor the
  configured Tor proxy.
- Quote integrity: a core-side check rejects a quote whose echoed assets do
  not resolve (through the catalog) to the requested chains, closing a
  same-chain token-substitution vector the signature alone does not cover.
- Engine concurrency: swap-record updates are serialized and reload under
  the lock, so a stale poll cannot regress state or drop the deposit txid.
- Reconstruction resolves the destination chain through the catalog with an
  explicit unknown sentinel, fixing a `DestAssetID = 0` (BTC) misread.
- A `DEPOSIT_SENT` swap the service never observes is timed out past its
  deadline instead of polled forever; the poller parks when idle; the
  catalog is served stale-while-revalidate.

One finding was refuted: the max-withdraw fee margin is adequate (implicit
account creation costs ~1.9× the fee estimate, not the ~9× claimed), though
headroom is only ~6% and worth watching under gas-price spikes.

## Phase 5 — live mainnet validation

No testnet exists; validate with small amounts on mainnet.

- [ ] End-to-end swaps across representative pairs (UTXO→account,
  account→UTXO, token→native, NEAR-origin).
- [ ] `appFees` net receipt: confirm the 50% split, the fee's token
  denomination, and its NEP-245 form at the treasury identity.
- [ ] Refund path: force an under-deposit and a missed deadline; measure
  the refund haircut (ToS permits fees on failed transactions).
- [ ] `FAILED` terminal status semantics (undocumented) and whether funds
  are recoverable from it.
- [ ] Deadline override behavior per chain.
- [ ] Treasury sweep rehearsal: ERC-191-signed withdrawal from the
  verifier balance to the target chain, using `wallet/cmd/treasurysweep`
  (balances → build → sign → simulate → submit). The signing path is
  already validated against the deployed verifier: a simulated
  throwaway-identity withdrawal fails with "insufficient balance", i.e.
  past signature recovery, nonce-salt, and deadline checks. The rehearsal
  still must confirm the external-chain (`WITHDRAW_TO`) conventions per
  bridge and an actual funded withdrawal end to end.

## Phase 6 — later work

- [ ] Verifier-native flows: deposits into `intents.near`, NEP-413-signed
  intent batches (`INTENTS` deposit/recipient types), enabling swaps from
  verifier balances without an origin-chain transaction. Includes probing
  whether `/submit-intent` accepts batches extended with a fee-transfer
  leg.
- [ ] `FLEX_INPUT` / `ANY_INPUT` swap types where they fit the UX.
- [ ] In-wallet automation of the BTC permissionless refund claim.
- [ ] Treasury sweep tooling/runbook (scheduled conversion and withdrawal,
  e.g. to USDC on Polygon).
- [ ] Revisit partner key with volume data (fee-free quotes + revenue
  share vs. identity and revocation risk).

## Risks and contingencies

- **Keyless `appFees` is undocumented behavior.** It works today but the
  docs frame it as a partner feature, and the keyless surcharge has already
  been repriced once (10→20 bps). Contingency: origin-side fee outputs (an
  extra output on UTXO deposit transactions, a second transfer on account
  chains) behind the fee-mechanism seam — nets 100% of the fee with no
  protocol dependency, at the cost of per-chain plumbing.
- **In-band compliance screening.** Committed quotes are screened (TRM et
  al.); behavior for funds already at a deposit address when an address is
  flagged is undocumented. Probed during Phase 5; failure UX must handle
  it.
- **Bridge migration churn.** The PoA→Omni migration will keep moving
  assets between trust models; per-chain trust labels and any
  bridge-dependent behavior stay data-driven.
- **Provider drift.** RPC providers and the 1Click deployment both change;
  the failover ranking and the pinned quote-signature key are configuration,
  not constants baked across the codebase.
