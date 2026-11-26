# RHEA lending release-gate test

A manual, funded procedure on mainnet that checks behavior the unit tests
and the read-only live checks cannot. Design background is in
[rhea-lending.md](rhea-lending.md).

Coverage of the open items in the design doc:

| Item | Covered by |
| --- | --- |
| U1 relayer fee acceptance | Step 3: one acceptance of the computed fee, not a validation of the fee formula across tokens or configurations |
| U2 failed `Withdraw` inside a business | Not covered; unvalidated pending a controlled test (see the optional checks) |
| U3 rejected recipient message | Not covered by the mandatory steps; optional check, funds at risk |
| U4 execution outside the relayer | Not covered by the mandatory steps; the optional check probes `exec` only, not the other entry points |
| U5 nonce and deadline rules | Steps 4 and 5 |
| NEAR-origin refund shape | Step 6 |
| Restore, admission, sweeps | Steps 7 to 9 |

Seed-only recovery for lending is not claimed until the mandatory steps
pass and the recovery limitation in the design doc (execution outside the
relayer) is resolved.

## Preparation

- A mainnet build of the branch under test, started with a data directory
  dedicated to this test, and a seed whose EVM identity has no MCA
  (`GET /api/rhea/account` reports the account as not found).
- About 30 USD in a wallet asset 1Click supports (BTC or ZEC) for supplies.
- A NEAR account with about 3 USDC and 0.1 NEAR for seeding plain balances
  and the refund captures. Caribou's own NEAR wallet works; its account
  name is referred to as `$NEARACCT`.
- `curl`, `python3`, and `jq`. The authenticated API is reached with the
  session cookie of a logged-in browser (`$COOKIE`, the full `Cookie`
  header value) at `$API` (for example `http://127.0.0.1:5758/api`).
- A log directory. After every step, save the operations list:

  ```sh
  curl -s -H "Cookie: $COOKIE" "$API/rhea/ops" > ops-<step>.json
  ```

For every operation record its ID, the states it passed through, the
relayer batch ID, the exec transaction hash, and the 1Click deposit
address. Look up each exec on an explorer and keep the hash list; failed
or surprising transactions become test fixtures.

