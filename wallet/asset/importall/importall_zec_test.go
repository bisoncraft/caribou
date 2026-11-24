//go:build zec && cgo

package importall

import (
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
)

func TestZcashRegistered(t *testing.T) {
	if asset.Asset(133) == nil {
		t.Fatal("Zcash is not registered under the zec build tag")
	}
}
