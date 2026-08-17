// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"slices"
	"testing"
)

// TestLimitDefaultProviders pins the provider cap per build. The expectation is
// build-tagged alongside the function itself; see providers_desktop.go and
// providers_mobile.go.
func TestLimitDefaultProviders(t *testing.T) {
	full := []string{"one", "two", "three", "four", "five"}

	// The input must survive: it is package-level state in the chain packages,
	// shared by every wallet they construct, so a cap that aliased or truncated
	// it in place would corrupt the next wallet's list.
	input := slices.Clone(full)
	got := limitDefaultProviders(input)
	if !slices.Equal(input, full) {
		t.Fatalf("input mutated to %v, want %v", input, full)
	}
	if len(got) != wantProviderLimit {
		t.Fatalf("limited %d providers to %d, want %d", len(full), len(got), wantProviderLimit)
	}
	if !slices.Equal(got, full[:wantProviderLimit]) {
		t.Fatalf("kept %v, want the first %d of %v", got, wantProviderLimit, full)
	}

	// When a trim happened the result must be a copy, so that writing through it
	// cannot reach the shared list. A build that does not trim returns the very
	// same slice, which is correct and nothing to assert against.
	if len(got) < len(full) && len(got) > 0 {
		got[0] = "mutated"
		if input[0] != full[0] {
			t.Fatalf("trimmed result aliases the input: input[0] = %q", input[0])
		}
	}

	// A list already at or under the cap comes back whole.
	short := []string{"only"}
	if kept := limitDefaultProviders(short); !slices.Equal(kept, short) {
		t.Fatalf("limited a single provider to %v, want %v", kept, short)
	}

	// Empty and nil must not panic. Nothing configured is a real case: the
	// simnet lists are a single local endpoint and a chain could define none.
	if kept := limitDefaultProviders(nil); len(kept) != 0 {
		t.Fatalf("limited nil to %v, want empty", kept)
	}
	if kept := limitDefaultProviders([]string{}); len(kept) != 0 {
		t.Fatalf("limited an empty list to %v, want empty", kept)
	}
}
