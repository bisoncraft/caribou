# Migrating wallet/db to lexi/glossi

Plan for replacing the bespoke bbolt backend at `wallet/db/bolt` with the
build-tag-selected storage engines already used by `wallet/txdb` — Badger
(`util/lexi`) on desktop, bbolt (`util/glossi`) on mobile — and for removing the
database upgrade machinery and legacy-format compatibility inherited from the
dcrdex fork.

## Motivation

`wallet/db/bolt` is a 1,035-line bbolt integration that predates the fork. It
duplicates, in a bespoke and engine-coupled way, what `wallet/txdb` already does
generically. Migrating it:

- Deletes the last hand-rolled storage layer in `wallet/core`'s dependency graph.
- Puts core's data behind the same engine abstraction as the transaction DB, so
  there is one storage story per platform rather than two.
- Replaces an O(all notifications) scan-and-sort in `NotificationsN` with a
  secondary index.

The project is pre-release and simnet-only. No database on disk anywhere was
created by an older release of this product, so every upgrade path and every
legacy-format decoder in the tree is unreachable code. Removing them alongside
the migration is cheaper than porting them, and this is the last point at which
it is free.

## Current state

`wallet/db/interface.go` defines `DB`; `wallet/db/bolt` is the only
implementation and `wallet/core/core.go` is its only construction site.
`bolt.NewDB` already returns the interface type, so `core` is interface-typed at
the seam and the port is contained.

On-disk today: `<appdata>/<net>/bossw.db`, a single bbolt file, from
`app.ResolveConfig` via the `--db` flag.

Of the three frontend bundles, only two are live: `embed_desktop.go` serves
`newui` and `embed_mobile.go` serves `mobile`. `wallet/appserver/site` is served
by no build and is not one of the `shared`/`newui`/`mobile` npm workspaces, so
it is left untouched throughout. `shared` is live — both live bundles import it.

`wallet/db/types.go` encodes every record as a versioned `encode.DecodeBlob`
blob with per-version decoders. The blob envelope is engine-agnostic and carries
over unchanged; only the key/bucket layout is bbolt-specific. The `_v0` decoders
inside it do not carry over — see below.

## Engine constraints

Established by reading `util/lexi` and `util/glossi`:

| Capability | lexi (Badger) | glossi (bbolt) |
| --- | --- | --- |
| Path argument | directory, **suffixed `_v4`** | file |
| Multi-key atomic write, one table | `SetBatch` or `WithTxn` | `SetBatch` |
| Multi-table atomic write | `WithTxn` | **not expressible** |
| Backup | promoted `badger.Backup`/`Load` | `View` + `tx.WriteTo` |
| Compaction | none (`Flatten` is the analogue) | unreachable (`bdb` unexported) |

Two consequences drive the design:

1. **No cross-table transaction exists behind a shared interface.** `txdb.Store`
   omits transactions for exactly this reason; it is the honest intersection of
   the two engines. Any operation spanning two tables cannot be made atomic.
2. **`lexi.New` does not open the path it is given.** `NeedsV1toV4Update`
   appends `_v4` and, if anything exists at the original path, treats it as a
   Badger v1 database to upgrade. Handed a bbolt file, that upgrade fails and
   `New` swallows the error with `Warnf("Unable to update old db, creating new
   one")`, silently starting an empty database. Phase 1 deletes this outright.

## Decisions

- **No data migration.** Any existing `bossw.db` is disposable.
- **Remove all upgrade machinery and legacy-format compatibility.** Inventoried
  below. This is not opportunistic cleanup: `lexi`'s v1-to-v4 path is what makes
  `lexi.New` hazardous, and the `_v0` decoders and `creds.Version` would
  otherwise have to be carried into the new schema.
- **Drop `Backup` / `BackupTo` / `Core.BackupDB`**, along with
  `Opts.BackupOnShutdown` and the `--no-db-backup` flag. These are the only
  bbolt-specific methods on the interface, they have no analogue in either
  engine, and `Core.BackupDB` has zero callers. The compact-on-shutdown logic in
  `bolt.Run` is meaningless against Badger's LSM.
