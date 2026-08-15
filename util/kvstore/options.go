// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package kvstore

// SetOpts is the resolved form of a list of SetOptions. Engines apply the
// options to a zero (or table-default) SetOpts and read the fields.
type SetOpts struct {
	// Replace allows overwriting an existing value for the key. Without it,
	// Set returns an error if the key (or, for a unique index, the index
	// entry) already exists.
	Replace bool
	// Txn, when non-nil, is an engine-native transaction handle to run the
	// operation inside, instead of the engine opening its own. There is no
	// engine-agnostic constructor: each engine exports a typed one
	// (lexi.WithTxn takes a *badger.Txn, glossi.WithTxn a *bbolt.Tx) and
	// rejects a handle of the wrong type. SetBatch always runs in its own
	// transaction and overrides this field.
	Txn any
}

// SetOption is a knob controlling how items are inserted with Set and
// SetBatch.
type SetOption func(*SetOpts)

// WithReplace allows replacing a pre-existing value when calling Set.
func WithReplace() SetOption {
	return func(o *SetOpts) { o.Replace = true }
}

// GetOpts is the resolved form of a list of GetOptions.
type GetOpts struct {
	// Txn is an engine-native transaction handle to read inside; see
	// SetOpts.Txn. Constructed with the engines' typed WithGetTxn.
	Txn any
}

// GetOption is a knob controlling how items are retrieved with Get.
type GetOption func(*GetOpts)

// IterationOpts is the resolved form of a list of IterationOptions.
type IterationOpts struct {
	// Update runs the iteration in a read-write transaction so the callback
	// may modify the database, e.g. via StoreIter.Delete.
	Update bool
	// Reverse iterates in reverse-lexicographical order.
	Reverse bool
	// Seek positions the start of iteration. It is in the same key space as
	// the iteration's prefix argument: index-entry space for an index, primary
	// keys for a table.
	Seek []byte
}

// IterationOption is a knob controlling how Iterate runs on a table or index.
type IterationOption func(*IterationOpts)

// WithUpdate must be used if the callback intends to modify the database
// during iteration, e.g. via StoreIter.Delete.
func WithUpdate() IterationOption {
	return func(o *IterationOpts) { o.Update = true }
}

// WithReverse sets the direction of iteration to reverse-lexicographical.
func WithReverse() IterationOption {
	return func(o *IterationOpts) { o.Reverse = true }
}

// WithForward sets the direction of iteration to lexicographical, clearing a
// previously-applied WithReverse (e.g. a table default).
func WithForward() IterationOption {
	return func(o *IterationOpts) { o.Reverse = false }
}

// WithSeek starts iteration at the specified key.
func WithSeek(seek []byte) IterationOption {
	return func(o *IterationOpts) { o.Seek = seek }
}
