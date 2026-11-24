//go:build !zec

package importall

import (
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset"
)

func TestZcashNotRegistered(t *testing.T) {
	if asset.Asset(133) != nil {
		t.Fatal("Zcash is registered without the zec build tag")
	}
}
