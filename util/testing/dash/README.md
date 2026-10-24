# DASH Simnet Test Harness

You must have `dashd` and `dash-cli` in `PATH` to use the harness. It is tested
with Dash Core **v23.1.4**. (Dash Core v23+ treats a missing default
configuration file as fatal, so the control scripts pass an explicit `-datadir`;
this is handled for you.)

The harness script creates two connected regtest nodes and their wallets, then
mines some blocks and sends some DASH around. The result is a set of wallets
named **alpha**, **beta**, **gamma**, and **delta**, each with slightly
different properties.

**Beta** is purely a mining node/wallet. Beta has mature coinbase outputs to
spend.

**Alpha** is also a mining node/wallet. Unlike beta, alpha has sent some DASH,
so it has change outputs that are not coinbase and have a varying number of
confirmations.

**Gamma** is a second wallet on the alpha node and **delta** is a second wallet
on the beta node. Both are encrypted, so they require unlocking for sensitive
operations, and both hold a number of UTXOs of varying size and confirmation
count. **The wallet password is "abc".**

Dash uses legacy P2PKH addresses (BIP-44, purpose 44'), so all harness
addresses start with `y` on regtest.

## Ports

| Node  | P2P     | RPC     |
|-------|---------|---------|
| alpha | `20775` | `20777` |
| beta  | `20776` | `20778` |

The nodes run with `-peerblockfilters=1 -blockfilterindex=basic`, so they serve
BIP-157/158 compact block filters to the neutrino-backed SPV wallet, which peers
with alpha at `127.0.0.1:20775`.

## Harness control scripts

The `./harness.sh` script drops you into a tmux window in a directory called
`harness-ctl`. Inside are a number of scripts to perform RPC calls against each
wallet.

- `./alpha`, `./beta`, `./gamma`, and `./delta` are just `dash-cli` configured
  for their respective wallets. Try `./gamma getbalance`, for example.
- `./mine-alpha n` and `./mine-beta n` mine `n` blocks on the respective node.
- `./reorg` steps through a script that causes the alpha node to undergo a
  1-deep reorganization.
- `./new-wallet node name` creates and loads a new wallet on the given node.
- `./mn-register [port]` registers a regular masternode funded from gamma, with
  internal collateral (the ProRegTx creates the 1000 DASH collateral output). It
  prints the proTxHash and the generated operator BLS key. No masternode
  process is run, so the node is only registered on chain. Dash rejects a second
  registration on the same address, so give each one its own port (the default
  is 19999).
- `./quit` shuts down the nodes and closes the tmux session.

## Masternodes

The harness mines past the regtest DIP-3 enforcement height (500) during setup,
so it finishes at about height 520. DIP-3 activates at 432, but until it is
enforced a ProRegTx must use the same key for owner and voting
(`bad-protx-key-not-same`). With ElectrumX running, the registered masternode
is visible through `protx.info <proTxHash>` and `protx.diff <base> <tip>`.

`sendtoaddress` and other coin selection can spend an unlocked collateral
output. If you register with external collateral (`protx register`), lock the
output with `lockunspent false` first.

## Electrum backend

The electrum-backed wallet tests need an electrumx server in addition to the
node harness. With the harness running, start one with:

```sh
./electrumx.sh
```

This delegates to the shared `../btc/electrumx.sh` (upstream spesmilo electrumx,
which ships a `DashRegtest` coin class) and serves `ssl://127.0.0.1:55021`.
electrumx needs the `x11_hash` Python package — because Dash identifies blocks
by their X11 hash rather than double-SHA256 — which `btc/electrumx.sh` installs
automatically when invoked for Dash.

## Integration tests

With the harness up, run the wallet integration tests:

```sh
# electrum backend (also start ./electrumx.sh first)
go test -tags harness ./wallet/asset/dash/ -run TestDashElectrum

# neutrino / SPV backend (no electrumx needed)
go test -tags harness ./wallet/asset/dash/ -run TestDashSPV
```

## Dev stuff

If things aren't looking right, look at the node windows for errors. In tmux,
navigate between windows by typing `Ctrl+b` and then the window number shown in
the status bar at the bottom.
