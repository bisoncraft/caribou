# NEAR Intents Swap Integration

Cross-chain swaps through [NEAR Intents](https://docs.near-intents.org/)
via the 1Click API, plus the NEAR wallet hardening that supports it. The
build-out (wallet hardening, API client, swap engine, UI, fee-account
tooling)
is complete; live mainnet validation remains. A follow-on design for
verifier-native swaps is planned separately in
[near-intents-verifier-native-plan.md](near-intents-verifier-native-plan.md).

## How 1Click works

NEAR Intents settles swaps atomically inside the `intents.near` verifier
contract on NEAR mainnet. Assets from other chains enter and exit through
bridges, and the 1Click REST service (`https://1click.chaindefuser.com/v0`)
wraps the pipeline behind a deposit-address flow: `POST /v0/quote` returns
a quoted rate and a single-use deposit address on the origin chain; the
user sends an ordinary transaction to it; the service bridges, settles
against a solver, and delivers funds to the recipient on the destination
chain, with `GET /v0/status` tracking `PENDING_DEPOSIT →
KNOWN_DEPOSIT_TX → PROCESSING → SUCCESS | REFUNDED | FAILED |
INCOMPLETE_DEPOSIT`.

No NEAR account, gas, or signature is required from the user for any
origin/destination pair, which makes the integration primarily a
core-level feature rather than a NEAR-wallet feature.

## Architecture decisions

- **Keyless operation.** The API works unauthenticated at the cost of a
  platform surcharge priced into quotes (documented as 20 bps; measured at
  10 bps in 2026-07, injected as an `appFees` entry paying the protocol's
  collection account). A partner API key would remove the surcharge and
  improve fee terms, but ties the project to an identified business
  relationship, creates a revocable dependency, and cannot be kept secret
  inside an open-source client with no server component. Keyless first;
  revisit with volume data. Switching later is additive.
- **Application fee: 20 bps configured, netting 10 bps.** The quote
  request's `appFees` parameter is honored on unauthenticated quotes.
  Measured mechanics: the service rewrites a configured fee of N bps into
  N/2 to the configured recipient plus N/2 (on top of the surcharge) to
  the protocol account, so the user's marginal cost of the application fee
  is exactly N. The rewritten array is echoed on every quote, giving the
  UI an exact, displayable fee breakdown. Fees accrue in the traded token
  as NEP-245 balances credited to the recipient identity inside
  `intents.near`.
- **Fee account: 0x-style secp256k1 account.** Bisoncraft's
  fee-collection account inside the verifier, called the treasury in the
  tool names. The verifier derives account
  identities from public keys with no registration; an ECDSA key yields an
  0x-style identity whose withdrawals are signed with ERC-191. The key
  lives entirely outside the repo; only the derived identity string
  appears in configuration (`intentsFeeRecipient`, empty until release).
  Accrued fees are swept periodically in bulk, paying bridge costs once
  per sweep; sweep cadence bounds the accrued fees' exposure to verifier and
  bridge custody risk. Any funded NEAR account can submit a sweep —
  authorization comes from the fee-account key's signature, not the
  submitter.
- **Chain set: runtime intersection.** Eligible pairs are computed from
  the user's enabled wallets intersected with the live `GET /v0/tokens`
  catalog — no hardcoded chain list. As of 2026-07 the intersection covers
  BTC, LTC, DOGE, ZEC (transparent), BCH, DASH, ETH + ERC-20 stablecoins,
  Polygon, BNB + BEP-20, SOL + SPL, XRP, NEAR + NEP-141, and Hyperliquid.
  Not supported by Intents: DCR, XMR, FIRO, DGB.
- **Code isolation.** All Intents functionality lives in dedicated files
  and packages: the `wallet/intents` client package, `Core` methods in
  `wallet/core/near-intents.go`, swap-record storage in
  `wallet/db/near-intents.go`, appserver handlers in
  `wallet/appserver/near-intents.go`. Existing files are touched only for
  registration and routing.
- **Custody honesty in UX.** 1Click is custodial-in-transit: the deposit
  address is service-controlled, and bridge legs are custodied by either a
  validator federation (legacy PoA bridge: LTC, DOGE, XRP, ZEC, BCH, …)
  or MPC networks (Omni: BTC, ETH, SOL, BNB, …; HOT: Polygon, TON, …).
  The UI states this plainly — operator identity, custody window
  (typically minutes), per-chain bridge trust — and never describes the
  feature as trustless. Per-chain trust labels are data-driven because the
  PoA→Omni migration is ongoing. All refund and recipient addresses are
  seed-derived, so funds at rest are never in third-party custody.

## Verified protocol behavior (measured 2026-07)

- `appFees` is honored without authentication and rewritten by the service
  (split + surcharge injection). It is excluded from the quote signature's
  signed payload.
- The `/tokens` catalog carries no min/max bounds; limits surface at quote
  time (`minAmountIn`/`minAmountOut`), and quotes disclose `refundFee`
  (origin asset) and `withdrawFee` (destination asset) upfront.
- The service normalizes asset IDs across its schemes (`nep141:` ↔
  `1cs_v1:`), so echoed asset IDs cannot be compared byte-wise against the
  request; the `nep141:` form is preferred for requests.
- Quote signatures verify only against the original `/quote` response
  bytes; the `/status` endpoint re-serializes stored quotes in a form that
  does not re-verify. Quotes are verified once at quote time and
  persisted.
- Deposit addresses are unique per quote and serve as the swap's identity
  and primary key. Memo-mode chains use a shared address plus memo
  instead; memo-mode deposits are refused.
- The service may override the requested `deadline` (a request for +1 day
  came back +4 days). Exact semantics are pinned down during live
  validation; per-chain deadline defaults must cover origin-chain
  confirmation times.
- A dry quote binds nothing on either side: it creates no server-side
  state, and execution always prices a fresh committed quote. The client
  enforces the previewed terms itself (see `minAtomsOut` below).
- Refunds are automatic, service-side, to the quote's `refundTo`. The only
  client-initiated path is BTC's permissionless timelocked refund through
  `btc-connector.bridge.near` (2-day timelock when a refund address is
  set).
- There is no testnet deployment. Live validation happens on mainnet with
  small amounts.

## Implementation map

- **NEAR wallet** (`wallet/asset/near`): ranked multi-provider RPC
  failover; execution-checked `send_tx` broadcasts that record ambiguous
  outcomes as pending rather than retrying (no double-send); access-key
  nonce watermark; dynamic fees from the `gas_price` RPC;
  `asset.Withdrawer`; NEP-413 message signing; Tor honored.
- **1Click client** (`wallet/intents`): `/tokens`, `/quote`, `/status`,
  `/deposit/submit`; catalog indexing with chain-slug → BIP-44 mapping;
  quote-signature verification (ported canonicalization, validated against
  captured production responses, pinned key with config override); echo
  cross-checks of the unsigned request fields; application-fee injection;
  bounded response reads; Tor honored.
- **Swap engine** (`wallet/core/near-intents.go`, storage in
  `wallet/db/near-intents.go`): records keyed by deposit address in a
  core-database table indexed by creation time (newest-first listing) and
  activity (the poller reads only swaps still needing polling); states
  `QUOTED → DEPOSIT_SENT →` service statuses, plus local `EXPIRED`;
  serialized record updates that never regress a terminal state; a parked
  poller that resumes persisted records across restarts; per-chain
  deadline defaults with timeout of swaps the service never observes;
  restore-time reconstruction by probing `/status` with recipients of
  recent outgoing transactions. `IntentsExecute` requires the displayed
  preview's minimum output (`minAtomsOut`) and rejects a committed quote
  paying less before anything is persisted or sent.
- **API and UI** (`wallet/appserver/near-intents.go`, `Swap.tsx`):
  assets/preview/execute/swaps/refresh/reconstruct endpoints; debounced
  dry-quote preview with itemized fees; password-gated confirmation that
  echoes the previewed minimum and refreshes the quote on failure;
  live-updating swap list; custody-honesty disclosure (first-use modal,
  persistent notice, data-driven per-chain bridge-trust popover).
  `IntentsAsset.atomDecimals` carries the wallet atomic-unit exponent,
  distinct from the Intents catalog decimals (e.g. NEAR 8 vs 24).
- **Fee-account tooling** (`wallet/cmd/treasurykey`,
  `wallet/cmd/treasurysweep`): key generation/validation, and verifier
  withdrawals (balances → build → sign → simulate) with ERC-191 signing
  validated against the deployed contract. Keys are read only from stdin;
  signing is offline-capable; submission is any funded NEAR account's
  `execute_intents` call.

## Live mainnet validation (Phase 5)

- [ ] End-to-end swaps across representative pairs (UTXO→account,
  account→UTXO, token→native, NEAR-origin).
- [ ] `appFees` net receipt: confirm the 50% split, the fee's token
  denomination, and its NEP-245 form at the fee-account identity.
- [ ] Realized fill vs. quoted `amountOut`: if fills cluster at the
  `minAmountOut` floor rather than the quote, slippage tolerance is a
  per-swap price concession and the default (100 bps) should tighten.
- [ ] Whether quoted spreads widen defensively during volatility.
- [ ] Refund path: force an under-deposit and a missed deadline; measure
  the refund haircut (ToS permits fees on failed transactions).
- [ ] `FAILED` terminal status semantics (undocumented) and whether funds
  are recoverable from it.
- [ ] Deadline override behavior per chain.
- [ ] Fee-sweep rehearsal with accrued fees: per-bridge
  external-withdrawal conventions (`WITHDRAW_TO` is the PoA-factory form;
  Omni may differ) and a funded withdrawal end to end.
- [ ] Compliance-screening failure modes: behavior for funds already at a
  deposit address when the address is flagged is undocumented; the failure
  UX must handle whatever surfaces.

## Later work (Phase 6)

- [ ] Verifier-native flows — planned in
  [near-intents-verifier-native-plan.md](near-intents-verifier-native-plan.md).
- [ ] NEAR token layer polish: unknown-token panic → error return,
  token-send fee estimation (the storage-deposit action doubles gas),
  native USDT (`usdt.tether-token.near`).
- [ ] Pair-aware slippage defaults (tighter for stable↔stable) once
  Phase 5 produces fill data; a user-facing slippage control with a sane
  input cap.
- [ ] `FLEX_INPUT` / `ANY_INPUT` swap types where they fit the UX.
- [ ] In-wallet automation of the BTC permissionless refund claim.
- [ ] Fee-sweep runbook (scheduled conversion and withdrawal, e.g. to
  USDC on Polygon).
- [ ] README/site principles carve-out describing third-party in-transit
  services.
- [ ] Revisit partner key with volume data (fee-free quotes + revenue
  share vs. identity and revocation risk).

## Risks and contingencies

- **Keyless `appFees` is undocumented behavior.** It works today but the
  docs frame it as a partner feature, and the keyless surcharge has
  already been repriced once. Contingency: origin-side fee outputs (an
  extra output on UTXO deposit transactions, a second transfer on account
  chains) behind the fee-mechanism seam — nets 100% of the fee with no
  protocol dependency, at the cost of per-chain plumbing.
- **Bridge migration churn.** The PoA→Omni migration will keep moving
  assets between trust models; per-chain trust labels and any
  bridge-dependent behavior stay data-driven.
- **Provider drift.** RPC providers and the 1Click deployment both change;
  the failover ranking and the pinned quote-signature key are
  configuration, not constants baked across the codebase.
