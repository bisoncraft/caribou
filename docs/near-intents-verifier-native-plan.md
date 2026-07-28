# NEAR Intents verifier-native swaps

Successor plan to the 1Click integration ([near-intents-plan.md](near-intents-plan.md)).
The 1Click service is a distribution channel over the NEAR Intents
verifier contract (`intents.near`): it takes custody of the user's funds
at a service-controlled deposit address, moves them into the verifier,
runs the market-maker auction, and withdraws to the recipient — a trusted
agent in the middle of every swap. Verifier-native swaps remove that
agent: the wallet deposits directly into the verifier under a
seed-derived identity, requests quotes from market makers itself, signs
`token_diff` intents that commit exact amounts, and withdraws directly.

## What it buys, honestly

- **One fewer custodian.** The trust set shrinks from {origin bridge,
  destination bridge, verifier contract, 1Click agent} to {origin bridge,
  destination bridge, verifier contract}. Swap execution inside the
  verifier is an atomic netting of signed intents — a counterparty can
  take no more than the signed diff, and the worst outcome is no fill.
  Bridge custody is unchanged and remains the dominant trust for every
  non-NEAR asset: a bridged token inside the verifier is an IOU on its
  bridge's federation or MPC committee, at rest, for as long as it exists
  there. Custody-disclosure copy must describe bridge trust identically
  in both modes. The only fully bridge-free case is NEAR-native ↔
  NEAR-native.
- **Full fee capture.** The application fee becomes a transfer intent to
  the treasury inside the same signed batch, executed atomically with the
  swap: 100% of the fee, versus the 50% protocol split under `appFees`,
  and no 10 bps keyless surcharge priced into the user's quote. The
  surcharge and the split are both 1Click-layer constructs; the verifier
  contract's own protocol fee (`fee()`, applied to `token_diff`) measures
  1 pip = 0.01 bp on mainnet as of 2026-07 — economically nil, though it
  is a governance-settable knob and worth re-checking periodically. The
  treasury accrues the same NEP-245 balances in the same account, so the
  existing sweep tooling (`wallet/cmd/treasurysweep`) applies unchanged.
- **Direct quotes.** RFQ against the solver bus rather than 1Click's
  re-priced aggregation. Whether direct pricing is better at retail size
  is an empirical question (see risks).
- **No deposit deadline machinery.** 1Click quotes commit before the
  deposit exists, so its design revolves around deposit windows,
  deadlines, refund paths, and screening of in-flight funds. A
  verifier-native swap quotes against funds already credited in the
  verifier: the RFQ→sign→execute loop runs in seconds against a resting
  balance, and a failed or expired quote costs nothing — the balance just
  sits. Expiry, under-deposit, and refund states largely disappear from
  the swap itself and move to the (separately tracked) deposit leg.
- **On-chain recoverability.** The user's verifier identity is
  seed-derived, and verifier balances are ordinary on-chain state
  (`mt_batch_balance_of`), so seed restore recovers in-verifier value by
  direct query — strictly better than the 1Click status-probing
  reconstruction.

The trade: deposit and swap become distinct steps. A one-shot BTC→SOL
user waits for the bridge deposit either way; total time is comparable,
but the wallet must present a two-stage flow (or hide it behind
sequenced automation). For repeat swappers and stable-pair traders,
funds can rest in the verifier and swaps become near-instant.

## Identity and signing

- The user's verifier identity is the seed-derived NEAR key: the
  implicit account (hex-encoded ed25519 public key), signing intents
  under NEP-413 (already implemented in `wallet/asset/near`, currently
  unused) or raw ed25519. Implicit accounts need no registration with
  the verifier. This preserves the invariant that every value-bearing
  endpoint is recoverable from the seed.
- The treasury keeps its 0x-style secp256k1 identity and ERC-191
  signing; the fee leg is a user-signed transfer *to* the treasury, so
  no treasury key is involved in swaps.
- Wire formats (DefusePayload, MultiPayload, salted nonces, intent
  serialization) are already implemented and mainnet-validated in
  `wallet/cmd/treasurysweep`. That code moves to a shared package —
  `wallet/intents/verifier` — consumed by both the sweep tool and the
  swap engine, gaining NEP-413 alongside ERC-191.

## Architecture

New code isolates in `wallet/intents/verifier` (intent construction,
signing, bus client, verifier views) and extends the existing engine in
`wallet/core/near-intents.go`; per-chain deposit/withdraw plumbing lands
behind small interfaces so bridge conventions stay data, not code paths.

1. **Verifier client** (`wallet/intents/verifier`): payload building and
   signing (NEP-413 + ERC-191), salted-nonce management,
   `simulate_intents` preflight, balance and event views, deposit-address
   resolution per bridge.
2. **Solver bus client**: subscribe/RFQ against the solver relay, quote
   collection with expiry handling (quote hashes live on the order of
   two minutes), quote-to-`token_diff` assembly, submission path.
