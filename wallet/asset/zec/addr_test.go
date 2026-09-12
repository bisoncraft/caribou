package zec

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
)

func TestAddress(t *testing.T) {
	pkHash, _ := hex.DecodeString("0ca584c97d2c84ea296524ac89f2febcf1094347")
	addr := "t1K2UQ5VzGHGC1ZPqGJXXSocxtjo5s6peSJ"

	btcAddr, err := DecodeAddress(addr, MainNetAddressParams, MainNetParams)
	if err != nil {
		t.Fatalf("DecodeAddress error: %v", err)
	}

	if !bytes.Equal(btcAddr.ScriptAddress(), pkHash) {
		t.Fatalf("wrong script address")
	}

	reAddr, err := RecodeAddress(btcAddr.String(), MainNetAddressParams, MainNetParams)
	if err != nil {
		t.Fatalf("RecodeAddress error: %v", err)
	}

	if reAddr != addr {
		t.Fatalf("wrong recoded address. expected %s, got %s", addr, reAddr)
	}
}

func TestEncodeDecodeAddressScriptHash(t *testing.T) {
	scriptHash, _ := hex.DecodeString("0ca584c97d2c84ea296524ac89f2febcf1094347")
	btcAddr, err := btcutil.NewAddressScriptHashFromHash(scriptHash, MainNetParams)
	if err != nil {
		t.Fatalf("NewAddressScriptHashFromHash error: %v", err)
	}

	encoded, err := EncodeAddress(btcAddr, MainNetAddressParams)
	if err != nil {
		t.Fatalf("EncodeAddress error: %v", err)
	}

	decoded, err := DecodeAddress(encoded, MainNetAddressParams, MainNetParams)
	if err != nil {
		t.Fatalf("DecodeAddress error: %v", err)
	}
	if !bytes.Equal(decoded.ScriptAddress(), scriptHash) {
		t.Fatalf("round-tripped script hash does not match")
	}
}

func TestEncodeAddressUnsupportedType(t *testing.T) {
	_, pub := btcec.PrivKeyFromBytes([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32})
	pkAddr, err := btcutil.NewAddressPubKey(pub.SerializeCompressed(), MainNetParams)
	if err != nil {
		t.Fatalf("NewAddressPubKey error: %v", err)
	}
	if _, err := EncodeAddress(pkAddr, MainNetAddressParams); err == nil {
		t.Fatalf("expected error encoding unsupported address type")
	}
}

func TestDecodeAddressErrors(t *testing.T) {
	if _, err := DecodeAddress("1", MainNetAddressParams, MainNetParams); err == nil {
		t.Fatalf("expected error decoding too-short address")
	}

	// Valid length and structure, but corrupted checksum.
	addr := "t1K2UQ5VzGHGC1ZPqGJXXSocxtjo5s6peSJ"
	if _, err := DecodeAddress(addr[:len(addr)-1]+"1", MainNetAddressParams, MainNetParams); err == nil {
		t.Fatalf("expected error decoding address with bad checksum")
	}

	// Unknown address ID: use testnet params against a mainnet address.
	if _, err := DecodeAddress(addr, TestNet4AddressParams, MainNetParams); err == nil {
		t.Fatalf("expected error decoding address with unknown addr ID")
	}
}
