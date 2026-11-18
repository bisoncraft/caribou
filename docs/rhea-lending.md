# RHEA lending backend

Design of Caribou's backend for supplying to and withdrawing from RHEA
(Burrow) lending markets through RHEA's cross-chain multichain account
(MCA) route. Background and market research are in
`rhea-lending-research.md`; the funded release-gate procedure is in
[rhea-lending-release-test.md](rhea-lending-release-test.md).

Scope: supply, position display, withdrawal, and sweeps of an MCA's plain
token balance, from the `wallet/appserver` handlers down to a protocol
package. No UI, no borrowing, and supplied funds are never designated as
collateral. Mainnet, plus a simulated deployment for simnet (see Simnet).

## Deployment

| Role | Account | Notes |
| --- | --- | --- |
| Account manager (AM) | `multica.near` | v0.3.4; creates MCAs and holds the identity registry |
| MCA | `rheaNNNNNN.multica.near` | global contract code `FDX6nYTm…` (v0.1.6) on current accounts |
| Lending (Burrow) | `contract.main.burrow.near` | v0.16.2 |
| Relayer and fee collector | `mca-relayer.rhealab.near` | submits `exec`; API at `api.rhea.finance` |
| 1Click verifier | `intents.near` | delivers supplies and receives withdrawals |

Markets are an allowlist in `wallet/rhea/config.go` (USDC, USDT, wNEAR,
ZEC), each with the 1Click asset that means the token held on NEAR.

## Packages

| Package | Role |
| --- | --- |
| `wallet/rhea` | Protocol layer: identity, canonical business encoding, builders and validator, recipient messages, typed views at named blocks, receipt-tree decoding and extractors, relayer client |
| `wallet/asset/eth/evmkey` | EVM key derivation shared with the EVM wallets |
| `wallet/asset/near` | `ViewClient`: views at a block, final block, `EXPERIMENTAL_tx_status` with archival fallback, cross-endpoint corroboration |
| `wallet/intents` | 1Click client; `CustomRecipientMsg` and `NoAppFee` quote fields |
| `wallet/db` | `RheaOp` and `RheaAccount` records |
| `wallet/core` | Engine (`rhea.go`), supply (`rhea-supply.go`), withdrawal (`rhea-withdraw.go`), sweep (`rhea-sweep.go`), and the 1Click leg rules shared with swaps (`intents-leg.go`) |
| `wallet/appserver` | HTTP handlers (`rhea.go`) |

### HTTP API

All routes are under the authenticated API group.

| Route | Purpose |
| --- | --- |
| `GET /rhea/markets` | Market parameters and rates |
| `GET /rhea/account` | Discovery status, positions, plain balances, admission state; with no account, each market's creation fee |
| `GET /rhea/ops` | Every lending operation, newest first |
| `POST /rhea/supply/preview`, `/execute` | Supply from a wallet asset, creating the MCA when needed |
| `POST /rhea/withdraw/preview`, `/execute` | Withdraw supplied tokens to a wallet asset |
| `POST /rhea/sweep/preview`, `/execute` | Move an MCA plain balance into supply or to a wallet asset |

Execute requests echo the bounds of the accepted preview (minimum output,
maximum fee or creation fee) and core rechecks them after re-planning.
Every signature for the lending account requires the app password, taken
from the request or the cached session password.

## USD rates

USD values come from Caribou's ticker rates, the same map the wallet
exposes as its ticker rates. Each market names its ticker (`FiatTicker`):
`USDC`, `USDT`, `NEAR` for wNEAR, and `ZEC`, the same in every deployment.
A ticker's rate averages the fresh fiat rates of every registered asset in
its ticker group (USDC on Ethereum, Polygon, NEAR, and so on), and an
asset's rate is fresh while some source has updated it within the hour.
Rate sources fetch only registered assets, so a ticker is priced only
while some asset of its group is registered on the network. On simnet
`usdc.near` is not registered, and USDC is priced through the group's
other members. No ZEC asset is registered, so ZEC has no rate until one
is.

