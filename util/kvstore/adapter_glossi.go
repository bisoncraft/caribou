// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build mobile

package kvstore

import (
	"context"
	"sync"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/glossi"
)

// glossiStore adapts a *glossi.DB (bbolt) to the Store interface. glossi's
// sentinel errors (ErrKeyNotFound, ErrEndIteration, ErrNotIndexed) share their
// underlying string values with this package's, so no error translation is
// required.
type glossiStore struct {
	db *glossi.DB
}

func newGlossiStore(path string, log util.Logger) (Store, error) {
	db, err := glossi.New(path, log)
	if err != nil {
		return nil, err
	}
	return &glossiStore{db: db}, nil
}

// Connect forwards to glossi.DB.Connect, which binds bbolt's shutdown to ctx.
func (s *glossiStore) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	return s.db.Connect(ctx)
}

func (s *glossiStore) Table(name string) (StoreTable, error) {
	t, err := s.db.Table(name)
	if err != nil {
		return nil, err
	}
	return &glossiTable{t: t}, nil
}

// Close closes the underlying bbolt database. Only valid when Connect has not
// been called; glossi.Connect's goroutine handles Close internally on ctx done.
func (s *glossiStore) Close() error { return s.db.Close() }

type glossiTable struct {
	t *glossi.Table
}

func (t *glossiTable) AddIndex(name string, f IndexFunc) (StoreIndex, error) {
	idx, err := t.t.AddIndex(name, f)
	if err != nil {
		return nil, err
	}
	return &glossiIndex{idx: idx}, nil
}

func (t *glossiTable) AddUniqueIndex(name string, f IndexFunc) (StoreIndex, error) {
	idx, err := t.t.AddUniqueIndex(name, f)
	if err != nil {
		return nil, err
	}
	return &glossiIndex{idx: idx}, nil
}

func (t *glossiTable) Set(k, v []byte, replace bool) error {
	if replace {
		return t.t.Set(k, v, glossi.WithReplace())
	}
	return t.t.Set(k, v)
}

func (t *glossiTable) SetBatch(entries []BatchEntry, replace bool) error {
	kvs := make([]glossi.KV, len(entries))
	for i, e := range entries {
		kvs[i] = glossi.KV{K: e.K, V: e.V}
	}
	if replace {
		return t.t.SetBatch(kvs, glossi.WithReplace())
	}
	return t.t.SetBatch(kvs)
}

func (t *glossiTable) Get(k []byte) ([]byte, error) {
	return t.t.Get(k)
}

func (t *glossiTable) Delete(k []byte) error {
	return t.t.Delete(k)
}

func (t *glossiTable) Iterate(prefix []byte, f func(StoreIter) error, reverse bool, seek []byte) error {
	return t.t.Iterate(prefix, func(it *glossi.Iter) error {
		return f(it)
	}, glossiIterOpts(reverse, seek)...)
}

type glossiIndex struct {
	idx *glossi.Index
}

func (i *glossiIndex) Iterate(prefix []byte, f func(StoreIter) error, reverse bool, seek []byte) error {
	return i.idx.Iterate(prefix, func(it *glossi.Iter) error {
		return f(it)
	}, glossiIterOpts(reverse, seek)...)
}

func glossiIterOpts(reverse bool, seek []byte) []glossi.IterationOption {
	var opts []glossi.IterationOption
	if reverse {
		opts = append(opts, glossi.WithReverse())
	}
	if len(seek) > 0 {
		opts = append(opts, glossi.WithSeek(seek))
	}
	return opts
}
