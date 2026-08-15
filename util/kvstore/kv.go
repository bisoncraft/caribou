// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package kvstore

import (
	"encoding"
	"encoding/binary"
	"fmt"
	"time"
)

// KV is any one of a number of common types whose binary encoding is
// straight-forward. Both engines (util/lexi and util/glossi) accept KV
// arguments for keys, values and iteration prefixes, and parse them with
// ParseKV.
type KV any

// ParseKV converts a KV to its binary encoding.
func ParseKV(i KV) (b []byte, err error) {
	switch it := i.(type) {
	case []byte:
		b = it
	case uint32:
		b = make([]byte, 4)
		binary.BigEndian.PutUint32(b[:], it)
	case time.Time:
		b = make([]byte, 8)
		binary.BigEndian.PutUint64(b[:], uint64(it.UnixMilli()))
	case encoding.BinaryMarshaler:
		b, err = it.MarshalBinary()
	case nil:
	default:
		err = fmt.Errorf("unknown KV type %T", it)
	}
	return
}
