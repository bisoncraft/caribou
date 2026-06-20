// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package hyperliquid

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/math"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/bisoncraft/bosswallet/util"
)

// Fixed inputs for the golden SendAsset vector. The expected values are frozen
// regression anchors AND are independently reproduced below by go-ethereum's
// general EIP-712 engine (apitypes), so this test catches both accidental drift
// and any disagreement between the hand-rolled encoding and a standard EIP-712
// implementation (type string, field order, domain).
const (
	goldenDest   = "0x000000000000000000000000000000000000dEaD"
	goldenToken  = "HYPE:0x0000000000000000000000000000000000000000000000000000000000000000"
	goldenAmount = "5"
	goldenNonce  = uint64(1700000000000)
	// goldenPrivKey is an arbitrary fixed secp256k1 key (32 bytes of 0x01).
	goldenPrivKey = "0101010101010101010101010101010101010101010101010101010101010101"

	goldenSigner = "0x1a642f0e3c3af545e7acbd38b07251b3990914f1"
	goldenDigest = "d91d56f9ea5f2ccbab1cd9a2761de50f545c1d45925227b4c6621c597d5860af"
	goldenSig    = "8f92a44efe7894872cd928b1d9c019e3fa9f9e2f1208a1c513d67187570bde6013ffd7e8215e07a6ca159fe06cdfd3ef811fb5f02dc42b317db3e6cd95c194991c"
)

// apitypesSendAssetDigest recomputes the SendAsset EIP-712 digest using
// go-ethereum's general-purpose typed-data engine — an implementation entirely
// independent of the wallet's hand-rolled byte concatenation.
func apitypesSendAssetDigest(t *testing.T, chainID uint64, chainName, dest, token, amount string, nonce uint64) []byte {
	t.Helper()
	td := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"HyperliquidTransaction:SendAsset": {
				{Name: "hyperliquidChain", Type: "string"},
				{Name: "destination", Type: "string"},
				{Name: "sourceDex", Type: "string"},
				{Name: "destinationDex", Type: "string"},
				{Name: "token", Type: "string"},
				{Name: "amount", Type: "string"},
				{Name: "fromSubAccount", Type: "string"},
				{Name: "nonce", Type: "uint64"},
			},
		},
		PrimaryType: "HyperliquidTransaction:SendAsset",
		Domain: apitypes.TypedDataDomain{
			Name:              "HyperliquidSignTransaction",
			Version:           "1",
			ChainId:           (*math.HexOrDecimal256)(new(big.Int).SetUint64(chainID)),
			VerifyingContract: "0x0000000000000000000000000000000000000000",
		},
		Message: apitypes.TypedDataMessage{
			"hyperliquidChain": chainName,
			"destination":      dest,
			"sourceDex":        "spot",
			"destinationDex":   "spot",
			"token":            token,
			"amount":           amount,
			"fromSubAccount":   "",
			"nonce":            new(big.Int).SetUint64(nonce),
		},
	}
	domSep, err := td.HashStruct("EIP712Domain", td.Domain.Map())
	if err != nil {
		t.Fatalf("domain hash: %v", err)
	}
	structHash, err := td.HashStruct(td.PrimaryType, td.Message)
	if err != nil {
		t.Fatalf("struct hash: %v", err)
	}
	raw := append([]byte{0x19, 0x01}, append(domSep, structHash...)...)
	return gethcrypto.Keccak256(raw)
}

// TestSendAssetGoldenVector pins the SendAsset signing path to a known vector and
// cross-checks the wallet's digest against an independent EIP-712 engine.
func TestSendAssetGoldenVector(t *testing.T) {
	walletDigest := sendAssetEIP712Hash(util.Testnet, goldenDest, "spot", "spot", goldenToken, goldenAmount, "", goldenNonce)

	// 1. Frozen digest (regression anchor).
	if got := hex.EncodeToString(walletDigest); got != goldenDigest {
		t.Fatalf("digest drift: got %s, want %s", got, goldenDigest)
	}

	// 2. Independent reconstruction with go-ethereum apitypes (ground truth that
	//    the hand-rolled domain/type encoding is correct, not just stable).
	indep := apitypesSendAssetDigest(t, eip712ChainIDs[util.Testnet], eip712ChainNames[util.Testnet],
		goldenDest, goldenToken, goldenAmount, goldenNonce)
	if hex.EncodeToString(indep) != goldenDigest {
		t.Fatalf("apitypes digest %s != golden %s — hand-rolled construction disagrees with a general EIP-712 engine",
			hex.EncodeToString(indep), goldenDigest)
	}

	// 3. Frozen signature for the fixed key, and recover the signer to confirm
	//    the V normalization (27/28) and that the right address comes back.
	kb, err := hex.DecodeString(goldenPrivKey)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := gethcrypto.ToECDSA(kb)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signHash(priv, walletDigest)
	if err != nil {
		t.Fatalf("signHash: %v", err)
	}
	if got := hex.EncodeToString(sig[:]); got != goldenSig {
		t.Fatalf("signature drift: got %s, want %s", got, goldenSig)
	}

	// recover: geth wants V in {0,1}; the wallet emits {27,28}.
	rsv := make([]byte, 65)
	copy(rsv, sig[:64])
	rsv[64] = sig[64] - 27
	pub, err := gethcrypto.SigToPub(walletDigest, rsv)
	if err != nil {
		t.Fatalf("SigToPub: %v", err)
	}
	if got := strings.ToLower(gethcrypto.PubkeyToAddress(*pub).Hex()); got != goldenSigner {
		t.Fatalf("recovered signer %s, want %s", got, goldenSigner)
	}
}
