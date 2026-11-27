// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"strings"
	"testing"
)

func TestRestorationInfo(t *testing.T) {
	e := openEnv(t)
	const phrase = "legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful " +
		"legal winner thank year wave sausage worth title"
	rs, err := e.w.RestorationInfo(testEntropy)
	if err != nil || len(rs) != 2 {
		t.Fatalf("RestorationInfo: %d, %v", len(rs), err)
	}
	if rs[0].Target != "zcash-devtool" || !strings.Contains(rs[0].Instructions, "--birthday 100") {
		t.Errorf("zcash-devtool: %+v", rs[0])
	}
	for _, r := range rs {
		if r.Seed != phrase {
			t.Errorf("%s: phrase %q", r.Target, r.Seed)
		}
		for _, need := range []string{"Ironwood", "ZIP 32 account 0", "empty passphrase", "100 or lower"} {
			if !strings.Contains(r.Instructions, need) {
				t.Errorf("%s: the instructions do not say %q", r.Target, need)
			}
		}
	}
	if _, err := e.w.RestorationInfo(make([]byte, 64)); err == nil {
		t.Error("a 64-byte seed")
	}
	// A closed wallet gives the phrase and a lower bound for the birthday,
	// and says so.
	e.w.Close()
	rs, err = e.w.RestorationInfo(testEntropy)
	if err != nil || rs[0].Seed != phrase || !strings.Contains(rs[0].Instructions, "--birthday 2") ||
		!strings.Contains(rs[0].Instructions, "could not be read") {
		t.Errorf("RestorationInfo of a closed wallet: %+v, %v", rs, err)
	}
}

func TestFees(t *testing.T) {
	e := openEnv(t)
	if r := e.w.FeeRate(); r != 5_000 {
		t.Errorf("FeeRate %d", r)
	}
	if f := e.w.StandardSendFee(0); f != 10_000 {
		t.Errorf("StandardSendFee(0) %d", f)
	}
	if f := e.w.StandardSendFee(5_000); f != 10_000 {
		t.Errorf("StandardSendFee(5000) %d", f)
	}
}
