# Migrating wallet/db to lexi/glossi

Replace the bespoke bbolt backend at `wallet/db/bolt` with the
build-tag-selected storage engines already used by `wallet/txdb` — Badger
(`util/lexi`) on desktop, bbolt (`util/glossi`) on mobile. The project is
pre-release and simnet-only, so no database on disk requires migration. Each
phase is a self-contained commit that builds and passes tests under both
build tags, `gofmt`ed and `golangci-lint` clean.

## Phase 1 — Strip lexi's upgrade machinery (d76af68e9)

- [x] Delete the Badger v1-to-v4 migration and the `_v4` path suffix.
  `lexi.New` opens exactly the path it is given, retiring the fallback that
  silently started an empty database when an upgrade failed.
- [x] Delete `DB.Upgrade`/`ReIndex`/`DeleteIndex`, the DB-version plumbing,
  and politeia's version block.
- [x] Delete `wallet/asset/kvdb` (zero importers).
- [x] Tidy the root and nested `wallet/cmd/caribou-desktop` modules: badger
  v1 and the four modules reachable only through it dropped.
- [x] On-disk renames: `<net>/tx.db_v4/` becomes `<net>/tx.db/`,
  `<walletDir>/politeia_v4/` becomes `<walletDir>/politeia/`.

## Phase 2 — Extract the engine abstraction to util/kvstore (c4f31a121)

- [x] Move `Store`, `StoreTable`, `StoreIndex`, `StoreIter`, `IndexFunc`,
  `BatchEntry` and the sentinels from `wallet/txdb/engine.go` to
  `util/kvstore`, along with the per-engine adapters and their build tags.
- [x] Alias the moved names in `wallet/txdb`, leaving the five asset packages
  that use them untouched.

## Phase 3 — Remove the dead and legacy surface, still on bolt (67d984534)

- [x] Delete the `_v0` record decoders, `PrimaryCredentials.Version`, and the
  creds-upgrade paths; collapse every record format to a single version,
  numbered 0.
- [x] Delete `Recrypt` — the only operation requiring cross-table atomicity,
  which glossi cannot express. Deleting it retired the blocker.
- [x] Delete `Backup`/`BackupTo`/`BackupDB` and `--no-db-backup`.
- [x] Delete `util/order`, legacy hex seed import/export,
  `LegacyWalletIndex` and its frontend twin `emptyidx`, and the deprecated
  `Experimental` flag.
- [x] Re-key `seedInnerKey` to BLAKE-256("Caribou-InnerKey-v0").

Release-note items: `--no-db-backup` removed (an INI file setting it fails to
parse); wallet creation omitting `"type"` errors instead of defaulting to
`AvailableWallets[0]`; a bad mnemonic surfaces the real checksum/word error.

## Phase 4 — Backend-agnostic conformance suite, still on bolt (d32b4a7b8)

- [x] Add `wallet/db/test/suite.go` exporting
  `RunSuite(t *testing.T, newDB func(*testing.T) db.DB)`, covering the full
  interface and validated against bolt before a second backend exists. It
  pins the contracts a backend must honor for `wallet/core`: the
  `ErrNoCredentials`/`ErrNoSeedGenTime` sentinels matched with `errors.Is`,
  partial-update semantics of `UpdateBalance`/`SetWalletPassword`/
  `UpdateWalletStatus`, credential overwrite, empty- and cleared-token reads,
  per-asset `NextMultisigKeyIndex` sequencing, and newest-first
  `NotificationsN` ordering with `Ack`/`Id` population.
- [x] Reduce `wallet/db/bolt/db_test.go` to a driver calling `RunSuite`.
- [x] Leave `NotificationsN(0)` unconstrained: bolt returns all
  notifications, an index iteration would return none, and the only
  production caller passes 100.

## Phase 5 — The port (b1825231d)

- [x] Add `wallet/db/db.go` implementing `db.DB` over `kvstore.Store`:
  exported `db.New(path, log)` builds its own store, unexported
  `newDB(store, log)` accepts one for tests, mirroring `txdb.newTxDB`. Lands
  in package `db` beside the interface, which is retained for
  `core_test.go`'s `TDB` stub. Schema:
  - Table `app` — singleton values, one key each: `creds`, `seedGenTime`,
    `lang`, `disabledRateSources`, `companionToken`, `apiToken`,
    `sendRequiresPassword`, `pokes`. `PrimaryCredentials` collapses from six
    sibling keys into one versioned blob under `creds`, making
    `SetPrimaryCredentials` a single atomic `Set`; absence of the key maps to
    `ErrNoCredentials`.
  - Table `wallets` — key `assetID(4) || fieldTag(1)`: `0x01` wallet
    encoding, `0x02` balance encoding, `0x03` disabled flag. Preserves the
    partial-update semantics exactly: `UpdateBalance` (the hottest write)
    stays a single-key `Set`; `UpdateWallet` writes its pair in one
    `SetBatch`.
  - Table `notes` — key `Notification.ID()`, value a versioned blob of the
    notification encoding plus its ack flag. A non-unique `stamp` index
    emits the 8-byte big-endian timestamp; `NotificationsN(n)`
    reverse-iterates the index taking the first n.
  - Tables `multisigIndexes` (`assetID(4)` -> counter) and
    `multisigPubkeys` (`assetID(4) || pubkey(33)` -> index).
  - Access rule: `kvstore` iteration is key-ordered only on indexes — lexi
    keys table datums by an insertion-ordered internal ID, so table
    iteration runs in insertion order and its prefix argument does not
    filter user keys. `Wallet(wid)` is therefore three point `Get`s
    (`wid||0x01/0x02/0x03`); `Wallets()` iterates the whole table with a nil
    prefix, grouping entries in a map on the first 4 bytes of `K()`. Every
    other lookup is a point `Get`.
  - Concurrency: one `sync.Mutex` serializes the read-modify-write
    operations — `SetWalletPassword`, `AckNotification`,
    `NextMultisigKeyIndex` (a lost update there is a duplicate key index,
    i.e. address reuse). All three are cold paths; both engines hold an
    exclusive file lock, so there is no cross-process case.