- **Drop `Recrypt`** and its only callers, `Core.upgradeV0CredsToV1` and
  `Core.initializePrimaryCredentials`. `Recrypt` is the sole operation requiring
  cross-table atomicity — the one thing glossi cannot express. Both callers are
  reachable only for `creds.Version == 0` or an empty `EncInnerKey`, and
  `generateCredentials` unconditionally writes `Version: 1` with a populated
  `EncInnerKey` (which `validateCreds` also enforces on every write), so neither
  path can execute against a database this codebase created. Removing them
  retires the blocker rather than working around it.
- **Keep `SavePokes` / `LoadPokes`** as-is.
- **One format version per record, numbered 0.** Once the unreachable `_v0`
  decoders are gone, each record type's surviving decoder is renamed `_v0` and
  its `Encode` writes version 0, so the only format on disk is version 0. This
  affects `wallet/db`'s `Wallet` and `Notification`, which both currently
  dispatch v0/v1 and encode as v1. `Balance` already satisfies this.
- **Delete `util/order` outright.** The package has no production importers —
  only `util/order/test`, `wallet/core/dex_stubs_test.go` and
  `wallet/core/helpers_test.go`. It is 2,603 lines of dcrdex order
  serialization, and deleting it retires far more fork-era code than
  renumbering its decoders would.
- **Re-key `seedInnerKey`'s domain constant** from
  `BLAKE-256("DCRDEX-InnerKey-v0")` to `BLAKE-256("Caribou-InnerKey-v0")`. The
  existing comment's derivation was verified to reproduce the current constant
  exactly. The stated reason for freezing it — backwards compatibility with
  existing encrypted databases — does not apply to this product, and this is the
  last point at which the value can change for free. It invalidates every
  database on disk, which is already the case.
- **Drop `wallet/app/config.go`'s `Experimental` flag**, which is marked
  deprecated and has no reader anywhere.
- **Share the engine abstraction via `util/kvstore`.** The `Store` interface and
  its adapters move out of `wallet/txdb` into a neutral package that both
  databases import, rather than `wallet/db` importing `wallet/txdb` for them.
- **Separate store instances** for `wallet/db` and `wallet/txdb`. Measured
  marginal cost of a second Badger instance is ~1 MB RSS (~2.4 GB of virtual
  address space, untouched); memory is not an argument for consolidation, and
  separate instances preserve the two databases' independent lifecycles.

## Upgrade and legacy baggage inventory

Everything below is verified unreachable for a database created by this
codebase. Grouped by the phase that removes it.

### Engine layer (Phase 1)

| Item | Location | Status |
| --- | --- | --- |
| `BadgerV1Update`, `NeedsV1toV4Update`, `version4Suffix` | `util/lexi/lexi.go` | v1-to-v4 migration; the `_v4` suffix exists only to serve it |
| `New`'s migration branch | `util/lexi/lexi.go` | contains the silent-fresh-database fallback |
| `DB.Upgrade`, `upgradeTxn` field + its fork in `DB.Update` | `util/lexi/lexi.go` | zero non-test callers |
| `DB.ReIndex`, `DB.DeleteIndex` | `util/lexi/index.go` | zero non-test callers |
| `existingPrefixForName`, `prefixForNameImpl`'s `existing` param | `util/lexi/lexi.go` | orphaned by the two above |
| `GetDBVersion`, `SetDBVersion`, `versionPrefix` | `util/lexi/lexi.go`, `keyprefix.go` | live only via politeia's version block |
| proposals `dbVersion` const + version check | `util/politeia/politeia.go` | its `DropAll` arm is unreachable (`dbVersion = 1`) |
| `v1badger` import + `Logger` assertion | `util/lexi/log.go` | assertion is already redundant with the v4 one |
| `wallet/asset/kvdb` | whole package | zero importers; open-codes the v1-to-v4 dance |

