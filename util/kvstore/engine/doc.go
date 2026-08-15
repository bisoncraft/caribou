// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package engine selects the kvstore engine at build time: Badger (util/lexi)
// on desktop and bbolt (util/glossi) on mobile. It exists apart from
// util/kvstore so that the engines themselves can import kvstore's shared
// types (KV, BatchEntry, the sentinel errors) without an import cycle.
package engine
