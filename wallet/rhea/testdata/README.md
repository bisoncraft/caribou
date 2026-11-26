# RHEA lending test data

Mainnet captures taken on 2026-11-17. The tests treat them as fixed vectors;
re-capturing changes balances and nonces that tests assert.

## Sources

- Transactions: `EXPERIMENTAL_tx_status` with `wait_until: FINAL` from
  `https://archival-rpc.mainnet.fastnear.com` (the regular endpoints had
  already dropped the older transactions). The `proof` arrays of every
  outcome are removed; nothing else is altered.
- Views: `call_function` and `view_account` queries from
  `https://free.rpc.fastnear.com`, all at final block 225016693
  (`4jRBh7o3ND46sRyPzqLenVKarw2NcCYxnwf72aEfehHH`, timestamp
  1794949549020383055 ns). Results are stored as the exact returned
  strings.
- Relayer: `https://api.rhea.finance` responses, unaltered.
- 1Click: the `/v0/tokens` entries for the market tokens.

## Files

| File | Content |
| --- | --- |
| `tx-create-supply-1aqcDa8Y.json` | 1Click delivery that created `rhea001594.multica.near` through `BurrowRegisterAndSupply` (USDT) |
| `tx-supply-existing-Ey4MNC5d.json` | 1Click delivery to an existing MCA (`rhea001624`) through the collateral template |
| `tx-exec-4SDDqzRj.json` | Relayer `exec` of a supply-only USDC withdrawal (`simple_withdraw`, `execute` Withdraw, deposit-address transfer) on `rhea000747` |
| `tx-exec-gaspayment-4175G6iY.json` | Relayer `exec` of a `GasPayment` business moving an MCA plain USDT balance to a deposit address (`rhea001589`) |
| `exec-vectors.json` | Arguments of five relayer `exec` transactions (`Rq7EVPvs`, `4SDDqzRj`, `DDYfxVN1`, `9dFTx2A9`, `4175G6iY`) with the MCA code hash each ran under |
| `views.json` | Account manager, MCA, Burrow, and token views at block 225016693, plus `view_account` of three MCAs |
| `governance.json` | DAO policies and admin roles at block 225016693 |
| `relayer-config.json` | `get_multichain_lending_config` |
| `relayer-history.json` | `get_multichain_lending_history` for `rhea001569`, page 1 of size 5 |
| `relayer-batch.json` | `get_multichain_lending_data` for the batch of `9dFTx2A9` |
| `oneclick-tokens.json` | 1Click catalog entries for USDC, USDT, wNEAR, and ZEC |

Every `exec` vector ran under MCA code hash
`FDX6nYTmwVVwkjSXwFZ8xRjkPsyU1BEt1QqB9TbXwD11` (MCA version 0.1.6, the
account manager's latest). `rhea000001` still runs
`GaLHJv6RqCfniprg6dc7K484Ga3i1qjEYPkSRk5aL31n` (version 0.1.5).

## Governance at capture

Checked at block 225016693 (2026-11-17).

- Account manager `multica.near`, version 0.3.4. Super admin
  `ref-labs.sputnik-dao.near`, whose council of five (`farbread8025.near`,
  `nicktrust.near`, `reffer.near`, `hajimi.near`, `wannamakeit.near`)
  passes proposals with more than half the votes, three of five. Upgrade
  staging duration is null: no timelock between staging and deploying
  code. A staged code hash `F5FMpGcf5WHs1bWjYg4s4rSFSZUMbbchNx6m5vWUqEXj`
  is present.
- Burrow `contract.main.burrow.near`, version 0.16.2. Owner
  `burrow.sputnik-dao.near`, council of five (`farbread8025.near`,
  `nicktrust.near`, `brrrb.near`, `brhost.near`, `wannamakeit.near`),
  three of five. Guardians `nicktrust.near` and `doublerose.near`.
- `farbread8025.near`, `nicktrust.near`, and `wannamakeit.near` sit on both
  councils and together can upgrade both contracts.
- The public Burrow source (`ref-finance/burrowland`, commit `2110c70`) is
  version 0.15.7, older than the deployed 0.16.2. The account manager and
  MCA sources are not public. None of the deployed code is
  source-verifiable.