`util/glossi` has no upgrade, versioning, or path-mangling machinery at all;
it needs no changes. Its `datumVersion` is a wire-format guard, not migration.

### Record formats and credentials (Phase 3)

| Item | Location | Status |
| --- | --- | --- |
| `decodeWallet_v0` + `case 0` | `wallet/db/types.go` | synthesizes an empty wallet `Type`; only writer is `Encode`, hard-coded v1. `_v1` is then renamed `_v0` and `Encode` writes 0 |
| `decodeNotification_v0` + `case 0` | `wallet/db/types.go` | synthesizes an empty `TopicID`; only writer is `Encode`, hard-coded v1. `_v1` is then renamed `_v0` and `Encode` writes 0 |
| `util/order` | whole package (2,603 lines) | dcrdex order serialization; zero production importers |
| `Match`, `Order`, `OrderReader`, `StatusString` stubs | `wallet/core/dex_stubs_test.go` | stubs "for the removed DEX order type", used only by `helpers_test.go` |
| `TestOrderReader_StatusString` | `wallet/core/helpers_test.go` | tests only the stub above; the whole file goes |
| `Experimental` flag | `wallet/app/config.go` | marked deprecated, no reader |
| `NotificationsN` mismatched-key self-heal | `wallet/db/bolt/db.go` | twin of `decodeNotification_v0`; its removal lets `NotificationsN` drop to a read-only view |
| `PrimaryCredentials.Version` | `wallet/db/types.go` | nothing writes anything but 1; sole reader is the `case 0` upgrade switch |
| `credsVersionKey` + its default-to-zero read | `wallet/db/bolt/db.go` | dies with the field |
| `Login`'s `len(creds.EncInnerKey) == 0` branch | `wallet/core/core.go` | `validateCreds` rejects empty on every write |
| `creds()`'s empty-`EncInnerKey` nil return | `wallet/core/core.go` | same unreachable condition |
| `TopicUpgradedToSeed` + 6 locale entries | `wallet/core/notification.go`, `locale_ntfn.go` | sole emitter is `initializePrimaryCredentials` |
| `LegacyWalletIndex` | `wallet/asset/interface.go`, `driver.go` | no driver sets it; always 0. Consumed only for empty `walletType`, which only `decodeWallet_v0` produces |
| `emptyidx` | `appserver/shared/src/registry.ts` | frontend twin of `LegacyWalletIndex`; `shared` is a live npm workspace that newui imports |
| `legacySeedLength`, `ExportSeed`'s hex branch, `decodeSeedString`'s hex fallback | `wallet/core/core.go` | 64-byte hex seeds are a dcrdex-era restore format; `wallet/mnemonic` has no legacy path |
| — | `appserver/newui` | no frontend change needed: newui has no 128-char seed sniff and never reads `emptyidx` |

Two adjacent defects to fix rather than delete, both inherited:

- `decodeBalance_v0` validates `len(pushes) >= 2` and an even push count, a
  leftover from when dcrdex's `Balance` encoded multiple pairs. `Encode` writes
  exactly 2. Should be `!= 2`.
- `decodeNotification_v1`'s error strings name `decodeNotification_v0`, and one
  reports `len(pushes[2])` while checking `pushes[3]`.

## Target design

### Package layout

Extract the engine abstraction out of `txdb` so both databases share it:

```text
util/kvstore/            (moved from wallet/txdb)
  store.go               Store, StoreTable, StoreIndex, StoreIter, IndexFunc,
                         BatchEntry, sentinel errors
  adapter_lexi.go        //go:build !mobile
  adapter_glossi.go      //go:build mobile
  engine_desktop.go      //go:build !mobile   -> New = newLexiStore
  engine_mobile.go       //go:build mobile    -> New = newGlossiStore
```