- [x] `wallet/db/interface.go`: swap `util.Runner` for
  `Connect(ctx) (*sync.WaitGroup, error)` and `Close() error`, matching
  `txdb` and both engines' native shape.
- [x] `wallet/core/core.go`: construct via `db.New`; replace the hand-rolled
  DB goroutine and `sync.WaitGroup` with `Connect`; move
  `stopDB()`/`dbWG.Wait()` down beside `stopTxDB()` — the early stop only
  served bolt's backup/compact and loses `SaveNotification` writes fired
  from the wallet lock/disconnect path after the DB closes. Correct the
  stale "The DB starts first and stops last" comment.
- [x] `wallet/core/core_test.go`: `TDB.Run` becomes
  `TDB.Connect`/`TDB.Close`.
- [x] `wallet/app/config.go`: default DB path `caribou.db` becomes `coredb` —
  a directory on desktop, a file on mobile, the same split `tx.db` already
  has. A stale bbolt file is ignored and can be deleted by hand;
  `filepath.Dir(cfg.DBPath)` still parents `assetdb/` and `tx.db`.
- [x] Delete `wallet/db/bolt`.
- [x] Add `wallet/db/db_lexi_test.go` (`//go:build !mobile`) and
  `wallet/db/db_glossi_test.go` (`//go:build mobile`), each a driver calling
  `dbtest.RunSuite` — the first time both engines get identical coverage.
- [x] Verify: `go test ./wallet/...` and
  `go test -tags mobile ./wallet/db/...` (the Phase 4 suite passes against
  both engines); `go build -tags mobile ./...`; the artifact is
  `<net>/coredb` — a directory on desktop, a file on mobile;
  `grep -rn bbolt wallet/db` is empty.

## Phase 6 — Shed bolt-era interface baggage (e86df528d)

A post-port review pass over `db.DB` for shapes that existed only because of
bolt (or its dcrdex heritage) and cost something on the new engines.

- [x] Decompose the wallet record along writer lines: `addressTag 0x04` and
  `encPWTag 0x05` join the existing field tags, so every wallet write is a
  blind `Set`/`SetBatch` of exactly the fields its caller owns.
  `SetWalletPassword` and the new `SetWalletAddress` drop their
  read-modify-write cycles (db-layer and core-layer respectively), the db
  mutex no longer covers any wallet operation, and the wallet encoding
  shrinks to the configuration pair (type + settings) with the asset ID
  supplied by the record key. `UpdateWallet` is the full upsert of all four
  field keys; only `UpdateWalletStatus` writes the disabled flag.
- [x] Key wallets by `assetID uint32` rather than a caller-encoded `[]byte`
  ID — the byte keys were bolt bucket paths leaking through the API.
  `xcWallet.dbID`, `Wallet.ID` and `Wallet.SID` are deleted. Absent wallets
  report a matchable `ErrWalletNotFound` from every wallet method. The
  existence guard on the single-field setters is kept deliberately: a stray
  write would otherwise create an orphaned field row that poisons
  `Wallets()`, and wallets are never deleted, so the check has no TOCTOU
  window.
- [x] Delete `SeedGenerationTime`/`SetSeedGenerationTime`/`ErrNoSeedGenTime`.
  The only writer stored `uint64(creds.Birthday.Unix())` immediately after
  the credentials carrying that Birthday; core derives the value instead.
- [x] Store disabled fiat rate sources as a JSON array, dropping the comma
  join and its no-commas-in-names constraint.
- [x] Rename `stampIdx` to `noteStampIdx`; extend the conformance suite to
  pin the sentinel, the `SetWalletAddress` semantics, and the new encodings.
- [ ] Left alone for now: the INI text encoding of wallet settings inside
  the configuration blob (`config.Data`/`config.Parse`, which lowercases
  keys on read). Whether that encoding is load-bearing for frontend
  text-input coercion is still being evaluated; revisit before release.
