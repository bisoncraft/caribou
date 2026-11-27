//go:build zec && cgo

package importall

// Zcash's wallet, zeclib, links a Rust library (librustzcash) through cgo,
// which must be built and staged first, so the zec wallet is registered only
// under the "zec" build tag. This is additive: the default set in importall.go
// always registers, and -tags zec adds Zcash on top. The tag requires cgo; a
// build with -tags zec and CGO_ENABLED=0 fails (importall_zec_nocgo.go). The
// build instructions are in wallet/asset/zec/zeclib/README.md.
import (
	_ "github.com/bisoncraft/caribou/wallet/asset/zec" // register zec asset
)
