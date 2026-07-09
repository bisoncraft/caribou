// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dash

import (
	"strings"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
)

// TestDashSPVChainConfig confirms the SPV parameterization selects the neutrino
// backend with a non-zero recovery window (which drives the compact-filter
// scan) and that regtest neutrino seeds are present.
func TestDashSPVChainConfig(t *testing.T) {
	cc := dashSPVChainConfig(RegressionNetParams)
	if cc.Chain != bisonwire.ChainDASH {
		t.Fatalf("chain = %q, want %q", cc.Chain, bisonwire.ChainDASH)
	}
	if cc.BackendKind != wallet.BackendNeutrino {
		t.Fatalf("BackendKind = %v, want neutrino", cc.BackendKind)
	}
	if cc.RecoveryWindow == 0 {
		t.Fatal("SPV RecoveryWindow should be >0 (drives the neutrino filter scan)")
	}
	if len(cc.NetParams.KeyPurposes) != 1 || cc.NetParams.KeyPurposes[0] != 44 {
		t.Fatalf("KeyPurposes = %v, want [44] (legacy P2PKH)", cc.NetParams.KeyPurposes)
	}
	if len(cc.NetParams.NeutrinoSeeds) == 0 {
		t.Fatal("regtest has no neutrino seeds")
	}
}

// TestDashTableNetParams verifies each clone *chaincfg.Params maps to the
// correct assets-table netparams entry.
func TestDashTableNetParams(t *testing.T) {
	cases := []struct {
		clone    *chaincfg.Params
		wantName string
	}{
		{MainNetParams, "mainnet"},
		{TestNetParams, "testnet3"},
		{RegressionNetParams, "regtest"},
	}
	for _, c := range cases {
		np := dashTableNetParams(c.clone)
		if np == nil {
			t.Fatalf("%s: nil netparams", c.clone.Name)
		}
		if np.Name != c.wantName {
			t.Fatalf("%s mapped to %q, want %q", c.clone.Name, np.Name, c.wantName)
		}
		if np.Chain != "dash" {
			t.Fatalf("%s: chain %q, want dash", c.clone.Name, np.Chain)
		}
	}
}

// TestDashElectrumChainConfig checks the shared-wallet config Dash supplies:
// the bisonwire chain id, the legacy address purpose (no SegWit), and that
// built-in electrum servers are present where expected.
func TestDashElectrumChainConfig(t *testing.T) {
	cc := dashElectrumChainConfig(MainNetParams)
	if cc.Chain != bisonwire.ChainDASH {
		t.Fatalf("chain = %q, want %q", cc.Chain, bisonwire.ChainDASH)
	}
	if len(cc.NetParams.KeyPurposes) != 1 || cc.NetParams.KeyPurposes[0] != 44 {
		t.Fatalf("KeyPurposes = %v, want [44] (legacy P2PKH, no SegWit)", cc.NetParams.KeyPurposes)
	}
	if cc.RecoveryWindow == 0 {
		t.Fatal("electrum RecoveryWindow should be >0 (drives the gap-limit restore scan)")
	}
	if cc.NetParams.Bech32HRPSegwit != "" {
		t.Fatalf("unexpected bech32 HRP %q", cc.NetParams.Bech32HRPSegwit)
	}
	if len(cc.NetParams.ElectrumServers) == 0 {
		t.Fatal("mainnet has no built-in electrum servers")
	}
	if n := len(dashElectrumChainConfig(RegressionNetParams).NetParams.ElectrumServers); n == 0 {
		t.Fatal("regtest has no built-in electrum server (harness)")
	}
}

// TestDashCloneAssetsConsistency guards against the clone params (params.go,
// used by the RPC wallet and for address decode/validate) drifting from the
// assets-table params (which drive the native wallet's address derivation) on
// the address-encoding magics. These MUST agree, or the native wallet would
// derive addresses under one prefix and validate them under another.
// (HDCoinType legitimately differs — the clone leaves it 0 like LTC, while the
// assets entry sets the real coin type for BIP-44 derivation.)
func TestDashCloneAssetsConsistency(t *testing.T) {
	for _, c := range []*chaincfg.Params{MainNetParams, TestNetParams, RegressionNetParams} {
		np := dashTableNetParams(c)
		if c.PubKeyHashAddrID != np.PubKeyHashAddrID {
			t.Fatalf("%s PubKeyHashAddrID: clone 0x%02x, assets 0x%02x", c.Name, c.PubKeyHashAddrID, np.PubKeyHashAddrID)
		}
		if c.ScriptHashAddrID != np.ScriptHashAddrID {
			t.Fatalf("%s ScriptHashAddrID: clone 0x%02x, assets 0x%02x", c.Name, c.ScriptHashAddrID, np.ScriptHashAddrID)
		}
	}
}

// TestDashP2PKHAddressPrefix confirms the chain params produce Dash-style
// legacy P2PKH addresses: mainnet starts with 'X', testnet/regtest with 'y'.
// The native wallet derives BIP-44 P2PKH addresses (KeyPurposes {44}), so these
// are the address forms users will see.
func TestDashP2PKHAddressPrefix(t *testing.T) {
	pkHash := make([]byte, 20) // all-zero hash is fine for a prefix check
	cases := []struct {
		params *chaincfg.Params
		prefix string
	}{
		{MainNetParams, "X"},
		{TestNetParams, "y"},
		{RegressionNetParams, "y"},
	}
	for _, c := range cases {
		addr, err := btcutil.NewAddressPubKeyHash(pkHash, c.params)
		if err != nil {
			t.Fatalf("%s: %v", c.params.Name, err)
		}
		if got := addr.EncodeAddress(); !strings.HasPrefix(got, c.prefix) {
			t.Fatalf("%s address %q does not start with %q", c.params.Name, got, c.prefix)
		}
	}
}
