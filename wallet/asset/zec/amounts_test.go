// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import "testing"

func TestToSatoshi(t *testing.T) {
	tests := []struct {
		name string
		v    float64
		want uint64
	}{
		{"zero", 0, 0},
		{"one", 1, 1e8},
		{"fraction", 0.00000001, 1},
		{"rounds up", 1.000000005, 100000001},
		{"rounds down", 1.000000004, 100000000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toSatoshi(tt.v); got != tt.want {
				t.Errorf("toSatoshi(%v) = %d, want %d", tt.v, got, tt.want)
			}
		})
	}
}

func TestToBTC(t *testing.T) {
	if got := toBTC(uint64(1e8)); got != 1 {
		t.Errorf("toBTC(1e8) = %v, want 1", got)
	}
	if got := toBTC(int64(-1e8)); got != -1 {
		t.Errorf("toBTC(-1e8) = %v, want -1", got)
	}
	if got := toBTC(uint64(0)); got != 0 {
		t.Errorf("toBTC(0) = %v, want 0", got)
	}
}
