package mnemonic

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// mnemonicVectors are fixed encodings produced by this package. TestEncodeDecode
// only checks that generateMnemonic and DecodeMnemonic agree with each other, so
// a change to the bit packing that broke both symmetrically would still pass it
// while making every seed already issued to a user unrecoverable. These vectors
// pin the on-the-wire encoding so such a change fails loudly.
var mnemonicVectors = []struct {
	entropy  string
	stamp    string // time encoded, always midnight UTC
	mnemonic string
}{{
	entropy:  "000102030405060708090a0b0c0d0e0f1011",
	stamp:    "2024-01-15T00:00:00Z",
	mnemonic: "abandon amount liar amount expire adjust cage candy arch gather drum bullet absurd pledge gun",
}, {
	// Minimum entropy and timestamp.
	entropy:  "000000000000000000000000000000000000",
	stamp:    "1970-01-01T00:00:00Z",
	mnemonic: "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon address",
}, {
	// Maximum entropy, and the largest day count a uint16 can hold.
	entropy:  "ffffffffffffffffffffffffffffffffffff",
	stamp:    "2149-06-06T00:00:00Z",
	mnemonic: "zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrist",
}, {
	entropy:  "5a3f01c9e27b48d6aa10ff3e9c7d2b845e60",
	stamp:    "2026-07-29T00:00:00Z",
	mnemonic: "focus way include shaft region hero portion cable direct token enough anger toward choose that",
}}

func TestMnemonicVectors(t *testing.T) {
	for _, v := range mnemonicVectors {
		entropy, err := hex.DecodeString(v.entropy)
		if err != nil {
			t.Fatalf("bad test entropy %q: %v", v.entropy, err)
		}
		stamp, err := time.Parse(time.RFC3339, v.stamp)
		if err != nil {
			t.Fatalf("bad test stamp %q: %v", v.stamp, err)
		}

		got, err := GenerateMnemonic(entropy, stamp)
		if err != nil {
			t.Fatalf("GenerateMnemonic error: %v", err)
		}
		if got != v.mnemonic {
			t.Fatalf("encoding changed for entropy %s\n got: %q\nwant: %q", v.entropy, got, v.mnemonic)
		}

		reEntropy, reStamp, err := DecodeMnemonic(v.mnemonic)
		if err != nil {
			t.Fatalf("DecodeMnemonic error: %v", err)
		}
		if !bytes.Equal(reEntropy, entropy) {
			t.Fatalf("decoded entropy %x, want %s", reEntropy, v.entropy)
		}
		if !reStamp.UTC().Equal(stamp) {
			t.Fatalf("decoded stamp %s, want %s", reStamp.UTC(), stamp)
		}
	}
}

func TestGenerateMnemonicBadEntropy(t *testing.T) {
	for _, n := range []int{0, entropyBytes - 1, entropyBytes + 1} {
		if _, err := GenerateMnemonic(make([]byte, n), time.Now()); err == nil {
			t.Fatalf("no error for %d-byte entropy", n)
		}
	}
}

// TestDecodeMnemonicErrors covers the paths that protect a user who mistypes a
// seed. Silently accepting a corrupted seed would hand back the wrong entropy
// and, with it, an empty wallet the user cannot distinguish from a lost one.
func TestDecodeMnemonicErrors(t *testing.T) {
	valid := mnemonicVectors[0].mnemonic
	words := strings.Fields(valid)

	tests := []struct {
		name     string
		mnemonic string
	}{
		{"empty", ""},
		{"too few words", strings.Join(words[:seedWords-1], " ")},
		{"too many words", valid + " gun"},
		{"unknown word", strings.Join(append([]string{"notaword"}, words[1:]...), " ")},
		// Word matching is case-sensitive, so a seed pasted or typed with any
		// capitalization is rejected rather than normalized. Recorded here as
		// current behavior; whether restore should lower-case its input first is
		// a separate decision.
		{"uppercased", strings.ToUpper(valid)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := DecodeMnemonic(tt.mnemonic); err == nil {
				t.Fatalf("no error for %s", tt.name)
			}
		})
	}

	// Substituting a single valid word corrupts the entropy, which the checksum
	// must catch.
	for i := range words {
		bad := make([]string, len(words))
		copy(bad, words)
		if bad[i] == "abandon" {
			bad[i] = "ability"
		} else {
			bad[i] = "abandon"
		}
		if _, _, err := DecodeMnemonic(strings.Join(bad, " ")); err == nil {
			t.Fatalf("no checksum error substituting word %d (%q -> %q)", i, words[i], bad[i])
		}
	}
}

func TestFindWordIndex(t *testing.T) {
	for i := range wordList {
		j, err := wordIndex(wordList[i])
		if err != nil {
			t.Fatal(err)
		}
		if i != int(j) {
			t.Fatalf("wrong index %d returned for %q. expected %d", j, wordList[i], i)
		}
	}

	if _, err := wordIndex("blah"); err == nil {
		t.Fatal("no error for blah")
	}

	if _, err := wordIndex("aaa"); err == nil {
		t.Fatal("no error for aaa")
	}

	if _, err := wordIndex("zzz"); err == nil {
		t.Fatal("no error for zzz")
	}
}

func TestEncodeDecode(t *testing.T) {
	for i := 0; i < 1000; i++ {
		ogEntropy, mnemonic := New()
		reEntropy, stamp, err := DecodeMnemonic(mnemonic)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(reEntropy, ogEntropy) {
			t.Fatal("failed to recover entropy")
		}
		if stamp.Unix()/secondsPerDay != time.Now().Unix()/secondsPerDay {
			t.Fatalf("time not recovered")
		}
	}
}
