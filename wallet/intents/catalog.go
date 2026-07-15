// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package intents

import (
	"strings"
	"time"
)

// chainBipIDs maps the service's blockchain slugs to the wallet's BIP-44
// asset IDs, for chains this codebase implements (or is expected to). The
// service supports additional chains (arb, op, avax, ton, tron, sui, …);
// catalog entries for unmapped chains are retained but report no chain ID,
// and the eligible pair set is the runtime intersection of enabled wallets
// with mapped catalog entries.
var chainBipIDs = map[string]uint32{
	"btc":       0,
	"ltc":       2,
	"doge":      3,
	"dash":      5,
	"eth":       60,
	"zec":       133,
	"xrp":       144,
	"bch":       145,
	"near":      397,
	"sol":       501,
	"bsc":       714,   // BNB Smart Chain
	"pol":       966,   // Polygon
	"base":      8453,  // matches base.BaseBipID
	"hypercore": 10001, // matches hyperliquid.BipID
}

// ChainBipID resolves the service's chain slug to the wallet's chain asset
// ID.
func ChainBipID(blockchain string) (uint32, bool) {
	id, ok := chainBipIDs[blockchain]
	return id, ok
}

// nearNativeAssetID is the Intents-side representation of native NEAR.
// The verifier holds only tokens, so NEAR itself trades as wrapped NEAR.
const nearNativeAssetID = "nep141:wrap.near"

// isNative reports whether a catalog token represents its chain's native
// asset. Most natives carry no contract address; the newer "1cs_v1" asset
// ID scheme marks natives explicitly (with contractAddress "coin"); NEAR
// is special-cased to wrap.near.
func isNative(t *Token) bool {
	if t.Blockchain == "near" {
		return t.AssetID == nearNativeAssetID
	}
	if strings.Contains(t.AssetID, ":native:") {
		return true
	}
	return t.ContractAddress == "" || t.ContractAddress == "coin"
}

// assetIDPreference ranks duplicate representations of the same asset.
// Some assets are listed under both the established "nep141:" scheme and
// the newer "1cs_v1:" scheme; the service normalizes between them, and
// the nep141 form is preferred for requests.
func assetIDPreference(assetID string) int {
	switch {
	case strings.HasPrefix(assetID, "nep141:"):
		return 0
	case strings.HasPrefix(assetID, "nep245:"):
		return 1
	default:
		return 2
	}
}

type chainContract struct {
	chainID  uint32
	contract string
}

// Catalog is an indexed view of the service's supported assets.
type Catalog struct {
	Tokens    []*Token
	FetchedAt time.Time

	byAssetID  map[string]*Token
	natives    map[uint32]*Token
	byContract map[chainContract]*Token
}

func newCatalog(tokens []*Token) *Catalog {
	c := &Catalog{
		Tokens:     tokens,
		FetchedAt:  time.Now(),
		byAssetID:  make(map[string]*Token, len(tokens)),
		natives:    make(map[uint32]*Token),
		byContract: make(map[chainContract]*Token),
	}
	for _, t := range tokens {
		c.byAssetID[t.AssetID] = t

		chainID, ok := ChainBipID(t.Blockchain)
		if !ok {
			continue
		}
		if isNative(t) {
			if cur, exists := c.natives[chainID]; !exists ||
				assetIDPreference(t.AssetID) < assetIDPreference(cur.AssetID) {
				c.natives[chainID] = t
			}
			continue
		}
		key := chainContract{chainID, strings.ToLower(t.ContractAddress)}
		if cur, exists := c.byContract[key]; !exists ||
			assetIDPreference(t.AssetID) < assetIDPreference(cur.AssetID) {
			c.byContract[key] = t
		}
	}
	return c
}

// ByAssetID looks up a token by its Intents asset ID.
func (c *Catalog) ByAssetID(assetID string) *Token {
	return c.byAssetID[assetID]
}

// Native returns the preferred catalog entry for a chain's native asset,
// or nil if the chain is unmapped or unsupported.
func (c *Catalog) Native(chainBipID uint32) *Token {
	return c.natives[chainBipID]
}

// FindContract returns the catalog entry for a token contract on a chain,
// or nil. Contract addresses match case-insensitively.
func (c *Catalog) FindContract(chainBipID uint32, contractAddress string) *Token {
	return c.byContract[chainContract{chainBipID, strings.ToLower(contractAddress)}]
}

// Chains returns the mapped chain asset IDs with at least a native entry
// in the catalog.
func (c *Catalog) Chains() []uint32 {
	ids := make([]uint32, 0, len(c.natives))
	for id := range c.natives {
		ids = append(ids, id)
	}
	return ids
}
