// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/bisoncraft/bosswallet/util"
)

type addressRecycler struct {
	recyclePath string
	log         util.Logger

	mtx   sync.Mutex
	addrs map[string]struct{}
}

func newAddressRecycler(recyclePath string, log util.Logger) (*addressRecycler, error) {
	// Try to load any cached unused redemption addresses.
	b, err := os.ReadFile(recyclePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("error looking for recycled address file: %w", err)
	}
	addrs := strings.Split(string(b), "\n")
	recycledAddrs := make(map[string]struct{}, len(addrs))
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		recycledAddrs[addr] = struct{}{}
	}
	return &addressRecycler{
		recyclePath: recyclePath,
		log:         log,
		addrs:       recycledAddrs,
	}, nil
}

// WriteRecycledAddrsToFile writes the recycled address cache to file.
func (a *addressRecycler) WriteRecycledAddrsToFile() {
	a.mtx.Lock()
	addrs := make([]string, 0, len(a.addrs))
	for addr := range a.addrs {
		addrs = append(addrs, addr)
	}
	a.mtx.Unlock()
	contents := []byte(strings.Join(addrs, "\n"))
	if err := os.WriteFile(a.recyclePath, contents, 0600); err != nil {
		a.log.Errorf("Error writing recycled address file: %v", err)
	}
}

func (a *addressRecycler) Address() string {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	for addr := range a.addrs {
		delete(a.addrs, addr)
		return addr
	}
	return ""
}

func (a *addressRecycler) ReturnAddresses(addrs []string) {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	for _, addr := range addrs {
		if _, exists := a.addrs[addr]; exists {
			a.log.Errorf("Returned address %q was already indexed", addr)
			continue
		}
		a.addrs[addr] = struct{}{}
	}
}
