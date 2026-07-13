//go:build xmr

package importall

// Monero links the native libmonero_wallet2_api_c through cgo, which must be
// built and staged first, so the xmr wallet is registered only under the "xmr"
// build tag. This is additive: the default set in importall.go always
// registers, and -tags xmr adds Monero on top. Build with -tags xmr to enable
// Monero support.
import (
	_ "github.com/bisoncraft/bosswallet/wallet/asset/xmr" // register xmr asset
)