`wallet/txdb` keeps the moved names as aliases. This is required rather than
cosmetic: 28 references across `wallet/asset/{btc,eth,hyperliquid,sol,xrp}` use
`txdb.StoreTable`, `txdb.IndexFunc`, `txdb.BatchEntry` and the sentinels for
wallet-private aux tables. Aliasing leaves all five asset packages untouched.

`wallet/db` gains a `kvstore`-backed implementation and `wallet/db/bolt` is
deleted.

### Schema

Neither engine has nested buckets, so the three nested-bucket layouts flatten to
composite keys. `Wallet.ID()` is a fixed 4 bytes and `Notification.ID()` is a
fixed-length blake2s digest, so composite keys are unambiguous. Both engines
reject zero-length keys.

**Table `app`** — singleton values, one key each: `creds`, `seedGenTime`,
`lang`, `disabledRateSources`, `companionToken`, `apiToken`,
`sendRequiresPassword`, `pokes`.

`PrimaryCredentials` collapses from six sibling keys into one versioned blob
under `creds`. This makes `SetPrimaryCredentials` a single atomic `Set`,
replacing the six-key `bucketPutter` chain that relied on a bbolt transaction.
Absence of the key maps to `ErrNoCredentials`. The struct's own `Version` field
is gone by this point; the blob envelope carries the format version.

**Table `wallets`** — key `assetID(4) || fieldTag(1)`:

| Tag | Value |
| --- | --- |
| `0x01` | wallet encoding |
| `0x02` | balance encoding |
| `0x03` | disabled flag |

This preserves today's partial-update semantics exactly. `UpdateBalance` — the
hottest write in the interface, fired on every tip change per asset — stays a
single-key `Set` with no read-modify-write and no lock. `UpdateWallet` writes
the wallet/balance pair via one `SetBatch`. `Wallets()` iterates the table
grouping on the 4-byte prefix; `Wallet(wid)` iterates `prefix=wid`.

**Table `notes`** — key `Notification.ID()`, value a versioned blob of the
notification encoding plus its ack flag. A non-unique `stamp` index emits the
8-byte big-endian timestamp, so `NotificationsN(n)` becomes a reverse index
iteration taking the first `n`, replacing `newestBuckets`' full scan and sort.

**Tables `multisigIndexes`** (`assetID(4)` -> counter) and **`multisigPubkeys`**
(`assetID(4) || pubkey(33)` -> index).

Fixed-length index entries are required for glossi unique indexes; the only
index here is non-unique and fixed-width regardless.

### Concurrency

Without transactions, the read-modify-write operations need a `sync.Mutex` on
the DB: `SetWalletPassword`, `AckNotification`, and `NextMultisigKeyIndex`. The
last is the one that matters — a lost update there yields a duplicate multisig
key index, i.e. address reuse. All three are cold, user-driven paths, so a
single coarse mutex is sufficient. Both engines hold an exclusive file lock, so
there is no cross-process case to defend.

### Lifecycle

`db.DB` drops `util.Runner` for `Connect(ctx) (*sync.WaitGroup, error)` +
`Close() error`, matching `txdb` and both engines' native shape. `Core.Run`'s
hand-rolled goroutine and `sync.WaitGroup` collapse into a `Connect` call.

`stopDB()` moves from its current position — before the wallets are locked and
disconnected — down beside `stopTxDB()`. It sits early today only because bolt
needed a quiet moment to back up and compact, which is going away. This also
fixes a latent bug: `notify` -> `SaveNotification` can fire from the wallet
lock/disconnect path after the DB is closed, and its error is discarded in
`notification.go`, so the write is lost silently. The stale "The DB starts first
and stops last" comment becomes true again.

### Path

With `_v4` gone (Phase 1), `lexi.New` opens the path it is given. The default DB
path changes from `<net>/bossw.db` to `<net>/coredb`, a directory on desktop and
a file on mobile — the same split `tx.db` already has. A fresh name avoids any
interaction with the stale bbolt file, which is ignored and can be deleted by
hand. `filepath.Dir(cfg.DBPath)` still parents `assetdb/` and `tx.db`, so those
are unaffected.