A supply requires a current rate for the market token. Without one,
preview and execute refuse with `no current USD rate for <symbol>;
supplying is unavailable until rates refresh`; execute re-plans, and so
re-reads the rate, before anything is persisted or sent. The rate values
the creation fee line and sets the creation minimum. `GET /rhea/markets`
and the positions in `GET /rhea/account` mark a market without a rate with
`supplyUnavailable` and that reason rather than hiding it, and its
creation fee line carries the same reason as `unavailable`, in place of an
amount. `supplyUnavailable` speaks only to supplies.

Withdrawals and sweeps need no rate. Their 2 USD relayer fee cap is priced
at the fee token's ticker rate when one is current (the market token for a
withdrawal, NEAR for a sweep), and otherwise at the token's price in the
1Click catalog (wNEAR for a sweep), provided the catalog is within its
15-minute freshness window and lists the token under its pinned asset ID.
With neither, the withdrawal or sweep is refused with an error naming
both. The preview reports which priced the cap as `feeCapSource`
(`fiatRate` or `catalog`), and a withdrawal's `feeUSD` uses the same
price. Execute re-plans and prices the cap again, possibly from the other
source; the fee must fit the cap at execute as well as the previewed fee.

Quote USD values (`amountInUSD`, `amountOutUSD`, and the operation's
`usdOut`) remain 1Click's own figures. Only new user-initiated operations
read a rate or a catalog price. Reconciliation of existing operations,
admission walks, and receipt recovery never do, so an operation already
signed or funded runs to its outcome while rates are missing.

## Identity and control

The control identity is the EVM wallet key at account 0'
(`m/44'/60'/0'/0/0` of the EVM asset seed), the address Caribou's EVM
wallets share. Derivation version 1 names this choice so a later scheme can
be discovered beside it. A business is signed with EIP-191 over its
canonical JSON (the SDK's key order, no HTML escaping); the business is
bound to the MCA only through its nonce, so the validator re-derives every
request from approved parameters before signing.

## Operations

### Supply

A 1Click swap whose payout is a market token delivered with a
`customRecipientMsg`: to the MCA itself (`BurrowSupply`) when the identity
has one, or to the AM (`BurrowRegisterAndSupply` with a creation
signature) when it has none. The AM's reliable-business templates are pinned byte for byte and
checked at the read block before every supply. Supplies carry Caribou's
application fee; withdrawals and sweeps do not.

The supply that creates the MCA also pays a one-time creation fee, taken
from the delivery. It is presented as its own line, never folded into
another amount (`RheaFeeLine`): the total in the market token, its USD
value, a one-time flag, and two components. `fixed` is the account
manager's `get_create_mca_fee`; `accountFunding` is the new MCA's initial
NEAR balance (`get_mca_init_balance`, also reported in NEAR) valued at the
token's `get_near_value`, rounded up. The supply preview carries the line
as `creationFee`, and execute is bound to its amount (`maxCreationFee`).
Before any amount is chosen, `GET /rhea/account` lists the line for every
market while discovery finds no account (`creationFees`), reports
`creationFeesUnavailable` when discovery cannot conclude, and omits both
once an account exists. Each listed line also carries `creationMinimum`
(amount and USD value), the creation minimum below, computed from the fee
and rate already read with no further requests. A market whose fee cannot
be read, or that has no current USD rate, carries an `unavailable` reason
instead of an amount and no minimum.

The creating supply's guaranteed delivery (the quote's `minAmountOut`)
must reach the creation minimum: twice the creation fee, or the token
amount worth 5 USD at the exact rate (rounded up), whichever is more.
Twice the fee leaves a supply at least the size of the fee at the delivery
floor; the 5 USD term keeps an account from being created for a supply
worth a few cents. The preview carries the minimum as `creationMinimum`
(amount and USD value), and a quote below it is refused with the minimum
named. Execute re-checks the minimum against the committed quote at the
current rate before anything is persisted or sent.

