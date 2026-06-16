//go:build dcr

package importall

// Decred governance (Politeia) links Badger through util/politeia, so the dcr
// wallet is registered only under the "dcr" build tag. Without the tag the
// default build omits dcr and does not link Badger. Build with -tags dcr to
// re-enable Decred support.
import (
	_ "github.com/bisoncraft/bosswallet/wallet/asset/dcr" // register dcr asset
)
