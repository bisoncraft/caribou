package main

import (
	"fmt"

	"github.com/bisoncraft/caribou/wallet/core"
)

func main() {
	fmt.Printf("Missing %d translations \n", core.CheckTopicLangs())
}
