package main

import (
	"fmt"

	"github.com/bisoncraft/bosswallet/wallet/core"
)

func main() {
	fmt.Printf("Missing %d translations \n", core.CheckTopicLangs())
}
