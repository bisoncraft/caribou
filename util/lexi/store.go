// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"context"
	"errors"
	"sync"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/kvstore"
)

// store adapts a *DB (Badger) to the kvstore.Store interface. lexi's
// ErrEndIteration and ErrNotIndexed share their underlying string values with
// kvstore's sentinels, so callback/index-function errors round-trip without
// translation. Only ErrKeyNotFound (badger.ErrKeyNotFound) differs and is
// translated in Get/Delete.
type store struct {
	db *DB
}

// NewStore opens a lexi database at path and adapts it to the kvstore.Store
// interface. The path names a directory.
func NewStore(path string, log util.Logger) (kvstore.Store, error) {
	db, err := New(&Config{Path: path, Log: log})
	if err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

// Connect forwards to DB.Connect, which spawns the value-log GC ticker and
// binds Badger's shutdown (id-sequence release + Close) to ctx.
func (s *store) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	return s.db.Connect(ctx)
}

// Close closes the underlying Badger database. Only valid when Connect has not
// been called; Connect's goroutine handles Close internally on ctx done.
func (s *store) Close() error {
	return s.db.Close()
}

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
	b, err := t.t.GetRaw(k)
	if errors.Is(err, ErrKeyNotFound) {
		return nil, kvstore.ErrKeyNotFound
	}
	return b, err
}

func (t *storeTable) Delete(k []byte) error {
	err := t.t.Delete(k)
	if errors.Is(err, ErrKeyNotFound) {
		return kvstore.ErrKeyNotFound
	}
	return err
}

func (t *storeTable) Iterate(prefix []byte, f func(kvstore.StoreIter) error, reverse bool, seek []byte) error {
	var prefixKV KV
	if len(prefix) > 0 {
		prefixKV = prefix
	}
	return t.t.Iterate(prefixKV, func(it *Iter) error {
		return f(it)
	}, storeIterOpts(reverse, seek)...)
}

type storeIndex struct {
	idx *Index
}

func (i *storeIndex) Iterate(prefix []byte, f func(kvstore.StoreIter) error, reverse bool, seek []byte) error {
	var prefixKV KV
	if len(prefix) > 0 {
		prefixKV = prefix
	}
	return i.idx.Iterate(prefixKV, func(it *Iter) error {
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
