// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package glossi

import (
	"fmt"

	"github.com/bisoncraft/caribou/util/encode"
)

// datumVersion is the version of the datum encoding.
const datumVersion = 0

// A datum is the value stored in a Table's data bucket. It is the value blob
// followed by the list of index-entry keys that this datum generated. Storing
// the index keys inline lets Set (on replace) and Delete remove stale index
// entries without re-deriving them.
//
// Encoding is a versioned, length-prefixed blob (see util/encode.BuildyBytes):
//
//	[version][value][indexKey0][indexKey1]...
//
// i.e. push 0 is the value and the remaining pushes are the index keys.
func encodeDatum(value []byte, indexKeys [][]byte) []byte {
	b := encode.BuildyBytes{datumVersion}.AddData(value)
	for _, ik := range indexKeys {
		b = b.AddData(ik)
	}
	return b
}

func decodeDatum(b []byte) (value []byte, indexKeys [][]byte, err error) {
	ver, pushes, err := encode.DecodeBlob(b)
	if err != nil {
		return nil, nil, fmt.Errorf("error decoding datum: %w", err)
	}
	if ver != datumVersion {
		return nil, nil, fmt.Errorf("unknown datum version %d", ver)
	}
	if len(pushes) == 0 {
		return nil, nil, fmt.Errorf("datum has no value push")
	}
	return pushes[0], pushes[1:], nil
}
