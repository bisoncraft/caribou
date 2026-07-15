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
- **Application fee: 10 bps (0.1%) via `appFees`.** The quote request's
  `appFees: [{recipient, fee}]` parameter is honored on unauthenticated
  quotes. Measured mechanics (paired live dry quotes, 2026-07-15): the
  service rewrites the fee array — a configured fee of N bps becomes N/2 to
  the configured recipient plus N/2 on top of the surcharge to the protocol
  account, so the user's total app-fee cost is N plus the surcharge, and
  the marginal cost of the application fee to the user is exactly N. The
  rewritten array is echoed on every quote, giving the UI an exact,
  displayable fee breakdown. Fees accrue in the traded token as NEP-245
  balances credited to the recipient account inside `intents.near`
  (denomination to confirm in Phase 5).
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

## Phase 3 — swap engine in `wallet/core/near-intents.go`

- [ ] Persistent swap records keyed by deposit address (lexi table):
  quote terms, signature, state, origin/destination assets, txids.
  States: `QUOTED → DEPOSIT_SENT → PROCESSING → SUCCESS | REFUNDED |
  FAILED`, plus `INCOMPLETE_DEPOSIT` handling with its deadline countdown.
- [ ] Execution: origin wallet `Send()` (or `Withdraw` for max) to the
  deposit address; `refundTo` = origin wallet's deposit address;
  `recipient` = destination wallet's deposit address (both always
  seed-derived); `/deposit/submit` hint with the txid.
- [ ] Status polling with restart resumption; refund transactions detected
  on the origin wallet and attached to the swap record.
- [ ] Per-chain deadline defaults covering origin confirmation times;
  min/max clamps from the catalog; slippage configuration.
- [ ] Swap-state reconstruction after seed restore: probe `GET /status`
  with the counterparty addresses of otherwise-unexplained outgoing
  transactions and rebuild records for matches. (No key-level recovery is
  possible or needed: deposit addresses are service-controlled, and all
  value paths terminate at seed-derived addresses.)
- [ ] Capability gating at core level: a pair is offered when both wallets
  exist and the catalog lists both assets.

## Phase 4 — API and UI

- [ ] Appserver endpoints (dedicated handler file): quote preview,
  execute, status/history.
- [ ] Fill the `Swap.tsx` skeleton: pair selection from the eligible set,
  dry-quote preview with full fee breakdown (application fee, platform
  surcharge, solver spread in the rate, network costs), commit flow,
  status timeline mirroring the real state machine.
- [ ] Custody-honesty framing: one-time first-use disclosure; persistent
  one-line notice on the swap form; per-pair details popover with operator
  identity, custody window, per-leg bridge trust (data-driven), refund
  behavior; plain-language status copy ("Swapping — funds with the
  service", "Refunded — funds returned to your wallet").
- [ ] Failure states surface the swap explorer link, the service status
  page, and the identifiers needed for a support escalation.
- [ ] README/site principles carve-out describing third-party in-transit
  services.

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
  verifier balance to the target chain.

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
