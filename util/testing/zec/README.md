# ZEC Simnet Test Harness

The harness script starts two connected
[Zebra](https://github.com/ZcashFoundation/zebra) (`zebrad`) regtest nodes,
**alpha** and **beta**, mines some blocks, and creates a funded light wallet
for each node. Every network upgrade through NU7 is active from height 1,
matching mainnet, which activated NU7 at height 3,543,405.

Zebra has no wallet and no command-line RPC client, so this harness differs
from the bitcoind-style harnesses in two ways:

- The control scripts call the node JSON-RPC directly with `curl` and `jq`.
- The **alpha** and **beta** funder wallets are
  [zcash-devtool](https://github.com/zcash/zcash-devtool) light wallets. Each
  one syncs through the lightwalletd-protocol gRPC server built into its node,
  which is the same interface a light wallet under test connects to. No
  separate lightwalletd process runs.

Each funder starts with 312.4916995 ZEC in the Ironwood pool, from shielding
150 blocks of transparent coinbase into 16 notes of about 19.53 ZEC. Ironwood
is the shielded pool added by NU6.3. From that upgrade a payment to the
Orchard receiver of a unified address creates an Ironwood note.

## Dependencies

`zebrad`, `zcash-devtool`, `tmux`, `curl`, `jq`, `awk` and `watch` must be in
`PATH`.
The harness is tested with Zebra **7.0.0** and zcash-devtool commit
**0b3436b**.

Zebra publishes release binaries. The Linux x86-64 build needs glibc 2.34 or
newer:

```sh
curl -LO https://github.com/ZcashFoundation/zebra/releases/download/v7.0.0/zebrad-7.0.0-x86_64-unknown-linux-gnu.tar.gz
curl -LO https://github.com/ZcashFoundation/zebra/releases/download/v7.0.0/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
tar xzf zebrad-7.0.0-x86_64-unknown-linux-gnu.tar.gz zebrad
install -D -m 755 zebrad ~/bin/zebrad
```

The last line installs into `~/bin`. Any directory in `PATH` works.

zcash-devtool has no release binaries and is built from source with its
`regtest_support` feature. It needs Rust 1.89 or newer (tested with 1.90) and
compiles about 630 crates:

```sh
cargo install --locked --git https://github.com/zcash/zcash-devtool \
  --rev 0b3436bb8d235c67cd36b800cb45a7b1b489c415 --features regtest_support
```

## Running

```sh
./harness.sh
```

Setup takes about 17 seconds and ends attached to the tmux session
`zec-harness` at height 422:

| Window          | Contents                              |
|-----------------|---------------------------------------|
| 0 `alpha`       | alpha node log                        |
| 1 `beta`        | beta node log                         |
| 2 `harness-ctl` | shell in the control script directory |
| 3 `miner`       | `watch -n 15 ./mine-alpha 1`          |

Window numbers start at the tmux `base-index` setting, 0 by default.
`export NOMINER="1"` before running the script skips the miner window.

If a setup step fails, the script stops and leaves the session running so that
the node windows can be read. `~/dextest/zec/harness-ctl/quit` shuts it down.

Everything is written under `~/dextest/zec`, which is deleted at the start of
each run:

```text
~/dextest/zec/
  alpha/                    node state and alpha.toml
  beta/                     node state and beta.toml
  wallets/alpha/            alpha funder wallet
  wallets/beta/             beta funder wallet
  harness-ctl/              control scripts
  activation-heights.toml   upgrade schedule in zcash-devtool's format
```

## Ports

| Node  | P2P     | JSON-RPC | Light-client gRPC |
|-------|---------|----------|-------------------|
| alpha | `33764` | `33768`  | `33770`           |
| beta  | `33765` | `33769`  | `33771`           |

All ports listen on `127.0.0.1`. The JSON-RPC servers run with cookie
authentication disabled. The gRPC servers are plaintext and unauthenticated.

## Funder wallets

The funder mnemonics are in `harness.sh`. These are the default addresses of
account 0:

```text
alpha unified      uregtest13pvma5dl78lka0cl83u4jmm78emspd6rm9e56xgm3ttk4pxcwcr4guhdc5u8mg7z3fyr9zjec593khe5m4c3a825yx22lcjwm4anp2xvu4ej5frpc5dm25294c8w3x0pnnxjde38tz4fpgaglrrhx0k7z5259slahnxng23mhn8lhhff8dkeznyhynrfwlu3e8mzedknqathcdlv7hd
alpha transparent  tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm
alpha Sapling      zregtestsapling1uwt3xqagtnemsucergl4f5lcjj0qsh4hu2avqs6gp3fq62yke38dcyxg5ddewwx5pnjrkfmrr6w

beta unified       uregtest109f95z55t5l6xmdgdlshu60xggacda887hvxsv6kp8mpau9s8kq4n7tcydfecf7hmcev85jmt77p5hr0nn94l9d7neld8dyk3w8frq5gscfpkml39y3mdnl9gyp8jv3tqlaxq33t5hv6wz7wfq840tulv942npgwm5ulptssgcc239ududzrav2wv50ahkdl97njf6mauhqtjghy3tz
beta transparent   tmAsfbvNQVxLcp88CBhp7AW74XdapKbpo5n
beta Sapling       zregtestsapling1tav8vzg0j7lq8xp9ur2unmahskuqnw6zgf4w6vs2keyug9y3rnx274f4w6pwvcz55yxmgp3rgd9

miner transparent  tmUV6poBiYV5thY9QeovauFFYUMAejmfKNh
```

A funder pays transparent, Sapling and unified addresses, always from its
shielded funds. Change is not spendable until it is mined, so the number of
notes bounds the number of sends between blocks: 16 sends of up to about 19.5
ZEC each were made from one funder in a single block. A larger send combines
notes.

`./mine-alpha`, `./mine-beta` and the miner window pay the miner address, which
belongs to neither funder. Funder balances therefore change only through sends
and `./mine-to`. The funder `shield` command spends every transparent output
the wallet holds, including immature coinbase, so a funder that received
coinbase on every block could never shield.

## Harness control scripts

The scripts are in `~/dextest/zec/harness-ctl`.

- `./alpha` and `./beta` make a JSON-RPC call to the node:
  `./alpha getblockcount`, `./beta getpeerinfo`. Arguments that parse as JSON
  are sent typed and anything else is sent as a string, so
  `./alpha getblockhash 5` and
  `./alpha getaddressbalance '{"addresses":["tm..."]}'` work as written. A
  failed call prints the error and exits with a non-zero status.
- `./mine-alpha n` and `./mine-beta n` mine `n` blocks (default 1) on the
  respective node.
- `./mine-to address n` mines `n` blocks (default 1) on alpha with the coinbase
  paid to `address`, which may be transparent, Sapling or unified. This funds
  any wallet without using a funder.
- `./send-alpha address amount [memo]` and `./send-beta address amount [memo]`
  sync the funder and send `amount` ZEC, a decimal number with at most 8
  decimal places. The transaction id is the last line of output. A send takes
  about 2 seconds.
- `./balance-alpha` and `./balance-beta` sync the funder and print its
  balances in ZEC, per pool.
- `./wallet-alpha` and `./wallet-beta` run any `zcash-devtool wallet`
  subcommand against the funder: `./wallet-alpha list-tx`,
  `./wallet-beta list-addresses --receiver sapling`, `./wallet-alpha shield`.
  The server and identity flags are filled in for `sync`, `enhance`,
  `get-info`, `send` and `shield`. Logging is at warn level unless `RUST_LOG`
  is set. Memos appear in `list-tx` only after `./wallet-alpha enhance`, which
  fetches the full transactions.
- `./reorg depth` replaces the last `depth` blocks (default 1) with
  `depth + 1` new blocks on both nodes. It exits 1 if beta has not followed
  alpha after 30 seconds.
- `./quit` shuts down the nodes and closes the tmux session. It can be run
  from any shell.

## Connecting a light wallet

A wallet under test connects to `127.0.0.1:33770` (alpha) or `127.0.0.1:33771`
(beta) with the lightwalletd `CompactTxStreamer` gRPC protocol, without TLS.

- **Network parameters.** The chain is regtest with Overwinter through NU7 at
  height 1. Address prefixes are the regtest ones (`tm`, `zregtestsapling`,
  `uregtest`). A wallet built on librustzcash needs a local-consensus network
  with these heights, because the built-in testnet parameters place the
  upgrades at testnet heights and produce transactions the nodes reject.
- **Chain name.** `GetLightdInfo` and `getblockchaininfo` report the chain as
  `test`.
- **Birthday.** The earliest usable wallet birthday is 2. A scan that starts
  at height 1 requests the tree state at height 0, and Zebra rejects a block
  identifier of height 0 with `block id must specify a hash or a height`.
- **Funding.** `./send-alpha address amount` pays from a funder.
  `./mine-to address n` pays `n` block subsidies directly as coinbase.
- **Propagation.** A block reaches the other node in about 20 ms. A
  transaction submitted to one node takes about 6 seconds to reach the other
  node's mempool. The miner window mines on alpha, so a transaction submitted
  through beta less than 6 seconds before a block waits for the next one.

A second zcash-devtool wallet is created the same way the funders are. An
empty mnemonic on standard input generates a new one:

```sh
mkdir w
echo "" | zcash-devtool wallet -w w init --name test -i w/age-identity.txt \
  --birthday 400 -n regtest -s 127.0.0.1:33770 --connection direct \
  --activation-heights ~/dextest/zec/activation-heights.toml
zcash-devtool wallet -w w list-addresses
zcash-devtool wallet -w w sync -s 127.0.0.1:33770 --connection direct
zcash-devtool wallet -w w balance --min-confirmations 1
```

## Differences from other harnesses

- **No node wallet.** `zebrad` serves none of the zcashd wallet RPCs
  (`z_sendmany`, `listunspent`, `gettransaction`). The zcashd wallet-RPC
  client in `wallet/asset/zec` cannot use this harness.
- **Block times.** Regtest blocks are stamped from the 2011 genesis time, one
  second after the median time of the preceding blocks, and advance about one
  second per six blocks. Wallet history shows dates in February 2011, and
  logic that judges sync progress by tip age sees a stale chain.
- **Subsidy.** The subsidy is 2.08333333 ZEC per block through height 858,
  halves at height 859, and halves again every 864 blocks. Total supply
  approaches about 3,590 ZEC. With one block every 15 seconds the first
  halving arrives about 1.9 hours after setup.
- **Coinbase.** Transparent coinbase matures after 100 blocks. Shielded
  coinbase is spendable in the next block. Coinbase paid to a unified address
  lands in the Ironwood pool. A block with coinbase paid to a unified address
  takes 0.4 to 0.7 seconds to mine and one paid to a Sapling address about 0.17
  seconds, against under a millisecond for a transparent one.
- **Shielded coinbase memos.** After `enhance`, zcash-devtool holds memo bytes
  for shielded coinbase notes that are not valid text, and its `list-tx` then
  fails with `Invalid UTF-8`. The funders hold no coinbase notes for this
  reason. A zcash-devtool wallet funded with `./mine-to` to a Sapling or
  unified address is affected.
- **Reorgs.** Zebra has no `disconnectnode`. `./reorg` invalidates blocks on
  alpha and mines a longer replacement chain, which beta follows.
  Transactions in the replaced blocks are dropped. They do not return to the
  mempool, and a wallet sees them as unmined until they expire. The funders'
  funding transactions are at heights 401 and 402, under 20 further setup
  blocks. A reorg that reaches those heights leaves a funder with no
  spendable balance, and the harness then has to be restarted.
- **Restarts.** A node that starts behind its peer can take over a minute to
  fetch even a few blocks, so the harness connects beta to alpha before
  mining. After a manual restart of one node the two reconnect without
  intervention in about 90 seconds. Blocks mined on either side in the
  meantime form a fork, and both nodes settle on the longer chain. `addnode`
  is not needed and fails because the peer address is already known.
- **Experimental gRPC server.** Zebra labels its built-in light-client server
  experimental. `Ping` is unimplemented, compact blocks carry no `header`, and
  `GetLightdInfo` leaves the build fields empty. Zebra's documentation
  recommends lightwalletd in front of Zebra for public service, so a wallet
  that passes against this harness has not been tested against the software a
  public server is likely to run.

## Dev stuff

If things aren't looking right, look at the node windows for errors. In tmux,
navigate between windows by typing `Ctrl+b` and then the window number shown in
the status bar at the bottom.
