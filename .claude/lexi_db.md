# Lexi DB Reference

Lexi DB is an indexed key-value database wrapping Badger v4 (`github.com/dgraph-io/badger/v4`). It lives at `dex/lexi/` and provides tables with automatic secondary indexes, DBID-based key compression, and versioned upgrades.

## Files

| File | Purpose |
|------|---------|
| `dex/lexi/lexi.go` | DB struct, New(), Connect(), Update(), Upgrade(), KeyID(), version ops |
| `dex/lexi/table.go` | Table type: Get, GetRaw, Set, Delete, Iterate |
| `dex/lexi/index.go` | Index type: AddIndex, AddUniqueIndex, Iterate, Iter (V/K/Entry/Delete), ReIndex, DeleteIndex |
| `dex/lexi/datum.go` | Internal value encoding (version + index refs + value bytes) |
| `dex/lexi/dbid.go` | DBID type ([8]byte unique ID mapped to each key) |
| `dex/lexi/keyprefix.go` | 2-byte prefix system for tables/indexes, PrefixedKey helper |
| `dex/lexi/json.go` | JSON() and UnJSON() convenience wrappers for BinaryMarshal |
| `dex/lexi/log.go` | Badger logger wrapper |
| `dex/lexi/db_test.go` | Tests |
| `dex/lexi/cmd/lexidbexplorer/` | Interactive CLI explorer |

## Core Types

```go
// DB wraps badger.DB with indexed table support
type DB struct {
    *badger.DB
    log        dex.Logger
    idSeq      *badger.Sequence
    upgradeTxn *badger.Txn  // non-nil during Upgrade()
}

type Config struct {
    Path string
    Log  dex.Logger
}

// KV is accepted as keys/values: []byte, uint32, time.Time, encoding.BinaryMarshaler, nil
type KV any

// DBID is an 8-byte internal ID mapped to each user key to keep index entries short
type DBID [8]byte
```

## Lifecycle

```go
// Create
db, err := lexi.New(&lexi.Config{Path: "/path/to/db", Log: logger})

// Start (launches GC goroutine, returns WaitGroup for shutdown)
wg, err := db.Connect(ctx)

// Shutdown: cancel the ctx, then wg.Wait()
```

## Tables

```go
tbl, err := db.Table("myTable")

// Insert (key and value are KV - []byte, BinaryMarshaler, uint32, time.Time, nil)
err = tbl.Set(key, value)
err = tbl.Set(key, value, lexi.WithReplace())          // allow overwrite
err = tbl.Set(key, value, lexi.WithTxn(txn))            // use existing txn

// Retrieve
err = tbl.Get(key, &myStruct)                           // BinaryUnmarshaler
rawBytes, err := tbl.GetRaw(key)
rawBytes, err = tbl.GetRaw(key, lexi.WithGetTxn(txn))

// Delete
err = tbl.Delete(keyBytes)

// Set defaults
tbl.UseDefaultSetOptions(lexi.WithReplace())
tbl.UseDefaultIterationOptions(lexi.WithReverse())
```

**Key rules:**
- Keys must have length > 0 (zero-length keys break reverse iteration)
- Values are stored as a `datum` internally: `[version:1B][indexes...][value]`

## Indexes

```go
// Add a regular index (duplicates allowed in index values)
idx, err := tbl.AddIndex("byTimestamp", func(k, v lexi.KV) ([]byte, error) {
    wt := v.(*MyTx)
    b := make([]byte, 8)
    binary.BigEndian.PutUint64(b, uint64(wt.Timestamp))
    return b, nil
})

// Add a unique index (enforces uniqueness on the index value)
idx, err := tbl.AddUniqueIndex("byNonce", func(k, v lexi.KV) ([]byte, error) {
    wt := v.(*MyTx)
    return nonceBytes(wt), nil
})

// Conditional indexing - return ErrNotIndexed to skip
idx, err := tbl.AddIndex("confirmedOnly", func(k, v lexi.KV) ([]byte, error) {
    wt := v.(*MyTx)
    if wt.BlockNumber == 0 {
        return nil, lexi.ErrNotIndexed
    }
    return blockBytes(wt), nil
})
```

**Important:** Indexes must be added before inserting data. Items inserted before `AddIndex` won't be indexed. Use `ReIndex` to fix.

**How indexes work internally:** Each index entry is stored as a badger key `[prefix:2B][indexValue][DBID:8B]` with nil value. The DBID at the end maps back to the table entry. Because index entries are sorted lexicographically, the index value determines sort order.

## Iteration

```go
// Iterate a table (entries ordered by DBID, i.e. insertion order)
tbl.Iterate(nil, func(it *lexi.Iter) error {
    // Access value bytes (only valid during callback!)
    return it.V(func(vB []byte) error {
        // use vB here, copy if needed beyond this scope
        return json.Unmarshal(vB, &myStruct)
    })
})

// Iterate an index
idx.Iterate(nil, func(it *lexi.Iter) error { ... })

// Reverse iteration
idx.Iterate(nil, f, lexi.WithReverse())

// Seek to a position
idx.Iterate(nil, f, lexi.WithSeek(startBytes))

// Prefix-scoped iteration (e.g., all entries for a specific asset ID)
assetIDBytes := make([]byte, 4)
binary.BigEndian.PutUint32(assetIDBytes, assetID)
idx.Iterate(assetIDBytes, f)    // only entries whose index value starts with assetIDBytes

// Early termination
idx.Iterate(nil, func(it *lexi.Iter) error {
    if done {
        return lexi.ErrEndIteration  // stops iteration, no error returned
    }
    return nil
})

// Delete during iteration (requires WithUpdate)
idx.Iterate(nil, func(it *lexi.Iter) error {
    return it.Delete()  // deletes entry + all its index entries
}, lexi.WithUpdate())
```