Shell helpers used below (`$MCA` is the account's MCA once it exists):

```sh
nonce() {
  curl -s -X POST -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"query\",\"params\":{\"request_type\":\"call_function\",\"finality\":\"final\",\"account_id\":\"$MCA\",\"method_name\":\"get_nonce\",\"args_base64\":\"e30=\"}}" \
    https://free.rpc.fastnear.com |
    python3 -c 'import json,sys; print(bytes(json.load(sys.stdin)["result"]["result"]).decode())'
}
```

`resubmit.py` posts a persisted signed request to the relayer unchanged.
Arguments: the saved operations file, the operation ID, and `live` or
`expired`, the state the business deadline must be in (with 30 seconds of
margin against clock skew); it refuses to send otherwise.

```python
import json, sys, time, urllib.request

if len(sys.argv) != 4 or sys.argv[3] not in ("live", "expired"):
    sys.exit("usage: resubmit.py <ops.json> <op id> live|expired")
ops = json.load(open(sys.argv[1]))["ops"]
op = next(o for o in ops if o["id"] == sys.argv[2])
req = op["relayer"]["signedRequest"]
left = int(req["business"]["deadline"]) / 1000 - time.time()
print(f"deadline {'in' if left > 0 else 'passed'} {abs(left):.0f} s")
if (sys.argv[3] == "live" and left < 30) or (sys.argv[3] == "expired" and left > -30):
    sys.exit("deadline not in the required state; not sent")
body = {
    "mca_id": op["mca"],
    "wallet": json.dumps(req["signer_wallet"], separators=(",", ":")),
    "request": [json.dumps(req, separators=(",", ":"), ensure_ascii=False)],
    "page_display_data": "",
}
r = urllib.request.Request(
    "https://api.rhea.finance/multichain_lending_requests",
    data=json.dumps(body).encode(),
    headers={"Content-Type": "application/json"},
)
print(urllib.request.urlopen(r).read().decode())
```

The relayer's reply carries a batch ID; its result is at
`https://api.rhea.finance/get_multichain_lending_data?batch_id=<id>`.

## Steps

### 1. Supply with MCA creation

Before choosing an amount, `GET /api/rhea/account` lists a one-time
`creationFees` line per market. Expect USDC, USDT, and wNEAR priced at
the wallet's ticker rates (`tickerRates` in `GET /api/user`), each with a
`creationMinimum` worth about 5.00 USD, and ZEC marked `unavailable` with
`no current USD rate for ZEC` and no minimum (no ZEC asset is registered,
so no source prices ZEC); `GET /api/rhea/markets` lists ZEC with that
reason as `supplyUnavailable`. Preview a supply of about 10 USD of USDC.
Expect `creating: true` and a `creationFee` line with `oneTime: true`,
an amount equal to `get_create_mca_fee` plus the MCA's initial NEAR
balance at the token's near value (0.6 USDC at the 2026-11-17 config),
the two components `fixed` and `accountFunding`, and a USD value at the
wallet's displayed USDC rate. Expect a `creationMinimum` of about 5 USDC
(5.00 USD), and a preview of about 4 USD to be refused with that minimum
named. Record whether the fee amount matches what the account manager's
fee events report for the creation.
Execute. Expect the operation to reach `SUPPLIED`, the account to report
the new MCA, and the supply in the positions. Record the MCA as `$MCA`.

### 2. Second supply

Supply again to the same market. Expect no creation and `SUPPLIED`.

### 3. Partial withdrawal (U1)

Withdraw about half the supply to the source asset. Expect the preview's
`feeCapSource` to be `fiatRate`, `feeCap` 2 USD at the wallet's displayed
USDC rate, and `feeUSD` the fee at that rate. Save the operations list as soon as the operation
reaches `DELIVERING` (its exec is attributed), for step 4. Expect
`SUBMITTED`, then `DELIVERING`, then `WITHDRAWN`, with `fees` equal to the
previewed fee. The relayer accepting
the computed fee is the U1 observation. On the explorer the exec shows
`simple_withdraw` of the fee, `execute` with `Withdraw`, and the transfer
to the deposit address.

### 4. Identical resubmission (U5, nonce)

The replay must reach the relayer while the original business's deadline
is still valid (about four minutes after its exec); past the deadline a
`DeadlineExpired` rejection can win and the nonce result is inconclusive.
Right after the step 3 operation reaches `DELIVERING`, read `nonce`, run
`resubmit.py ops-3.json <op> live`, wait for the batch result, and read
`nonce` again. Expect a rejection naming `InvalidNonce` (from the relayer
or in a failed exec) and an unchanged nonce. A rejection naming
`InvalidEvmSignature` means the resubmitted bytes differ from the signed
ones, and a relayer refusal naming neither (a duplicate-request check, for
example) does not reach the MCA; both are inconclusive. If the window was
missed, repeat steps 3 and 4 with a small withdrawal.

### 5. Lost submission past its deadline (U5, deadline)

This needs a submission that never reaches the relayer while the relayer's
configuration and history stay reachable (planning reads the configuration
before anything is signed). A local intercepting proxy drops only the
submission request. With `mitmproxy` installed, save this as
`drop-submit.py`:

```python
from mitmproxy import http


def request(flow: http.HTTPFlow) -> None:
    if (flow.request.method == "POST"
            and flow.request.pretty_host == "api.rhea.finance"
            and flow.request.path.startswith("/multichain_lending_requests")):
        flow.kill()  # never forwarded upstream
```

Start the proxy with `mitmdump -p 8081 -s drop-submit.py`. Its first run
creates `~/.mitmproxy/mitmproxy-ca-cert.pem`. In another shell, build a
trust bundle and restart Caribou on the same data directory through the
proxy, with any Tor proxy setting removed (a SOCKS proxy takes precedence
over `HTTPS_PROXY` for the relayer client):

```sh
cat /etc/ssl/certs/ca-certificates.crt ~/.mitmproxy/mitmproxy-ca-cert.pem > rhea-ca.pem
HTTPS_PROXY=http://127.0.0.1:8081 SSL_CERT_FILE=$PWD/rhea-ca.pem <caribou command>
```

Confirm in the `mitmdump` output that `get_multichain_lending_config` and
`get_multichain_lending_history` requests pass. Read `nonce`, then preview
and execute a small withdrawal. Expect the killed `POST
/multichain_lending_requests` in the proxy log and the operation in
`SUBMIT_UNKNOWN`. The engine resubmits the identical request about once a
minute until the deadline; the proxy kills each one. Keep everything
running for eight minutes (a five-minute deadline plus the two-minute
margin plus slack). Expect `NOT_EXECUTED` with `relayer.unconfirmed:
true` and an unchanged nonce, and save the operations list.

Cleanup: stop Caribou, stop `mitmdump`, delete `rhea-ca.pem` and
`drop-submit.py`, and restart Caribou without the two variables.

Then run `resubmit.py ops-5.json <op> expired`, wait for the batch result,
and read `nonce`. Expect a rejection naming `DeadlineExpired` and an
unchanged nonce. Together with step 4 this confirms the `NOT_EXECUTED`
conclusion.

### 6. NEAR-origin refund into the MCA

This captures the refund shape for a NEAR-side 1Click deposit and leaves
a plain balance in the MCA with no local record of its origin. Request a
committed quote from `$NEARACCT`'s USDC to any asset, refunding to the MCA,
with a deadline two minutes out:

```sh
DL=$(date -u -d '+2 min' +%Y-%m-%dT%H:%M:%S.000Z)
curl -s -X POST -H 'Content-Type: application/json' https://1click.chaindefuser.com/v0/quote -d "{
  \"dry\": false, \"swapType\": \"EXACT_INPUT\", \"slippageTolerance\": 100,
  \"originAsset\": \"nep141:17208628f84f5d6ad33f0da3bbbeb27ffcb398eac501a31bd6ad2011e36133a1\",
  \"depositType\": \"ORIGIN_CHAIN\", \"destinationAsset\": \"nep141:usdt.tether-token.near\",
  \"amount\": \"1000000\", \"refundTo\": \"$MCA\", \"refundType\": \"ORIGIN_CHAIN\",
  \"recipient\": \"$NEARACCT\", \"recipientType\": \"DESTINATION_CHAIN\", \"deadline\": \"$DL\"}" | jq .
```

Wait until the deadline has passed, then send exactly 1 USDC from
`$NEARACCT` to the quote's `depositAddress` (register the deposit address
with `storage_deposit` first if needed). Poll
`/v0/status?depositAddress=<address>` until `REFUNDED`, and save the
status JSON. If 1Click fills the late deposit instead of refunding it, the
swap pays `$NEARACCT` and the step is repeated with a shorter deadline. For each hash in `swapDetails.nearTxHashes`, save
`EXPERIMENTAL_tx_status` from `https://archival-rpc.mainnet.fastnear.com`.

