# Simnet Development Guide

This guide covers Boss Wallet's local simnet (a.k.a. regtest / `--dev` mode)
test harnesses: what they are, how to drive them, and how the wallet code
talks to them.

A "simnet" here means a fully local, deterministic blockchain network — not the
public testnets. The wallet refers to this network as `Simnet` in
`util.Network`, and `bossw --simnet` selects it. The default host for the
simnet wallet's HTTP server is `127.0.0.3:5758` (note the third `.3`), so you can run mainnet,
testnet, and simnet wallets side-by-side on one box.

Simnet harnesses only run on Linux or Mac OS.

## Repository layout

All harness scripts live under `util/testing/<chain>/`:

```
util/testing/
├── base/           # Base L2 (eth-derived)
├── bch/            # Bitcoin Cash
├── btc/            # Bitcoin (descriptor wallets, v26.0.0+)
├── dash/           # Dash
├── dcr/            # Decred
├── dgb/            # DigiByte
├── doge/           # Dogecoin
├── eth/            # Ethereum (geth --dev), incl. baseharness.sh
├── firo/           # Firo
├── ltc/            # Litecoin (MWEB-aware)
├── near/           # NEAR
├── polygon/        # Polygon
├── tatanka/        # Tatanka mesh (cross-chain coordinator)
├── walletpair/     # End-to-end two-wallet test fixture
├── xmr/            # Monero
└── zec/            # Zcash
```

Two harness families dominate:

1. **Bitcoin-family** (`btc`, `ltc`, `bch`, `doge`, `dgb`, `dash`, `firo`,
   `zec`). All share `btc/base-harness.sh` and configure it with per-chain
   env vars (`SYMBOL`, `DAEMON`, `CLI`, ports, mining addresses, descriptor
   files).
2. **Ethereum-family** (`eth`, `base`, `polygon`). All share `eth/baseharness.sh`
   with similar per-chain wiring.

`dcr`, `xmr`, and `near` have custom harnesses but expose the same control-script style.

## Dependencies

Every harness needs:

- **tmux** — each harness runs in a named tmux session and uses windows for
  the daemons.
- The chain's node binaries in `PATH` (e.g. `bitcoind` + `bitcoin-cli`, `geth`,
  `litecoind` + `litecoin-cli`, `dcrd` + `dcrctl` + `dcrwallet`, …). See each
  harness's `README.md` for exact version requirements.
- `bc` (used by some ETH-family math).
- For BTC v30+ descriptor wallets, `xxd` is used to hex-encode contract
  bytecode for the eth family.

## Running a harness

```bash
cd util/testing/btc
./harness.sh
```

Each harness:

1. Kills/clears any prior `~/dextest/<chain>` data directory.
2. Starts a fresh tmux session named `<chain>-harness`.
3. Spawns the node(s) in their own windows.
4. Creates per-wallet helper scripts under
   `~/dextest/<chain>/harness-ctl/`.
5. Mines initial blocks, funds the wallets, and drops you into a `harness-ctl`
   shell with `set +o history`.

Inside the `harness-ctl` window, navigate the tmux windows with `Ctrl+b`
then the window number (window 0 is the control shell; nodes are 1+).

### Bitcoin-family wallets

The BTC harness creates four wallets, each with a different funding profile —
**alpha**, **beta**, **gamma**, **delta**. They are not interchangeable;
tests that need varying UTXO shapes depend on the specifics:

| Wallet | Node  | Coinbase outputs | Spent / change outputs | Use case |
|--------|-------|------------------|------------------------|----------|
| alpha  | alpha | yes (mining)     | yes                    | mixed UTXO set, change |
| beta   | beta  | yes (mining)     | none                   | pure coinbase, never spent |
| gamma  | alpha | none             | yes (varied sizes)     | many small UTXOs |
| delta  | beta  | none             | yes                    | as gamma, on the beta node |

The wallet password is **`abc`** for encrypted wallets.

Per-wallet helper scripts in `harness-ctl/` are just the chain's CLI
preconfigured for that wallet:

```bash
./alpha getbalance
./gamma listunspent
./beta -named sendtoaddress address=... amount=0.1
./reorg              # 1-deep reorg of the alpha node
./quit               # shut down nodes + kill tmux session
```