Phase 1 also changes two existing artifacts: `<net>/tx.db_v4/` becomes
`<net>/tx.db/`, and `<walletDir>/politeia_v4/` becomes `<walletDir>/politeia/`.
Both are disposable.

## Phased plan

Five phases, each a self-contained PR that builds and passes tests under both
build tags. The ordering is deliberate: the upgrade machinery goes before the
abstraction is extracted (`wallet/asset/kvdb` cannot survive Phase 1 and is the
package being extracted around), the dead code goes before it has to be ported,
and the test suite is made backend-agnostic and proven against the incumbent
before a second backend exists to check it against.

Every phase ends with `gofmt` applied and `golangci-lint` clean. Phases touching
`.ts` also run `npx eslint` on the touched files.

### Phase 1 — Strip lexi's upgrade machinery — done (d76af68e9)

Self-contained in `util/lexi`, `util/politeia`, and go.mod.

- Delete `BadgerV1Update`, `NeedsV1toV4Update`, `version4Suffix`, and `New`'s
  migration branch. `New` reduces to `badger.Open(badger.DefaultOptions(cfg.Path))`.
  The `os` and `strings` imports become unused; the `New` doc comment describing
  the `_v4` suffix must be rewritten.
- Delete `DB.Upgrade`, the `upgradeTxn` field, and its fork in `DB.Update`, which
  collapses to the `ErrConflict` retry loop. Delete `DB.ReIndex` and
  `DB.DeleteIndex`, then the orphaned `existingPrefixForName` and
  `prefixForNameImpl`'s `existing` parameter.
- Delete `GetDBVersion`, `SetDBVersion`, `versionPrefix`, and the `dbVersion`
  const plus version-check block in `util/politeia/politeia.go` that is their
  only non-test caller.
- Delete `wallet/asset/kvdb` (zero importers, and the only non-lexi caller of
  the two removed migration functions — it cannot compile without them).
- Drop `v1badger` from `util/lexi/log.go` (import + the redundant `Logger`
  assertion) and `util/lexi/lexi.go`.
- `go mod tidy`: drops `github.com/dgraph-io/badger v1.6.2` (direct) and four
  modules reachable only through it — `AndreasBriese/bbloom`,
  `dgraph-io/ristretto v0.0.2`, `cespare/xxhash v1.1.0` and
  `golang/protobuf` — plus 13 go.sum lines. `ristretto/v2` stays; it is badger
  v4's cache. The nested `wallet/cmd/caribou-desktop` module has its own
  `go.mod` with a `replace` to the root, which a root-level tidy does not
  cover; it is tidied separately and sheds the same five modules.
- Delete the ~16 upgrade tests across `util/lexi/db_test.go` and `more_test.go`
  (`TestNeedsV1toV4Update`, `TestBadgerV1Update`, `TestBadgerV1UpdateWithLargeData`,
  `TestNewWithMigration`, `TestDBVersion`, `TestUpgradeTransaction`,
  `TestDeleteIndex`, `TestReIndex*`, `TestDeleteIndex*`), then prune the imports
  they orphan (`os`, `v1badger`, possibly `bytes`/`encode`).

This deletes the silent-fresh-database fallback along with the branch that
contains it, so the `lexi.New` hazard is retired here rather than worked around
later.

On-disk: `<net>/tx.db_v4/` becomes `<net>/tx.db/`, `<walletDir>/politeia_v4/`
becomes `<walletDir>/politeia/`. No test asserts on either path.

Verify: `go build ./... && go build -tags mobile ./...`;
`go test ./util/lexi/... ./util/glossi/... ./wallet/txdb/...` under both tags;
`grep -rn "dgraph-io/badger\"" .` clean.

