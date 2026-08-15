// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package glossi

import (
	"context"
	"sync"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
)

// store adapts a *DB (bbolt) to the kvstore.Store interface. glossi's sentinel
// errors are kvstore's, so no error translation is required.
type store struct {
	db *DB
}

// NewStore opens a glossi database at path and adapts it to the kvstore.Store
// interface. The path names a file.
func NewStore(path string, log util.Logger) (kvstore.Store, error) {
	db, err := New(path, log)
	if err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

// Connect forwards to DB.Connect, which binds bbolt's shutdown to ctx.
func (s *store) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	return s.db.Connect(ctx)
}

// Close closes the underlying bbolt database. Only valid when Connect has not
// been called; Connect's goroutine handles Close internally on ctx done.
func (s *store) Close() error { return s.db.Close() }

func (s *store) Table(name string) (kvstore.StoreTable, error) {
	t, err := s.db.Table(name)
	if err != nil {
		return nil, err
	}
	return &storeTable{t: t}, nil
}

type storeTable struct {
	t *Table
}

func (t *storeTable) AddIndex(name string, f kvstore.IndexFunc) (kvstore.StoreIndex, error) {
	idx, err := t.t.AddIndex(name, f)
	if err != nil {
		return nil, err
	}
	return &storeIndex{idx: idx}, nil
}

func (t *storeTable) AddUniqueIndex(name string, f kvstore.IndexFunc) (kvstore.StoreIndex, error) {
	idx, err := t.t.AddUniqueIndex(name, f)
	if err != nil {
		return nil, err
	}
	return &storeIndex{idx: idx}, nil
}

func (t *storeTable) Set(k, v kvstore.KV, replace bool) error {
	if replace {
		return t.t.Set(k, v, WithReplace())
	}
	return t.t.Set(k, v)
}

func (t *storeTable) SetBatch(entries []kvstore.BatchEntry, replace bool) error {
	if replace {
		return t.t.SetBatch(entries, WithReplace())
	}
	return t.t.SetBatch(entries)
}

func (t *storeTable) Get(k []byte) ([]byte, error) {
	return t.t.Get(k)
}

func (t *storeTable) Delete(k []byte) error {
	return t.t.Delete(k)
}

func (t *storeTable) Iterate(prefix []byte, f func(kvstore.StoreIter) error, reverse bool, seek []byte) error {
	return t.t.Iterate(prefix, func(it *Iter) error {
		return f(it)
	}, storeIterOpts(reverse, seek)...)
}

type storeIndex struct {
	idx *Index
}

func (i *storeIndex) Iterate(prefix []byte, f func(kvstore.StoreIter) error, reverse bool, seek []byte) error {
	return i.idx.Iterate(prefix, func(it *Iter) error {
		return f(it)
	}, storeIterOpts(reverse, seek)...)
}

func storeIterOpts(reverse bool, seek []byte) []IterationOption {
	var opts []IterationOption
	if reverse {
		opts = append(opts, WithReverse())
	}
	if len(seek) > 0 {
		opts = append(opts, WithSeek(seek))
	}
	return opts
}