### Iter methods

| Method | Signature | Notes |
|--------|-----------|-------|
| `V` | `func(f func(vB []byte) error) error` | Value bytes valid only during callback |
| `K` | `func() ([]byte, error)` | Original user key |
| `Entry` | `func(f func(idxB []byte) error) error` | Index entry bytes (without prefix/DBID) |
| `Delete` | `func() error` | Requires WithUpdate() |

### Iteration options

| Option | Purpose |
|--------|---------|
| `WithReverse()` | Reverse lexicographic order |
| `WithForward()` | Forward order (default, useful to override a reverse default) |
| `WithSeek([]byte)` | Start at position |
| `WithUpdate()` | Required for Delete during iteration |

## Transactions

```go
// Write transaction with automatic ErrConflict retry (up to 10x, exponential backoff)
db.Update(func(txn *badger.Txn) error {
    // Multiple Set/Delete calls here are atomic
    tbl.Set(k1, v1, lexi.WithTxn(txn))
    tbl.Set(k2, v2, lexi.WithTxn(txn))
    return nil
})

// Read-only transaction (inherited from badger.DB)
db.View(func(txn *badger.Txn) error { ... })
```

## Versioning and Upgrades

```go
version, err := db.GetDBVersion()  // returns 0 if never set
err = db.SetDBVersion(2)

// Atomic multi-step upgrade (all Update calls share one txn)
db.Upgrade(func() error {
    if version < 1 {
        db.ReIndex("tableName", "indexName", func(k, v []byte) ([]byte, error) {
            // decode v, compute new index entry
            return newIndexBytes, nil
        })
    }
    return db.SetDBVersion(1)
})

// Nuclear option: drop all data and rebuild schema
db.DropAll()
db.SetDBVersion(newVersion)

// Delete an index entirely
db.DeleteIndex("tableName", "indexName")

// Rebuild an index from existing data
db.ReIndex("tableName", "indexName", func(k, v []byte) ([]byte, error) {
    // k = original key, v = raw value bytes
    // return new index entry, or lexi.ErrNotIndexed to skip
    return computeNewEntry(v), nil
})
```

## JSON Convenience

```go
// Store JSON-encodable types without implementing BinaryMarshal
tbl.Set(key, lexi.JSON(&myStruct))

// Retrieve
tbl.Get(key, lexi.JSON(&myStruct))

// In index functions, unwrap with UnJSON
idx, _ := tbl.AddIndex("idx", func(k, v lexi.KV) ([]byte, error) {
    s := lexi.UnJSON(v).(*MyStruct)
    return s.SortKey(), nil
})
```

## Errors

| Error | Meaning |
|-------|---------|
| `lexi.ErrKeyNotFound` | Key not in table (alias for `badger.ErrKeyNotFound`, `errors.Is` compatible) |
| `lexi.ErrEndIteration` | Return from iteration callback to stop early (not propagated as error) |
| `lexi.ErrNotIndexed` | Return from index function to skip indexing this entry |

## Consumers in the codebase

| Package | File | Tables | Use case |
|---------|------|--------|----------|
| `wallet/asset/eth` | `txdb.go` | "txs", "bridgeCompletions" | ETH/Polygon tx history with indexes by asset, block, nonce, bridge status |
| `wallet/asset/near` | `txdb.go` | "txs" | NEAR tx history with block-number index for confirmed txs |
| `wallet/asset/btc` | `txdb.go` | (similar pattern) | BTC tx history |
| `dex/politeia` | `politeia.go` | "proposals", "proposalMeta" | Decred governance proposals with status/timestamp indexes |
| `tatanka/db` | `db.go` | "scores", "bonds" | Reputation scores and bond tracking |

## Typical setup pattern (from ETH txdb.go)

```go
func NewTxDB(path string, log dex.Logger) (*TxDB, error) {
    ldb, err := lexi.New(&lexi.Config{Path: path, Log: log})
    if err != nil {
        return nil, err
    }
    txs, err := ldb.Table("txs")
    if err != nil {
        return nil, err
    }
    // Add indexes BEFORE any data operations
    idx, err := txs.AddUniqueIndex("byBlock", func(k, v lexi.KV) ([]byte, error) {
        wt := v.(*myTxType)
        return blockEntry(wt), nil
    })
    if err != nil {
        return nil, err
    }
    txs.UseDefaultSetOptions(lexi.WithReplace())
    
    db := &TxDB{DB: ldb, txs: txs, idx: idx}
    return db, db.upgrade()  // run version-based migrations
}
```

## Gotchas

1. **Iter.V() bytes are transient** - copy them if needed beyond the callback scope
2. **Zero-length keys forbidden** - breaks reverse iteration internally
3. **Use db.Update(), not db.DB.Update()** - the wrapper handles ErrConflict retries
4. **Indexes must be added before data** - use ReIndex for existing data
5. **DropAll() destroys prefix mappings** - tables/indexes need recreation after
6. **Index entry format determines sort order** - use big-endian fixed-width encoding for numeric fields
7. **Unique index + Set without WithReplace()** - returns error on conflict
8. **Unique index + Set with WithReplace()** - deletes the conflicting entry entirely (not just the index), then inserts the new one