### Phase 2 — Extract the engine abstraction to `util/kvstore` — done (c4f31a121)

Pure code motion. No behavior change.

- Move `wallet/txdb/engine.go` to `util/kvstore/store.go`: `Store`,
  `StoreTable`, `StoreIndex`, `StoreIter`, `IndexFunc`, `BatchEntry`, and the
  `ErrKeyNotFound` / `ErrEndIteration` / `ErrNotIndexed` sentinels.
- Move `adapter_lexi.go`, `adapter_glossi.go`, `engine_desktop.go` and
  `engine_mobile.go` with their build tags intact. The unexported `newStore`
  becomes the exported `kvstore.New`.
- Alias the moved names in `wallet/txdb` so its dependents do not move:

  ```go
  type (
      Store      = kvstore.Store
      StoreTable = kvstore.StoreTable
      StoreIndex = kvstore.StoreIndex
      StoreIter  = kvstore.StoreIter
      IndexFunc  = kvstore.IndexFunc
      BatchEntry = kvstore.BatchEntry
  )

  const (
      ErrKeyNotFound  = kvstore.ErrKeyNotFound
      ErrEndIteration = kvstore.ErrEndIteration
      ErrNotIndexed   = kvstore.ErrNotIndexed
  )
  ```

Two doc comments cite the moved files as precedent and are repointed at
`util/kvstore`: `txdb.go`'s package comment, and the build-tag note in
`wallet/appserver/embed_desktop.go` / `embed_mobile.go`.

Success criterion: `wallet/txdb`'s existing tests pass **unmodified**, and the
five asset packages are not touched.

Verify: `go build ./... && go build -tags mobile ./...`;
`go test ./wallet/txdb/... ./util/kvstore/...` under both tags.

### Phase 3 — Remove the dead and legacy surface, still on bolt — done (67d984534)

Deleting first keeps Phase 5's diff to the migration, and stops the `_v0`
decoders and `creds.Version` from being carried into the new schema. Follow the
dependency order below; each step orphans the next.

1. `wallet/core/core.go`: the `switch creds.Version` / `case 0` arm and
   `upgradeV0CredsToV1`.
2. `PrimaryCredentials.Version` (`wallet/db/types.go`), `credsVersionKey` and its
   default-to-zero read (`wallet/db/bolt/db.go`), and the `Version:` writes in
   `generateCredentials`, `changeAppPass`, and `dbtest.RandomPrimaryCredentials`.
3. `Login`'s `len(creds.EncInnerKey) == 0` branch, `initializePrimaryCredentials`,
   and `creds()`'s empty-`EncInnerKey` nil return.
4. `TopicUpgradedToSeed` and its 6 locale entries (en-US, pt-BR, zh-CN, pl-PL,
   de-DE, ar).
5. `decodeWallet_v0` and its `case 0`, then the orphaned `LegacyWalletIndex`
   (`wallet/asset/interface.go`, `driver.go`) and its frontend twin `emptyidx`
   (`appserver/shared/src/registry.ts`). Rename `decodeWallet_v1` to
   `decodeWallet_v0`, collapse the dispatch to `case 0`, and change
   `(*Wallet).Encode` to `versionedBytes(0)`.
6. `decodeNotification_v0` and its `case 0`, then the `NotificationsN`
   mismatched-key self-heal; `NotificationsN` drops from `notesUpdate` to a
   read-only view since the `Put` was its only reason to take a write txn.
   Rename `decodeNotification_v1` to `decodeNotification_v0`, collapse the
   dispatch to `case 0`, and change `(*Notification).Encode` to
   `versionedBytes(0)`.
7. `legacySeedLength`, `ExportSeed`'s hex branch, and `decodeSeedString`'s hex
   fallback, which collapses to `mnemonic.DecodeMnemonic`. No frontend change:
   newui has no legacy-seed sniff.
