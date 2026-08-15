// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/encode"
	"github.com/bisoncraft/caribou/util/kvstore"
	"github.com/dgraph-io/badger/v4"
)

func newBenchDB(b *testing.B) *DB {
	b.Helper()
	db, err := New(&Config{
		Path: filepath.Join(b.TempDir(), "bench.db"),
		Log:  util.StdOutLogger("B", util.LevelError),
	})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

// BenchmarkTableScanValuesOnly measures the cost this layout actually trades
// away: a table scan that reads only values, which is what
// politeia.fetchAllProposals does.
//
//   - keymap is what Table.Iterate does. It walks the key→DBID map, which is
//     ordered by user key (so prefix, seek and reverse work), then fetches each
//     datum by DBID.
//   - rowmajor is the theoretical floor, not any layout this package has
//     shipped: it walks the table's rows and decodes the value the iterator is
//     already sitting on, so it is the only variant here doing one lookup per
//     row. It cannot serve Table.Iterate, because it orders by DBID rather than
//     by key and has no way to honor a prefix or seek in key space.
//
// Both Table.Iterate layouts - this one and the row-keyed one that preceded it -
// do two lookups per row, since the older Iter.datum re-fetched the row by DBID
// rather than reusing the iterated value. The gap to rowmajor is therefore the
// standing cost of the DBID indirection, not of key-ordered iteration.
//
// Value sizes straddle typical record sizes; row counts span a small table up
// to one far larger than any table in this tree. Note that keys are inserted in
// sorted order, so key order and DBID order coincide - that favors the row-keyed
// layout, whose datum fetch would otherwise scatter.
func BenchmarkTableScanValuesOnly(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000} {
		for _, valSize := range []int{128, 4096} {
			db := newBenchDB(b)
			tbl, err := db.table("bench")
			if err != nil {
				b.Fatalf("Table: %v", err)
			}
			for i := 0; i < n; i++ {
				k := []byte(fmt.Sprintf("key-%08d", i))
				if err := tbl.Set(k, encode.RandomBytes(valSize), kvstore.WithReplace()); err != nil {
					b.Fatalf("Set: %v", err)
				}
			}

			label := fmt.Sprintf("n%d/val%d", n, valSize)

			b.Run("keymap/"+label, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var total int
					if err := tbl.Iterate(nil, func(it kvstore.StoreIter) error {
						return it.V(func(v []byte) error {
							total += len(v)
							return nil
						})
					}); err != nil {
						b.Fatalf("Iterate: %v", err)
					}
					if total != n*valSize {
						b.Fatalf("read %d bytes, want %d", total, n*valSize)
					}
				}
			})

			b.Run("rowmajor/"+label, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var total int
					if err := db.View(func(txn *badger.Txn) error {
						return iteratePrefix(txn, PrefixedKey(tbl.prefix, nil), nil, func(iter *badger.Iterator) error {
							return iter.Item().Value(func(dB []byte) error {
								d, err := decodeDatum(dB)
								if err != nil {
									return err
								}
								total += len(d.v)
								return nil
							})
						})
					}); err != nil {
						b.Fatalf("row scan: %v", err)
					}
					if total != n*valSize {
						b.Fatalf("read %d bytes, want %d", total, n*valSize)
					}
				}
			})
		}
	}
}