Outcomes come from the receipt trees of the delivery transactions 1Click
reports: Burrow `deposit` events and `ft_resolve_transfer` amounts for our
MCA. `SUPPLIED`, `PARTIAL` (the rest refunded into the MCA), `STRANDED`
(refunded into the MCA), or `DELIVERY_UNRESOLVED` (evidence incomplete or
a refund outside the MCA), which stays active and is retried.

### Withdrawal

A relayer business: `simple_withdraw` of the relayer fee, an optional MCA
token registration, an `execute` `Withdraw` of `max_amount` (the net
amount in internal units; the fee leaves supply through `simple_withdraw`),
then, after two blocks, registration of and
`ft_transfer` to a committed 1Click deposit address that pays the wallet.
The quote's refund address is the MCA.

### Sweep

A relayer business that moves an MCA plain token balance: a `GasPayment`
in native NEAR from the MCA's own NEAR balance, then either
`ft_transfer_call` to Burrow with `msg: ""` (back into supply) or
registration of and `ft_transfer` to a committed 1Click deposit address.

The balance swept is read at a final block. Part of it may be attributed:
`STRANDED`, `PARTIAL`, or `EXEC_FAILED` records that left tokens in the
MCA. The rest is recovered balance, owned because the MCA is bound to the
identity, with origin unknown. A sweep records the attributed operations it
covers (`Covers`) when it moves at least their total; once its exec moved
the tokens (supplied, or sent to the deposit address) those records stop
counting as funds in the account. Whatever the sweep itself left behind
(a partial supply, a refunded leg) is its own `STRANDED` record.

The attributed figure is a provenance estimate, not a balance. It can
overstate after a partial sweep (a sweep of less than the attributed total
covers no record) or a late refund (a refund attributed after a sweep has
already moved it). It never changes what is swept, which is bounded by the
balance read at the final block.

The MCA must cover the fee and the business's attached deposits from its
unlocked NEAR (balance less storage staking), without counting the deposit
the relayer attaches to `exec`, which the signature does not bind. MCAs
are created with about 0.1 NEAR and a sweep costs about 0.002 NEAR.

### States

| Kind | Path |
| --- | --- |
| Supply | 1Click leg states, `DELIVERED`, then `SUPPLIED`, `PARTIAL`, `STRANDED`, or `DELIVERY_UNRESOLVED` |
| Withdrawal, sweep out | `SIGNED`, `SUBMITTED` or `SUBMIT_UNKNOWN`, then `DELIVERING` (1Click leg) to `WITHDRAWN`, or `REFUNDED` to `STRANDED`; a sweep out is `EXECUTED` while its evidence is unresolved |
| Sweep into supply | `SIGNED`, `SUBMITTED` or `SUBMIT_UNKNOWN`, then `SUPPLIED`, `PARTIAL`, or `EXEC_FAILED`; `EXECUTED` while the exec's evidence is missing or contradictory |
| Any relayer business | `NOT_EXECUTED` (nonce unconsumed past the deadline), `EXEC_FAILED` (nothing reached the deposit address), `NONCE_CONSUMED_UNATTRIBUTED` (nonce consumed, no exec carrying the signature found) |

A refunded withdrawal or sweep leg is not terminal: the refund returns to
the MCA and must be attributed first.

## Evidence rules

Conclusions that something happened, or did not, come only from finalized
evidence:

- a transaction tree is used only when it is final, every receipt has an
  outcome, and every receipt the outcomes name is present;
- the status returned must describe the hash requested;
- evidence for a call is read from that call's own receipt subtree, so
  sibling work in the same transaction (another account's identical fee
  transfer, for example) is never credited to it;
- every transaction 1Click reports must be established before a refund is
  attributed; one unreadable hash keeps the operation unresolved;
- negative conclusions about discovery are corroborated across RPC
  endpoints, since responses are unauthenticated.

### Relayer submission

The signed request is persisted before any network call. A lost reply is
`SUBMIT_UNKNOWN`: before the deadline the identical request is resubmitted
at most once a minute (it cannot execute twice); after the deadline plus
two minutes at a final block, an unconsumed nonce is `NOT_EXECUTED`,
flagged unconfirmed until U5 is checked. An exec counts as ours only when
its arguments carry the persisted signature and the exact canonical
business. A failed exec of the same business (an identical resubmission
rejected after the original executed) is skipped in favor of the admitted
one.