8. `Backup`, `BackupTo`, `backup`, `compact`, `ovrFlag`, `fileSize`, `backupDir`,
   `Opts`/`BackupOnShutdown` and `Recrypt` from `wallet/db`; `BackupDB`, the
   `bolt.Opts` literal, `Config.NoAutoDBBackup` and `recrypt` from
   `wallet/core`; `--no-db-backup` from `wallet/app/config.go`. `bolt.Run`
   collapses to `<-ctx.Done()` followed by `Close`.
9. Delete `util/order` (all of `match.go`, `order.go`, `serialize.go`,
   `status.go`, their tests, and `util/order/test`). Then, in dependency order:
   - Delete `wallet/core/helpers_test.go` outright — its sole test targets
     `OrderReader.StatusString`, a stub defined in `dex_stubs_test.go`.
   - Trim `wallet/core/dex_stubs_test.go`: drop the `util/order` import and the
     `Match`, `Order`, `OrderReader` and `StatusString` stubs. **Keep** the rest
     — `tDriver` and `tCreator` are used by `core_test.go`. (`pokesCache` and
     the asset-54321 `init` appear to have no users outside this file; confirm
     and remove if so.)
   - Replace `ordertest.RandomAddress()` in `wallet/db/test/dbtest.go` with a
     local base58 `randomAddress()` helper. That is the only symbol `dbtest`
     imports from `util/order/test`.

   This step is independent of the rest of the phase.
10. Drop the deprecated `Experimental` flag from `wallet/app/config.go`.
11. `seedInnerKey` (`wallet/core/core.go`): replace `keyParam` with
    `BLAKE-256("Caribou-InnerKey-v0")` and rewrite the comment to state the new
    derivation without the backwards-compatibility claim:

    ```go
    // It is equal to BLAKE-256([]byte("Caribou-InnerKey-v0")).
    keyParam := [32]byte{
        0x26, 0x85, 0x02, 0x2d, 0x65, 0x70, 0x98, 0x43,
        0x24, 0x6f, 0x99, 0xbc, 0xa2, 0x36, 0x07, 0xfe,
        0xb9, 0x98, 0x73, 0x46, 0x91, 0x25, 0xe2, 0xab,
        0xa9, 0xa1, 0xce, 0xbf, 0x50, 0x8c, 0xfb, 0x41,
    }
    ```

12. Fix the two inherited defects: `decodeBalance_v0`'s push check becomes
    `!= 2`, and `decodeNotification_v0`'s error strings are corrected (they name
    the wrong function and one reports `len(pushes[2])` while checking
    `pushes[3]`).
13. Tests: delete `TestBackup`, `TestBackupTo`, `TestRecrypt` and
    `testCredentialsUpdate`; drop `TDB.BackupTo` from `core_test.go`.

Two user-visible behavior changes fall out of this phase, both belonging in
release notes alongside `--no-db-backup`:

- Removing `LegacyWalletIndex` means a wallet-creation request that omits
  `"type"` now errors instead of resolving to `AvailableWallets[0]`. Nothing
  in-tree relied on the fallback: no driver ever set the index, newui always
  sends a concrete type, and `createDBWallet` normalizes the stored type to
  `walletDef.Type` — so `dbWallet.Type`, and every `xcWallet.walletType` read
  from it, is always populated. Only an external API client omitting the field
  is affected.
- `decodeSeedString` losing its fallback means a bad mnemonic surfaces the real
  checksum/word error rather than the catch-all `"unabled to decode provided
  seed"`.

`--no-db-backup` is user-facing: an INI file setting it will fail to parse after
this. Acceptable pre-release, but it belongs in release notes.

Verify: both build tags; `go test ./wallet/...`; `npx eslint` on the touched
`.ts` plus `npm run build` in the `newui` and `mobile` workspaces, since both
consume `shared`; `BackupDB`, `Recrypt`, `no-db-backup`, `legacySeedLength` and
`LegacyWalletIndex` grep clean. `emptyidx` deliberately survives in
`wallet/appserver/site`, which keeps its own copy of the declaration.

