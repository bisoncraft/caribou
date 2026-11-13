# zeclib

zeclib is a Zcash light wallet backend for Go. It keeps a wallet database in
sync with the chain through a lightwalletd server and reports accounts,
addresses, balances and sync progress. The wallet logic is
[librustzcash](https://github.com/zcash/librustzcash), reached through
generated bindings.

Sending funds and an `asset.Wallet` implementation are not part of the
package yet. Both are planned to build on it.

Ironwood, the pool that NU6.3 adds, is the only shielded pool the wallet
supports. An account holds funds in the Ironwood pool and in transparent
outputs. The Sapling and Orchard pools are not supported: no account begins
before NU6.3, and there is no migration of funds out of the older pools.

## Usage

```go
w, err := zeclib.Open(&zeclib.Config{
    DBPath:  filepath.Join(dir, "wallet.sqlite"),
    Network: zeclib.Mainnet,
    Server:  "zec.rocks:443",
}, nil)
if err != nil {
    return err
}
// Close stops syncing if it is still running, waits for it, and releases
// the database.
defer w.Close()

// A new wallet needs an account.
acct, err := w.CreateAccount(ctx, &zeclib.AccountParams{Name: "main", Seed: seed})
if err != nil {
    return err
}

// Syncing runs in the background until ctx is cancelled or the wallet is
// closed.
wg, err := w.Connect(ctx)
if err != nil {
    return err
}

addr, err := w.Address(acct.ID)
if err != nil {
    return err
}
fmt.Println("receive at", addr)

// The WaitGroup is done when syncing has stopped.
<-ctx.Done()
wg.Wait()
```

| Call | Purpose |
| ---- | ------- |
| `Open`, `Close` | Open or create the wallet database, and release it. |
| `CreateAccount`, `CreateAccountWithTreeState` | Derive an account from a seed. The second takes the birthday tree state from the caller instead of the server. |
| `Connect` | Start syncing. Returns a `*sync.WaitGroup` that is done when syncing has stopped. |
| `SyncStatus`, `Config.OnSyncStatus` | Connection state, scan progress and the last error. |
| `Summary` | Balances per account and scan progress, read from the database. |
| `Address`, `NewAddress`, `TransparentAddresses` | Unified and transparent addresses of an account. |

The Go documentation of each call has the details.

## Design

The package follows the structure of the Zcash Android wallet SDK: a Rust
backend holds the wallet database and the host language does everything
else.

| Part | Location | Role |
| ---- | -------- | ---- |
| Rust backend | `rust/` | Wraps `zcash_client_sqlite` and `zcash_client_backend` in a small set of synchronous calls: open, create account, store subtree roots, update chain tip, suggest scan ranges, scan a batch of blocks, rewind, summarize. No networking and no threads. |
| Bindings | `internal/ffi/` | Go bindings generated from the compiled crate by uniffi-bindgen-go. |
| Protocol | `walletrpc/` | The lightwalletd gRPC client, generated from the `.proto` files of [lightwallet-protocol](https://github.com/zcash/lightwallet-protocol). |
| Wallet | this directory | The public API. Talks to the server, decides what to download, hands blocks to the backend, recovers from reorganizations. |

Because the Rust side only answers short calls, everything long-running is a
goroutine in Go. `Wallet.Connect` takes a `context.Context` and returns a
`*sync.WaitGroup`, as a `util.Connector` does. Cancelling the context stops
syncing. The WaitGroup is done when no goroutine of the wallet is running and
the server connection is closed, and the wallet can then be connected again.
No cancellation crosses into Rust: a stop waits at most for the scan of one
batch of blocks.

Blocks and tree states cross the FFI boundary as protobuf-encoded bytes.
Nothing is stored on disk besides the wallet database.

### Sync

One sync cycle, in `sync.go`, follows the flow that `zcash_client_backend`
documents:

1. On the first cycle of a connection, check that the server is on the
   wallet's network, and store the roots of completed Ironwood subtrees that
   it reports. The roots let the wallet scan recent blocks first. A server
   without them is used anyway, and the wallet scans in order.
2. Fetch the chain tip. If the wallet's block at the tip height differs from
   the server's, rewind.
3. Tell the wallet the tip, and record the unspent transparent outputs of its
   addresses. An account's outputs are queried only when the tip or the
   account's addresses have changed since the last query.
4. Ask the wallet which block range it most wants scanned, download the start
   of it, scan it, and ask again until it wants nothing. A batch that does not
   connect to the blocks already scanned is a reorganization: rewind and start
   the cycle over.
5. Answer the wallet's requests for transaction data: the status of
   transactions it has not seen mined, full transactions for their memos and
   transparent parts, and the history of its transparent addresses. A request
   that fails on its own account, such as a transaction the server rejects or
   the wallet cannot parse, is retried later with a growing delay and does not
   fail the cycle.

A synced wallet repeats the cycle at a randomized interval. A failed cycle is
retried with exponential backoff, and `Wallet.SyncStatus` carries the error.

### Behavior worth knowing

- **Ironwood is the only shielded pool.** This affects four things.
  - Addresses: a unified address has an Orchard receiver, which receives into
    the Ironwood pool from NU6.3 on, and optionally a transparent receiver. No
    address has a Sapling receiver. An account's viewing key still contains a
    Sapling key, because librustzcash derives every key of an account.
  - Birthdays: no account begins before the NU6.3 activation height, which is
    3,428,143 on mainnet and 4,134,000 on testnet. `CreateAccount` raises an
    earlier birthday to that height, and fails on a chain that has not reached
    it. A regtest network has to activate NU6.3.
  - Balances: `AccountBalance` has an Ironwood and a transparent balance only.
  - Subtree roots: only Ironwood roots are fetched. librustzcash first scans
    from the start of the oldest incomplete subtree among the trees it has
    roots for, so roots of the Sapling and Orchard trees would only make that
    first range longer.
- **Blocks are downloaded whole.** The lightwalletd protocol can strip pools
  out of compact blocks (`BlockRange.poolTypes`), but the librustzcash 0.25
  scanner checks every block's Sapling and Orchard tree sizes against the
  outputs it was given, so it cannot scan blocks reduced to Ironwood.
- **Checkpoints decide where a rewind lands.** The wallet can rewind only to a
  height where it kept a checkpoint of its note commitment trees, or to any
  height above the newest checkpoint. A rewind lands on the nearest such
  height at or below the requested one, which on a chain with few shielded
  transactions can be well below it. librustzcash keeps a checkpoint for the
  tree state each scanned batch starts from, for every block that adds a note
  commitment, and, from NU6.3 on, for every height of the ZIP 318 anchor grid,
  including heights inside a batch. Grid heights are multiples of the anchor
  interval before NU7 and of three times the interval from NU7 on. The
  interval is 144 on mainnet and 12 on other networks. The Android SDK aligns
  its scan batches with the grid. This package does not need to.
- **A replaced transaction lingers.** After a reorganization the wallet holds
  a transaction from a replaced block as unmined, and counts its value as
  pending, until the transaction is mined again or its expiry height has
  passed.
- **No summary before the first scan.** `Wallet.Summary` returns nil until the
  wallet has an account and has scanned a block.
- **Address checks see mined transactions only.** For some transparent
  addresses the wallet asks for transactions including the mempool. These are
  its ephemeral addresses, which hold funds while a payment to a TEX address
  (ZIP 320) is under way. The server call used returns only mined
  transactions, so the mempool part of the request goes unanswered. Sending to
  TEX addresses will need it.
- **Reads wait for scans.** The backend holds one database connection, so a
  read waits for the batch being scanned. `Config.BatchSize` bounds the wait.
- **What the server learns.** A lightwalletd server sees the block ranges
  downloaded, the transparent addresses queried (all of an account's in one
  request, repeated only when a block arrives or the account's addresses
  change), the IDs of transactions fetched in
  full, and the birthday when `CreateAccount` fetches the birthday tree state.
  `CreateAccountWithTreeState` takes that tree state from the caller instead.
  `Config.Dialer` routes the connection through a proxy.
- **Seeds are not stored.** Account creation derives viewing keys from the
  seed and keeps only those. The Rust side erases its copy of the seed. The
  temporary copy that the bindings make to pass it is freed without being
  erased.

## Building

The Rust backend is linked statically, and only with the `zec` build tag and
cgo. Without the tag the package still compiles, `Open` returns `ErrNotBuilt`,
and the tests that need no backend still run. Code that imports the package
therefore builds without a Rust toolchain.

Requirements:

- Rust 1.89 or newer.
- [uniffi-bindgen-go](https://github.com/NordSecurity/uniffi-bindgen-go) at
  the version that matches the `uniffi` crate in `rust/Cargo.toml`:

  ```sh
  cargo install uniffi-bindgen-go \
    --git https://github.com/NordSecurity/uniffi-bindgen-go --tag v0.7.1+v0.31.0
  ```

From this directory:

```sh
./build.sh
go build -tags zec ./...
```

`build.sh` builds the crate, copies the static library to
`lib/<os>-<arch>/libzeclib.a`, and regenerates `internal/ffi/ffi.go` and
`internal/ffi/ffi.h`. The generated bindings are tracked and the library is
not. `./build.sh --check` also exits with an error if the bindings on disk
were out of date. `CARGO_BUILD_JOBS` limits build parallelism. The first build
compiles about 300 crates.

Only Linux is supported so far. Another platform needs its link flags in
`internal/ffi/link.go` and its shared-library name in `build.sh`.

The librustzcash crates are pinned to exact versions in `rust/Cargo.toml`
because they are released as a matched set. The `uniffi` crate and
uniffi-bindgen-go are upgraded together.

`walletrpc/gen.sh` regenerates the protocol code. The protocol version has to
be the one the pinned `zcash_client_backend` was built against.

## Testing

From this directory:

```sh
# Rust unit tests.
(cd rust && cargo test)

# Go tests that need no backend: the sync logic against an in-memory backend
# and an in-process lightwalletd.
go test ./...

# The same tests against the Rust backend as well, plus the binding tests.
go test -tags zec ./...

# Integration tests against the simnet harness, which has to be running.
# See util/testing/zec/README.md.
go test -tags zec,harness -run TestHarness .
```

The lifecycle and sync tests in `wallet_test.go` run once per backend: always
against the in-memory fake, and against the Rust backend when it is compiled
in. Both get the same synthetic chain of empty blocks from the fake server in
`fakeserver_test.go`. The chain is valid input for the real wallet because its
note commitment trees are empty at every height.

The harness tests mine blocks, receive funds from the harness's alpha funder,
restore a wallet from its seed, and reorganize the chain by two blocks.