3. **Engine extension**: a second swap pipeline alongside 1Click —
   states roughly DEPOSIT_PENDING → AVAILABLE → QUOTED → EXECUTED →
   WITHDRAWING → SETTLED, persisted in the existing `intentsSwaps` table
   (the state-machine knowledge already lives in `wallet/db`); the
   deposit leg tracked independently of the swap leg.
4. **Routing policy**: per-pair choice between verifier-native and
   1Click, driven by which legs have native deposit/withdraw support and
   by net-of-fees quote comparison. 1Click remains as fallback and as
   coverage for chains whose bridge plumbing is not yet built.

## Phases

### Phase A — protocol probes (no user funds at risk)

- [ ] Solver-bus access and protocol: connect, RFQ for representative
  pairs, measure quote latency, expiry, and competitiveness vs. paired
  1Click dry quotes at retail sizes.
- [ ] Submission path: whether the relay submits signed batches
  (relayer-key model), or the wallet self-submits `execute_intents` —
  and if self-submitting, the gas story for users with no NEAR balance
  (the NEAR wallet exists in Caribou, but a BTC→SOL user cannot be
  required to hold gas; a sponsored-submission fallback needs sizing).
- [ ] Batch shape: a user-signed batch of [`token_diff`, fee `transfer`]
  plus the solver's signed diff, simulated and executed on mainnet with
  dust amounts; confirms atomicity and full fee capture.
- [ ] NEAR-native round trip: `ft_transfer_call` deposit into the
  verifier, swap, `ft_withdraw` back, all with dust.
- [ ] Screening behavior: what compliance filtering exists on the bus
  and verifier for direct participants, and what happens to a flagged
  identity's resting balance.
- [ ] Bridge deposit-address derivation and crediting semantics for the
  top chains (Omni: BTC, SOL, ETH; PoA: LTC, DOGE, XRP), plus
  per-bridge withdrawal conventions (shared research item with the
  treasury-sweep rehearsal).

### Phase B — shared verifier package

- [ ] Extract treasurysweep's wire-format code into
  `wallet/intents/verifier`; add NEP-413 signing; port the sweep tool
  onto it.
- [ ] Bus client with quote lifecycle management.
- [ ] Golden-vector and simulation-backed tests, as with the sweep tool.

### Phase C — NEAR-native swaps end to end

- [ ] Deposits and withdrawals for NEAR and on-NEAR NEP-141s (no
  bridges); engine states; auto-withdraw on settle.
- [ ] Fee transfer leg live, treasury accrual verified via sweep
  tooling.
- [ ] UI behind a development flag; swap review screen shows the exact
  signed amounts (the `token_diff` is the price commitment — nothing is
  displayed that is not signed).

### Phase D — bridged assets

- [ ] Per-chain deposit flows in priority order (BTC, SOL, ETH first;
  then PoA chains), each behind the deposit interface with data-driven
  trust labels.
- [ ] Withdrawal conventions per bridge, validated by simulation before
  funded tests.
- [ ] Deposit-leg tracking: bridge finality windows, crediting
  detection, stuck-deposit surfacing.

### Phase E — routing and launch

- [ ] Router: verifier-native when both legs supported and the direct
  quote nets better; 1Click otherwise; per-pair override for
  diagnostics.
- [ ] Custody-disclosure copy updated to the reduced (but honestly
  described) trust set; README carve-out revised.
- [ ] Fee-capture comparison measured live (net bps native vs. 1Click).

### Phase F — later

- [ ] Opt-in resting verifier balances with an explicit balance surface
  in the UI (until then, auto-withdraw everything on settle).
- [ ] Multi-hop and FLEX_INPUT-style quoting where the bus supports it.
- [ ] Retire 1Click per pair as native coverage and pricing prove out.

## Risks and open questions

- **Bus access and pricing.** The solver bus is built for market makers;
  taker-side access, rate limits, and whether direct RFQ at retail size
  prices better or worse than 1Click's aggregation are all unmeasured.
  If direct pricing is worse by more than the recovered fee spread
  (~20 bps), the economic case collapses to custody-only.
- **Gas sponsorship.** If self-submission is required, sponsored
  submission is a small standing cost and a liveness dependency on
  Caribou infrastructure; if the relay submits, the dependency moves to
  the relay's liveness. Neither party can steal — a stalled submission
  strands a signed intent until its deadline lapses, funds resting in
  the verifier throughout.
- **Two-step UX.** Deposit-then-swap must not read as "my coins
  disappeared into a balance I didn't ask for." Default automation:
  deposit → swap → withdraw as one sequenced flow with the intermediate
  state visible but not requiring action.
- **Quote expiry vs. signing UX.** Roughly two-minute quote validity is
  ample for programmatic signing but rules out long user deliberation at
  the confirm screen; the flow must re-quote transparently, with the
  signed amounts always matching the display.
- **Contract upgrade authority.** The verifier is upgradeable; its
  keyholders can in principle change execution semantics. This applies
  equally to the 1Click path and belongs in the trust documentation
  either way.
- **Bridge heterogeneity.** Deposit and withdrawal conventions differ
  per bridge and migrate over time (PoA → Omni). All per-bridge behavior
  stays data-driven, mirroring the existing trust-label machinery.
