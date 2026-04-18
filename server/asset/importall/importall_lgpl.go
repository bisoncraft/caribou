package importall

import (
	_ "github.com/bisoncraft/meshwallet/server/asset/base"    // register base asset
	_ "github.com/bisoncraft/meshwallet/server/asset/eth"     // register eth asset
	_ "github.com/bisoncraft/meshwallet/server/asset/polygon" // register polygon asset
)
