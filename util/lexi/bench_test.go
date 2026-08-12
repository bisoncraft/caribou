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

// BenchmarkScanValuesOnly measures value-only scans through both access paths,
// recording the trade the row layout makes.
//
//   - table is Table.Iterate. Rows are keyed by user key, so the scan decodes
//     the datum the cursor is already sitting on: one lookup per row, and
//     key-ordered prefix, seek and reverse come for free.
//   - index is Index.Iterate + Iter.V. An index entry carries only the row's
//     KeyID, so the row is resolved through the KeyID→key map and then fetched
//     by key: two extra lookups per row. That is the standing cost of keeping
//     index entries short rather than echoing full keys into every index.
//
// Value sizes straddle typical record sizes; row counts span a small table up
// to one far larger than any table in this tree. Keys are inserted in sorted
// order, so the index (which orders by insertion count here) visits rows in
// key order and its row fetches don't scatter; an adversarial insertion order
// would widen the gap.
func BenchmarkScanValuesOnly(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000} {
		for _, valSize := range []int{128, 4096} {
			db := newBenchDB(b)
			tbl, err := db.table("bench")
			if err != nil {
				b.Fatalf("Table: %v", err)
			}
			// seq counts index-function invocations, one per Set here, so
			// index order matches insertion order.
			var seq int
			idx, err := tbl.AddIndex("seq", func(_, _ KV) ([]byte, error) {
				e := fmt.Appendf(nil, "idx-%08d", seq)
				seq++
				return e, nil
			})
			if err != nil {
				b.Fatalf("AddIndex: %v", err)
			}
			for i := 0; i < n; i++ {
				k := []byte(fmt.Sprintf("key-%08d", i))
				if err := tbl.Set(k, encode.RandomBytes(valSize), kvstore.WithReplace()); err != nil {
					b.Fatalf("Set: %v", err)
				}
			}

			label := fmt.Sprintf("n%d/val%d", n, valSize)

			b.Run("table/"+label, func(b *testing.B) {
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

			b.Run("index/"+label, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var total int
					if err := idx.Iterate(nil, func(it kvstore.StoreIter) error {
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
		}
	}
}