### Withdrawal attribution

From the exec's subtree: the amount sent from the MCA to the deposit
address (any amount funds the 1Click leg, which is then tracked whatever
else failed), the fee (credited only with Burrow's `withdraw_succeeded`
for the MCA in the subtree), and transfers from Burrow into the MCA less
what was sent, recorded as `STRANDED`. Burrow's events must account for
everything transferred; a discrepancy is noted on the operation.

### Sweep attribution

Also from the exec's subtree, and terminal only on positive evidence. A
sweep out is `DELIVERING` once anything reached the deposit address and
`EXEC_FAILED` only when a step failed and none of the token left the MCA.
A sweep into supply needs exactly one resolution of the MCA's transfer to
Burrow for the signed amount, with Burrow's `deposit` event equal to the
amount used: all used is `SUPPLIED`, part `PARTIAL`, none (returned in
full) `EXEC_FAILED`; a failed transfer with nothing leaving the MCA is
also `EXEC_FAILED`. Missing, ambiguous, or contradictory evidence leaves
the sweep `EXECUTED`, active and retried with backoff.

### Refund attribution

A refunded leg becomes `STRANDED` only when 1Click reports the refunded
amount, exactly one verifier payout (`dip4` `ft_withdraw`) of the token to
the MCA for that amount was made with no message (so no MCA code ran on
receipt), the NEP-141 transfer for it is in the payout's subtree, and no
transfer of the token out of the MCA appears in any reported transaction.
The payout shape for a NEAR-origin deposit is assumed, not yet observed;
a different shape leaves the refund unattributed, which fails closed.

## Admission gate

Every relayer business (withdrawal, sweep) requires all of:

1. no local operation for the MCA is `SIGNED`, `SUBMITTED`, or
   `SUBMIT_UNKNOWN`;