### Ethereum-family wallets

The ETH harness runs a single private node, **alpha**, in `geth --dev` mode.
Blocks are mined every ~10 seconds in a dedicated mining tmux session — there
is no concept of separate wallets at the harness level; `eth.accounts[0]` is
the unlocked dev account that funds everything.

```bash
./alpha attach                                  # JS console on the dev node
./sendtoaddress 0xabc...def 1.5                 # send 1.5 ETH
./mine-alpha 5                                  # mine ~5 blocks
./quit
```

The `TESTING_ADDRESS` at the top of `eth/baseharness.sh`
(`946dfaB1AD7caCFeF77dE70ea68819a30acD4577`) is the wallet's client-internal
geth identity; the harness pre-funds it with 5000 ETH and writes the
funding-tx hash to `~/dextest/eth/test_tx_hash.txt` so tests can reuse it.

The harness also deploys the v0/v1 ETH swap contracts and the v0 ERC20 swap +
test-token contracts. The deployed addresses are written next to the data
directory; ERC20 helpers (`sendTokens`, `tokenBalance`, …) are added to the
attach console.

## Network/port conventions

Each harness pins its own port set so the families can coexist. Selected
examples:

| Chain  | Alpha RPC | Beta RPC | Notes |
|--------|-----------|----------|-------|
| btc    | 20556     | 20557    | regtest, RPC user/pass `user`/`pass` |
| ltc    | 20557 ↑   | 20558 ↑  | follows btc-style but offset; check `ltc/harness.sh` |
| eth    | 38556 (HTTP), 38557 (WS), 8552 (authrpc) | — | --dev mode, single node |
| base   | 39556 (HTTP), 39557 (WS), 8652 (authrpc) | — | eth-derived |

The authoritative ports are the `*_PORT` exports at the top of each
`harness.sh`. Read them before assuming.

The wallet binary on `--simnet` listens on `127.0.0.3:5758` by default
(`defaultSimnetHost` in `wallet/app/config.go`), so a typical multi-window
dev session is:

- Window A: harness shell (`./harness.sh` for one or more chains)
- Window B: `cd wallet/cmd/bossw && ./bossw --simnet --debuglevel=debug`
- Browser: `http://127.0.0.3:5758`

## How the wallet finds the harness

Each asset package's (`wallet/asset/<asset>`) simnet config points at the harness's data dir and RPC endpoint:

- **BTC-family**: the wallet uses the cookie-auth/credentials and ports from
  the harness. `wallet/asset/btc/simnet_test.go` opens a real wallet against
  `chaincfg.RegressionNetParams` and the alpha node.
- **ETH-family**: the wallet connects to `127.0.0.1:<ALPHA_HTTP_PORT>` (or WS
  for subscriptions).

The internal client uses the same default credentials as the harness
(`user`/`pass` for BTC, no-password JSON-RPC for ETH dev mode), so a freshly
booted `bossw --simnet` "just works" against a fresh harness on the same
machine.

## Running tests against a harness

Tests that need a live harness are gated by build tags:

| Tag            | What it means |
|----------------|---------------|
| `harness`      | Full harness must be running. |
| `rpclive`      | An RPC node is reachable (subset of `harness`). |
| `electrumlive` | An ElectrumX server is reachable (for `btc`/`ltc` SPV). |
| `live`         | Live mainnet/testnet endpoints — **not** simnet. |

Common invocations:

```bash
# Run BTC harness tests against a running btc harness
go test -tags harness -count=1 -v ./wallet/asset/btc/...

# Run ETH harness tests
go test -tags harness -count=1 -v ./wallet/asset/eth/...
```

## Debugging the harness

- **Check the node windows first.** `Ctrl+b 1` switches to the alpha node
  window in tmux; the daemon's stderr is right there. The harness READMEs
  call this out for a reason — RPC connection errors from the wallet are
  almost always something the node already complained about.
- **Zombie processes.** If `harness.sh` fails with "port in use" or "address
  already in use", a previous daemon didn't die cleanly. Hunt the PIDs and
  `kill` them. For ETH, `sudo pkill -9 geth` is the documented escape hatch.

- **Kill a harness from outside its tmux session**:

  ```bash
  tmux kill-session -t btc-harness
  ```