// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !mobile

package kvstore

import (
	"context"
	"errors"
	"sync"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/util/lexi"
)

// lexiStore adapts a *lexi.DB (Badger) to the Store interface. lexi's
// ErrEndIteration and ErrNotIndexed share their underlying string values with
// this package's sentinels, so callback/index-function errors round-trip
// without translation. Only ErrKeyNotFound (badger.ErrKeyNotFound) differs and
// is translated in Get/Delete.
type lexiStore struct {
	db *lexi.DB
}

func newLexiStore(path string, log util.Logger) (Store, error) {
	db, err := lexi.New(&lexi.Config{Path: path, Log: log})
	if err != nil {
		return nil, err
	}
	return &lexiStore{db: db}, nil
}

// Connect forwards to lexi.DB.Connect, which spawns the value-log GC ticker
// and binds Badger's shutdown (id-sequence release + Close) to ctx.
func (s *lexiStore) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	return s.db.Connect(ctx)
}

// Close closes the underlying Badger database. Only valid when Connect has not
// been called; lexi.Connect's goroutine handles Close internally on ctx done.
func (s *lexiStore) Close() error {
	return s.db.Close()
}

func (s *lexiStore) Table(name string) (StoreTable, error) {
	t, err := s.db.Table(name)
	if err != nil {
		return nil, err
	}
	return &lexiTable{t: t}, nil
}

type lexiTable struct {
	t *lexi.Table
}

func wrapLexiIndexFunc(f IndexFunc) func(k, v lexi.KV) ([]byte, error) {
	return func(k, v lexi.KV) ([]byte, error) {
		kB, _ := k.([]byte)
		vB, _ := v.([]byte)
		return f(kB, vB)
	}
}

func (t *lexiTable) AddIndex(name string, f IndexFunc) (StoreIndex, error) {
	idx, err := t.t.AddIndex(name, wrapLexiIndexFunc(f))
	if err != nil {
		return nil, err
	}
	return &lexiIndex{idx: idx}, nil
}

func (t *lexiTable) AddUniqueIndex(name string, f IndexFunc) (StoreIndex, error) {
	idx, err := t.t.AddUniqueIndex(name, wrapLexiIndexFunc(f))
	if err != nil {
		return nil, err
	}
	return &lexiIndex{idx: idx}, nil
}

func (t *lexiTable) Set(k, v []byte, replace bool) error {
	if replace {
		return t.t.Set(k, v, lexi.WithReplace())
	}
	return t.t.Set(k, v)
}

func (t *lexiTable) SetBatch(entries []BatchEntry, replace bool) error {
	pairs := make([]lexi.KVPair, len(entries))
	for i, e := range entries {
		pairs[i] = lexi.KVPair{K: e.K, V: e.V}
	}
	if replace {
		return t.t.SetBatch(pairs, lexi.WithReplace())
	}
	return t.t.SetBatch(pairs)
}

func (t *lexiTable) Get(k []byte) ([]byte, error) {
	b, err := t.t.GetRaw(k)
	if errors.Is(err, lexi.ErrKeyNotFound) {
		return nil, ErrKeyNotFound
	}
	return b, err
}

func (t *lexiTable) Delete(k []byte) error {
	err := t.t.Delete(k)
	if errors.Is(err, lexi.ErrKeyNotFound) {
		return ErrKeyNotFound
	}
	return err
}

func (t *lexiTable) Iterate(prefix []byte, f func(StoreIter) error, reverse bool, seek []byte) error {
	var prefixKV lexi.KV
	if len(prefix) > 0 {
		prefixKV = prefix
	}
	return t.t.Iterate(prefixKV, func(it *lexi.Iter) error {
		return f(it)
	}, lexiIterOpts(reverse, seek)...)
}

type lexiIndex struct {
	idx *lexi.Index
}

func (i *lexiIndex) Iterate(prefix []byte, f func(StoreIter) error, reverse bool, seek []byte) error {
	var prefixKV lexi.KV
	if len(prefix) > 0 {
		prefixKV = prefix
	}
	return i.idx.Iterate(prefixKV, func(it *lexi.Iter) error {
		return f(it)
	}, lexiIterOpts(reverse, seek)...)
}

func lexiIterOpts(reverse bool, seek []byte) []lexi.IterationOption {
	var opts []lexi.IterationOption
	if reverse {
		opts = append(opts, lexi.WithReverse())
	}
	if len(seek) > 0 {
		opts = append(opts, lexi.WithSeek(seek))
	}
	return opts
}
