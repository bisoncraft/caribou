# Electrum Harness Integration Tests

These are **opt-in** integration tests that run the lean Electrum wallets
(`electrumWallet` for BTC, `ltcElectrumWallet` for LTC) against a live local
harness. They are **not CI** — they require locally running infrastructure
you start manually.

## Prerequisites

- Go toolchain installed.
- `tmux` installed (the harness uses it).
- `electrumx` installed in a Python venv (the `electrumx.sh` scripts handle
  this the first time).
- `gencerts` from the Decred toolchain (`go install
  github.com/decred/dcrd/cmd/gencerts@release-v1.7`) for the TLS cert that
  electrumx serves; the wallets use `InsecureSkipVerify` so the self-signed
  cert is acceptable.

## BTC

### 1. Start the BTC simnet harness

```sh
cd util/testing/btc
./harness.sh
```

This starts a `btc-harness` tmux session with:

- Window 0 — `bitcoind` alpha node (RPC port 20556)
- Window 1 — `bitcoind` beta node (RPC port 20557)
- Window 2 — harness-ctl (mine-alpha, mine-beta scripts)
- Window 3 — auto-miner (`watch -n 15 ./mine-alpha 1`)

### 2. Start the BTC electrumx server

In a separate terminal (or tmux window):

```sh
cd util/testing/btc
./electrumx.sh
```

The server listens on `ssl://localhost:54002` (the built-in BTC simnet
server address in `electrum.go:btcElectrumServers`).

### 3. Run the BTC electrum harness tests

```sh
go test -tags harness ./wallet/asset/btc/ -run TestElectrum -v
```

Available tests:

| Test | What it checks |
| ---- | -------------- |
| `TestElectrumConnectSyncStatus` | Connect + poll `SyncStatus()` until `Synced` |
| `TestElectrumDepositAddressOwnsValidate` | `DepositAddress` / `OwnsDepositAddress` / `ValidateAddress` |
| `TestElectrumBalanceAfterFunding` | Fund from alpha; assert `Balance.Available` increases |
| `TestElectrumSendSelfPendingToConfirmed` | `Send` self; check `PendingTransactions`; mine; check confirmed in `WalletTransaction` + `TxHistory` |
| `TestElectrumReceiveDetection` | Alpha sends; mine; receive in `TxHistory` + balance |
| `TestElectrumStandardSendFee` | `StandardSendFee` returns positive, monotonic value |
| `TestElectrumInfo` | `Info()` returns Bitcoin `WalletInfo` |

---

## LTC

### 1. Start the LTC simnet harness

```sh
cd util/testing/ltc
./harness.sh
```

This starts an `ltc-harness` tmux session with:

- Window 0 — `litecoind` alpha node (RPC port 20566)
- Window 1 — `litecoind` beta node (RPC port 20567)
- Window 2 — harness-ctl (mine-alpha, mine-beta scripts)
- Window 3 — auto-miner

### 2. Start the LTC electrumx server

In a separate terminal:

```sh
cd util/testing/ltc
./electrumx.sh
```

The script delegates to `btc/electrumx.sh Litecoin 20566 55002`. The server
listens on `ssl://localhost:55002` (the built-in LTC simnet server address in
`electrum.go:ltcElectrumServers`).

### 3. Run the LTC electrum harness tests

```sh
go test -tags harness ./wallet/asset/ltc/ -run TestLTCElectrum -v
```

Available tests:

| Test | What it checks |
| ---- | -------------- |
| `TestLTCElectrumConnectSyncStatus` | Connect + poll `SyncStatus()` until `Synced` |
| `TestLTCElectrumDepositAddressOwnsValidate` | `DepositAddress` / `OwnsDepositAddress` / `ValidateAddress` |
| `TestLTCElectrumBalanceAfterFunding` | Fund from alpha; assert balance increases |
| `TestLTCElectrumSendSelfPendingToConfirmed` | Self-send; pending → confirmed flow |
| `TestLTCElectrumReceiveDetection` | Receive from alpha; `TxHistory` + balance |
| `TestLTCElectrumStandardSendFee` | `StandardSendFee` positive and monotonic |
| `TestLTCElectrumInfo` | `Info()` returns Litecoin `WalletInfo` |

### MWEB note

These tests use **standard transparent addresses** (P2WPKH / P2PKH) only.
MWEB extension-block outputs are stripped by the bisonwire MWEB-aware decoder
when transactions arrive as raw hex from the electrumx server. Sends to or
receives from MWEB outputs may be invisible. Full MWEB-through-electrum
behavior is a known follow-up item and is not exercised here.