Expected shape (what refund attribution requires): a `dip4` `ft_withdraw`
event from `intents.near` with `receiver_id` equal to the MCA, the token,
`amount` equal to `refundedAmount`, and an empty `msg`, followed in that
receipt's subtree by the NEP-141 transfer from `intents.near` to the MCA.
Any other shape is a defect to fix before release; the captures become
fixtures either way.

### 7. Restore with a withdrawal in flight

Execute a withdrawal of a small amount and, as soon as it reports
`SUBMITTED`, stop Caribou. Move the data directory aside, restore the seed
into an empty data directory, and log in.

- `GET /api/rhea/account` reports the MCA, its positions, and the plain
  USDC balance from step 6 as recovered holdings, with admission closed
  while finalized chain time is within the first-seen window (ten minutes
  plus margin) and while the in-flight exec is not final.
- The in-flight withdrawal completes on its own; its payout arrives as an
  ordinary wallet receive and is not tracked as a lending operation.
- After the window, admission opens only once the relayer history covers
  the current nonce with final execs. Record how long that took.

Then execute a further small withdrawal and expect `WITHDRAWN`.

### 8. Sweep recovered balance into supply

Preview a sweep of USDC with no destination. Expect `recovered` equal to
the step 6 refund, `attributed` zero, a fee of 0.001 NEAR (no storage
deposit), and `feeCapSource` `fiatRate` with `feeCap` 2 USD at the
wallet's displayed NEAR rate. Execute. Expect `SUPPLIED`, the supply in the positions, and the
MCA's NEAR balance lower by the fee plus one yoctoNEAR.

### 9. Sweep out to a wallet

Send 1 USDC from `$NEARACCT` straight to the MCA (`ft_transfer` to
`$MCA`). Preview a sweep of USDC to the source asset. Expect a fee of
0.00225 NEAR (one storage deposit). Execute. Expect `DELIVERING`, then
`WITHDRAWN`.

### 10. Full withdrawal

Withdraw with an empty amount (the most available). Expect `WITHDRAWN` and
no remaining supply beyond internal-precision dust.

## Optional checks with funds at risk

Each of these risks the amount sent (about 1 USD) and answers an open
question.

- U3, rejected recipient message: a committed quote paying 1 USDC to
  `multica.near` with `customRecipientMsg` set to a `BurrowSupply` message
  for an EVM identity that has no MCA. Record where the tokens end up
  (refund to the verifier, kept by the AM, or returned to the refund
  address) from the delivery receipts.
- U4, direct `exec`: during step 5, while the request is persisted and its
  submission dropped, call `exec` on the MCA with the persisted request from
  a throwaway NEAR account before the deadline. Expect `InvalidCaller`. If it
  executes, record the transaction; Caribou will report the withdrawal as
  `NONCE_CONSUMED_UNATTRIBUTED`.
- U2, failed `Withdraw`: unvalidated pending a controlled test. Planning
  refuses a withdrawal larger than the market's available liquidity before
  anything is signed, so a failed `Withdraw` needs liquidity to drop
  between an otherwise valid signed plan and its execution, within the
  five-minute deadline. That depends on other users' activity and cannot be
  arranged on mainnet; the expected outcome (`EXEC_FAILED` with the fee
  recorded and nothing sent) rests on synthetic receipt trees in the unit
  tests and on the simnet mock's `failWithdraws` fault, until a real
  occurrence is captured.

## Results

Record, per step: pass or fail, operation IDs, transaction hashes, and
deviations. Failures and every new receipt shape go to the branch as
fixtures under `wallet/rhea/testdata` with a test that reproduces them.
