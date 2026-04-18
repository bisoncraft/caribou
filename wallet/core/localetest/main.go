package main

import (
	"fmt"

	"github.com/bisoncraft/meshwallet/wallet/core"
)

func main() {
	fmt.Printf("Missing %d translations \n", core.CheckTopicLangs())
}
