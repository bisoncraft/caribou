package intents

import "testing"

func TestCatalogIndexing(t *testing.T) {
	tokens := []*Token{
		// BTC under both ID schemes; nep141 must be preferred.
		{AssetID: "1cs_v1:btc:native:coin", Blockchain: "btc", Symbol: "BTC(OMNI)", Decimals: 8, ContractAddress: "coin"},
		{AssetID: "nep141:btc.omft.near", Blockchain: "btc", Symbol: "BTC", Decimals: 8},
		// NEAR native is wrap.near despite its contract address.
		{AssetID: "nep141:wrap.near", Blockchain: "near", Symbol: "wNEAR", Decimals: 24, ContractAddress: "wrap.near"},
		// A NEP-141 token on NEAR.
		{AssetID: "nep141:usdc.example.near", Blockchain: "near", Symbol: "USDC", Decimals: 6, ContractAddress: "usdc.example.near"},
		// Polygon native via the HOT bridge scheme: no contract address.
		{AssetID: "nep245:v2_1.omni.hot.tg:137_111", Blockchain: "pol", Symbol: "POL", Decimals: 18},
		// An SPL token with a base58 contract address.
		{AssetID: "nep141:sol-abc.omft.near", Blockchain: "sol", Symbol: "USDC", Decimals: 6, ContractAddress: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		// Unmapped chain: retained by asset ID, absent from chain indexes.
		{AssetID: "nep245:v2_1.omni.hot.tg:1117_x", Blockchain: "ton", Symbol: "TON", Decimals: 9},
	}
	c := newCatalog(tokens)

	if n := c.Native(0); n == nil || n.AssetID != "nep141:btc.omft.near" {
		t.Fatalf("BTC native = %+v; expected the nep141 representation", n)
	}
	if n := c.Native(397); n == nil || n.AssetID != "nep141:wrap.near" {
		t.Fatalf("NEAR native = %+v; expected wrap.near", n)
	}
	if n := c.Native(966); n == nil || n.AssetID != "nep245:v2_1.omni.hot.tg:137_111" {
		t.Fatalf("POL native = %+v", n)
	}

	// Contract lookup is case-insensitive.
	if tok := c.FindContract(501, "epjfwdd5aufqssqem2qn1xzybapc8g4weggkzwytdt1v"); tok == nil || tok.Symbol != "USDC" {
		t.Fatalf("SPL USDC contract lookup failed: %+v", tok)
	}
	if tok := c.FindContract(397, "usdc.example.near"); tok == nil {
		t.Fatalf("NEP-141 token contract lookup failed")
	}

	// Unmapped chains don't contribute chain entries but remain findable
	// by asset ID.
	if c.ByAssetID("nep245:v2_1.omni.hot.tg:1117_x") == nil {
		t.Fatalf("unmapped-chain token missing from asset ID index")
	}
	for _, id := range c.Chains() {
		if _, ok := chainBipIDs["ton"]; ok && id == chainBipIDs["ton"] {
			t.Fatalf("unmapped chain leaked into Chains()")
		}
	}
	// btc, near, and pol have native entries; sol contributes only a
	// token; ton is unmapped.
	if len(c.Chains()) != 3 {
		t.Fatalf("Chains() = %v", c.Chains())
	}
}

func TestChainBipID(t *testing.T) {
	for slug, want := range map[string]uint32{
		"btc": 0, "ltc": 2, "doge": 3, "dash": 5, "eth": 60, "zec": 133,
		"xrp": 144, "bch": 145, "near": 397, "sol": 501, "bsc": 714,
		"pol": 966, "base": 8453, "hypercore": 10001,
	} {
		got, ok := ChainBipID(slug)
		if !ok || got != want {
			t.Errorf("ChainBipID(%q) = %d, %v; want %d", slug, got, ok, want)
		}
	}
	if _, ok := ChainBipID("tron"); ok {
		t.Errorf("tron unexpectedly mapped")
	}
}
