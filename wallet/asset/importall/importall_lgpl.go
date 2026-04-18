package importall

import (
	_ "github.com/bisoncraft/meshwallet/wallet/asset/base" // register base network
	_ "github.com/bisoncraft/meshwallet/wallet/asset/eth"  // register eth asset
	_ "github.com/bisoncraft/meshwallet/wallet/asset/near" // register near asset
)