### Phase 4 — Backend-agnostic conformance suite, still on bolt — done (d32b4a7b8)

Build the equivalence harness while bolt is still the only backend, so the suite
is validated against known-good behavior before it is used to judge a new one.

- Add `wallet/db/test/suite.go` exporting
  `RunSuite(t *testing.T, newDB func(*testing.T) db.DB)`, lifting the
  table-driven bodies out of `wallet/db/bolt/db_test.go` and extending coverage
  to the rest of the interface: credentials, wallets, wallet status,
  notifications, pokes, multisig, seed-generation time, disabled rate sources,
  language, tokens, and send-requires-password. The suite pins the contracts a
  new backend must honor for `core`: the `ErrNoCredentials` and
  `ErrNoSeedGenTime` sentinels that `core` matches with `errors.Is`, the
  partial-update semantics of `UpdateBalance` / `SetWalletPassword` /
  `UpdateWalletStatus`, credential overwrite (`changeAppPass`), empty-token and
  cleared-token reads, per-asset `NextMultisigKeyIndex` sequencing, and
  newest-first `NotificationsN` ordering with `Ack`/`Id` population.
- Reduce `wallet/db/bolt/db_test.go` to a driver that constructs a `*BoltDB`
  and calls `RunSuite`. No existing test reached into bbolt internals, so
  nothing stays behind.
- `dbtest.go`'s generators and comparators are reused; `RandomWallet` and
  `MustCompareWallets` additionally gain `Wallet.Type` coverage, which no test
  previously exercised.

Verify: `go test ./wallet/db/...` green — the lifted assertions, now routed
through the interface.

### Phase 5 — The port — pending

- Add `wallet/db/db.go` implementing `db.DB` over `kvstore.Store` with the
  schema above. Exported `db.New(path, log)` builds its own store; unexported
  `newDB(store, log)` accepts one for tests, mirroring `txdb.newTxDB`. The
  implementation lands in package `db` beside the interface — with bolt gone
  there is only one backend, and the interface is retained for `core_test.go`'s
  `TDB` stub.
- `wallet/db/interface.go`: swap `util.Runner` for
  `Connect(ctx) (*sync.WaitGroup, error)` and `Close() error`.
- `wallet/core/core.go`: construct via `db.New`; replace the hand-rolled DB
  goroutine and `sync.WaitGroup` with `Connect`; move `stopDB()` /
  `dbWG.Wait()` down beside `stopTxDB()`; correct the stale "The DB starts first
  and stops last" comment.
- `wallet/core/core_test.go`: `TDB.Run` becomes `TDB.Connect` / `TDB.Close`.
- `wallet/app/config.go`: default DB path `bossw.db` becomes `coredb`.
- Delete `wallet/db/bolt`.
- Add `wallet/db/db_lexi_test.go` (`//go:build !mobile`) and
  `wallet/db/db_glossi_test.go` (`//go:build mobile`), each a driver calling
  `dbtest.RunSuite`. This is the first time both engines get identical coverage;
  `txdb` only ever tests the build-selected one.

Verify: `go test ./wallet/...` and `go test -tags mobile ./wallet/db/...` — the
Phase 4 suite now passes against both engines. `go build -tags mobile ./...`.
Confirm the artifact is `<net>/coredb` — a directory on desktop, a file on
mobile — and that `grep -rn bbolt wallet/db` is empty.

## Notes

- Phase 3 is the widest phase: it spans `wallet/db`, `wallet/core`,
  `wallet/asset`, `wallet/app`, `util/order` and the frontend. Its steps are
  ordered so each removal orphans the next, and steps 9 and 10 are independent
  of the rest and can be reviewed separately if the diff gets unwieldy.
- The `Phase 3` deletions are what make the port tractable: without them, the
  new schema would have to carry `creds.Version`, two dead `_v0` decoders, and a
  `Recrypt` that no shared engine interface can implement atomically.