2. finalized chain time is past the account's first-seen chain time plus
   ten minutes plus a two-minute margin, and the host clock is within two
   minutes of chain time (signed deadlines are five minutes after the
   read block's timestamp; the validator refuses more than ten);
3. every nonce below the current one is verified: starting from the
   account's persisted verified prefix (nonce 0 for a new record), each
   nonce up to current − 1 maps to an exec found through the relayer
   history and read on chain, whose transaction is final and complete;
   the verified prefix advances and is persisted;
4. for a sweep, no supply of the same token is `DELIVERED` or
   `DELIVERY_UNRESOLVED` (a pending delivery can still change the
   balance).

A gap in the nonce chain (a nonce consumed outside the relayer) leaves
admission unresolved.

## Fees

| Business | Fee | Caps |
| --- | --- | --- |
| Withdrawal | relayer `GAS_FEE` for the token plus 0.00125 NEAR per storage deposit at `get_near_value`, paid from supply | 2 % of the net amount and 2 USD at the token's ticker rate, or its fresh 1Click price without one |
| Sweep | `GAS_FEE` for `near` plus 0.00125 NEAR per storage deposit, as a native NEAR `GasPayment` | 2 % of the swept amount (fee valued at `get_near_value`) and 2 USD at the NEAR ticker rate, or the fresh 1Click wNEAR price without one |

Fees round up and net amounts round down in integer arithmetic. The USD
cap converts at the exact value of its price and rounds down, so the cap
is never worth more than 2 USD at that price. The caps guard
against a poisoned relayer configuration; they are not tuned from
observed acceptance. `get_near_value` is a rate configured on the AM
(3 USDC per NEAR at every reading so far), not a market price, so the 2 %
check on a NEAR-denominated fee is approximate.

The relayer configuration lists `GAS_FEE` for several tokens, and the MCA
code has a `GasPaymentAction { token_id, amount }`. A scan of every MCA's
relayer history on 2026-11-17 (5,486 records, 744 batches) found 37
`GasPayment` requests: 18 in native NEAR, 17 in `wrap.near`, and 2 in
`lst.rhealab.near`. The token payments are on early accounts such as
`rhea000025` (deadlines in August 2026, one in an older business format)
and pay for withdrawals of `lst.rhealab.near`. None is in USDC, USDT, or
ZEC. Sweeps use native NEAR, which is observed for every market and needs
no token balance beside the one swept.

## Recovery

After a seed-only restore, login derives every identity version and looks
each up on the AM; positions, plain balances, and the nonce are read at a
final block and reported as recovered holdings. In-flight operations from
the previous installation are not recovered. The first sighting records
the account's first-seen chain time, which starts the admission window.
Concurrent installations of the same seed are not supported for lending.

## Recovery limitation: execution outside the relayer

Recovery and attribution follow only `exec` transactions submitted by the
relayer. The deployed MCA code (v0.1.6; the source is private) also
exports `on_auth`, the hook `intents.near` calls for an `auth_call`
intent, and `execute_zcash_business`, and its identifiers include a
business-with-signature message type. How these entry points authorize a
business is not established from the code alone, and no execution through
them has been observed on the accounts sampled.

If a business can run through such an entry point, it consumes a nonce in
a transaction that is not a relayer `exec`. Signed requests are public
once submitted (the relayer's batch endpoint returns them without
authentication). Such an execution runs exactly the signed business, but
attribution finds no relayer exec carrying the signature, so the
operation stays `NONCE_CONSUMED_UNATTRIBUTED`, and the nonce walk finds a
gap and holds admission unresolved. Both fail closed, at the cost of
liveness: withdrawals and sweeps stay blocked until the execution is
understood. Comprehensive recovery is not claimed until this is
validated, by a funded check or the contract source.

## Live checks

`go test -tags live ./wallet/rhea -run Live -v` reads mainnet, the relayer
API, and 1Click without signing anything: the pinned templates and latest
MCA code hash, every view decoder, the captured transactions read back
through the archival fallback with identical extracted evidence, the
relayer configuration and history formats, and dry 1Click quotes for both
supply shapes (to an existing MCA with the supply message, and to the AM
with a register-and-supply message).

## Simnet

The simnet mock (`util/testing/one-click`, started by its `harness.sh`)
simulates the deployment `rhea.Simnet` beside its 1Click API: an account
manager, MCAs, Burrow, and two market tokens (USDC and USDT as
`usdc.test.near` and `usdt.test.near`), all inside the mock. A `--simnet`
wallet's lending engine reaches it through `RheaTest` (`wallet/app`): the
mock's NEAR JSON-RPC subset at `/rhea/rpc` and its relayer API at
`/rhea/relayer`. Supplies are 1Click payouts from a harness chain into
the RHEA world; withdrawals and sweeps leave through 1Click to a harness
chain.

The simulation follows what the engine relies on:

- the chain produces blocks; each block snapshots the state, so views at
  a named block and a final block lagging the head are served from
  history;
- an exec consumes its nonce in its own receipt, and every child receipt
  executes in a later block, so a transaction is incomplete until its
  last receipt runs and that block is final;
- each MCA re-encodes the business and recovers the EIP-191 signer,
  rejecting with `InvalidEvmSignature`, `WalletDoesNotExist`,
  `DeadlineExpired` (exec block time past the deadline), and
  `InvalidNonce`; the verification is written independently of
  `wallet/rhea`;
- receipts carry the mainnet event shapes (NEP-141 transfers, Burrow
  `deposit`, `withdraw_started`, `withdraw_succeeded`, `dip4`
  `ft_withdraw`, account manager fees) and `ft_resolve_transfer`
  results; delivery trees follow the captured mainnet deliveries;
- a business's transfer to a 1Click deposit address funds that swap, and
  a refund of a swap from NEAR is a verifier payout reported among the
  swap's NEAR transactions.

Fault injections (`RheaFaults`, set through `/test/rhea/faults` or the
harness's `rhea-faults` helper):

| Fault | Effect |
| --- | --- |
| `lostResponses` | the relayer executes the next submissions but drops the reply |
| `execDelay` | exec inclusion waits; past the deadline it fails `DeadlineExpired` |
| `holdReceipts` | child receipts wait while the exec has consumed its nonce |
| `receiptDelayBlocks` | blocks added before every child receipt |
| `finalLag` | the final block lags the head (stale finalized views) |
| `failWithdraws` | Burrow's `execute` Withdraw fails |
| `rejectBurrowDeposits` | Burrow refuses deposits, refunding into the MCA |
| `rejectForwards` | a new MCA returns the account manager's forward |

`/test/rhea/warp` advances chain time (the admission window),
`/test/rhea/nonce` sets an MCA's nonce (a business executed elsewhere),
and `/test/rhea/fund` credits a plain balance of unknown origin; the
1Click `/test/force` endpoint refunds, fails, or stalls a swap.

`TestRheaSimnetEndToEnd` (`wallet/core`) runs supply with account
creation, withdrawal, and both sweeps against the mock over HTTP through
the engine's production wiring; `TestRheaSimnetFaults` covers a lost
reply, a failed Withdraw, a refunded withdrawal leg swept back into
supply, an expired deadline, a stale nonce, held receipts, a delivery
refused by Burrow, a forward returned to the account manager, and an
engine restart mid-flight.

`TestRheaSimnetRestore` restores the identity into a clean database
against the same mock: with an exec's child receipts held past the
restored admission window under stale finalized views, withdrawals and
sweeps stay blocked until the receipts run and the nonce checkpoint
advances; and a completed refund in the MCA is discovered as a balance of
unknown origin and swept without the operation that caused it.

A business, delivery, or sweep that succeeds against the mock is not
evidence the deployed contracts accept it. The mock simplifies:

- storage: verifier and Burrow payouts register their recipients with the
  token automatically, ft_transfer_call to a recipient is not checked for
  registration, and storage deposits are not priced against storage used;
- attached deposits: the one-yoctoNEAR deposits NEP-141 and Burrow
  require are not checked, and gas is neither metered nor limited;
- Burrow: a withdrawal checks only the account's own supply, not the
  asset's available liquidity; shares equal balances; there is no
  interest, price oracle, collateral, or borrowing;
- the relayer accepts any well-formed request for a known MCA without
  checking its fee, and `exec` is not restricted to the relayer as caller;
- the account manager serves the pinned templates, so template changes are
  never simulated, and market parameters are fixed (six decimals, twelve
  extra decimals, 3 tokens per NEAR, 0.3 tokens creation fee);
- after a failed request the simulated MCA still runs the rest of the
  business (the real behavior is U2);
- the world is in memory and does not survive a mock restart; `on_auth`
  is not simulated.

## Unverified behavior

| Item | Status | Handling |
| --- | --- | --- |
| U1 relayer fee acceptance | Fee formula mirrors the SDK; acceptance observed only through captured execs | Rejection resolves through `SUBMIT_UNKNOWN` |
| U2 failed `Withdraw` inside a business | Not observed | Per-step attribution; `EXEC_FAILED` with moved amounts recorded |
| U3 rejected recipient message or failed AM forward | Not observed | `DELIVERY_UNRESOLVED` until refund destinations are known |
| U4 `exec` from a caller other than the relayer | `InvalidCaller` exists; other entry points unexamined (see the recovery limitation) | Unattributed executions stay active; admission unresolved |
| U5 nonce and deadline semantics | `InvalidNonce` and `DeadlineExpired` exist in the code | `NOT_EXECUTED` flagged unconfirmed |
| NEAR-origin refund shape | Assumed `dip4` `ft_withdraw` to the refund address | Unattributed refunds stay `REFUNDED` |
| Token-denominated `GasPayment` | Observed only in `wrap.near` and `lst.rhealab.near` on early accounts | Sweeps use native NEAR |

The release-gate procedure covers U5 and the refund shape, observes U1
once, and leaves U2 through U4 to optional checks or later validation;
its coverage table states which step covers what.
