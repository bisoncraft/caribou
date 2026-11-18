package rhea

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeployment(t *testing.T) {
	for _, s := range []string{"rhea001594.multica.near", "rhea000001.multica.near"} {
		if !Mainnet.IsMCA(s) {
			t.Fatalf("%s not an MCA", s)
		}
	}
	for _, s := range []string{"multica.near", ".multica.near", "a.b.multica.near", "rhea001594.multica.near.evil", "RHEA1.multica.near", "rhea001594-multica.near"} {
		if Mainnet.IsMCA(s) {
			t.Fatalf("%s accepted as an MCA", s)
		}
	}
	for _, m := range Mainnet.Markets {
		if Mainnet.Market(m.TokenID) != m {
			t.Fatalf("market %s not found", m.TokenID)
		}
	}
	if Mainnet.Market("eth.bridge.near") != nil {
		t.Fatalf("bridged market allowlisted")
	}
	for s, want := range map[string]bool{
		"a.near": true, "ab": true, "a": false, "a..b": false, ".a": false, "a.": false, "A.near": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false, "a_b-c.near": true, "a b": false,
	} {
		if validAccountID(s) != want {
			t.Fatalf("validAccountID(%q) != %t", s, want)
		}
	}
}

// TestMarketsMatchCatalog checks the allowlist's 1Click asset IDs against
// the captured 1Click catalog: each must be listed on the NEAR chain with
// the market token as its contract.
func TestMarketsMatchCatalog(t *testing.T) {
	var cat []struct {
		AssetID         string `json:"assetId"`
		Blockchain      string `json:"blockchain"`
		ContractAddress string `json:"contractAddress"`
	}
	if err := json.Unmarshal(readTestdata(t, "oneclick-tokens.json"), &cat); err != nil {
		t.Fatal(err)
	}
	for _, m := range Mainnet.Markets {
		var found bool
		for _, tok := range cat {
			if tok.AssetID == m.OneClickAssetID {
				found = true
				if tok.Blockchain != "near" || tok.ContractAddress != m.TokenID {
					t.Fatalf("%s: catalog lists %s on %s with contract %q", m.Symbol, tok.AssetID, tok.Blockchain, tok.ContractAddress)
				}
			}
		}
		if !found {
			t.Fatalf("%s: %s not in the catalog", m.Symbol, m.OneClickAssetID)
		}
	}
}

// TestMarketFiatTickers checks that every market names the ticker whose
// USD rate prices it, the same ticker for a symbol in every deployment.
func TestMarketFiatTickers(t *testing.T) {
	want := map[string]string{"USDC": "USDC", "USDT": "USDT", "NEAR": "NEAR", "ZEC": "ZEC"}
	for _, dep := range []*Deployment{Mainnet, Simnet} {
		for _, m := range dep.Markets {
			if w, ok := want[m.Symbol]; !ok || m.FiatTicker != w {
				t.Errorf("%s (%s): fiat ticker %q, want %q", m.Symbol, m.TokenID, m.FiatTicker, w)
			}
		}
	}
}

func TestCheckTemplates(t *testing.T) {
	raw := viewResult(t, "multica.near", "get_reliable_business_template_paged")
	if err := Mainnet.CheckTemplates(raw); err != nil {
		t.Fatalf("captured templates rejected: %v", err)
	}
	var templates map[string]string
	if err := json.Unmarshal(raw, &templates); err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(map[string]string)) []byte {
		m := make(map[string]string, len(templates))
		for k, v := range templates {
			m[k] = v
		}
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	for name, f := range map[string]func(map[string]string){
		"supply as collateral": func(m map[string]string) { m[TemplateBurrowSupply] = m["BurrowCollateral"] },
		"create as collateral": func(m map[string]string) { m[TemplateBurrowRegisterAndSupply] = m["BurrowRegisterAndCollateral"] },
		"supply missing":       func(m map[string]string) { delete(m, TemplateBurrowSupply) },
		"create missing":       func(m map[string]string) { delete(m, TemplateBurrowRegisterAndSupply) },
		"gas changed": func(m map[string]string) {
			m[TemplateBurrowSupply] = strings.Replace(m[TemplateBurrowSupply], "50000000000000", "50000000000001", 1)
		},
		"receiver changed": func(m map[string]string) {
			m[TemplateBurrowSupply] = strings.Replace(m[TemplateBurrowSupply], "contract.main.burrow.near", "evil.near", 1)
		},
		"whitespace": func(m map[string]string) { m[TemplateBurrowSupply] = " " + m[TemplateBurrowSupply] },
	} {
		if err := Mainnet.CheckTemplates(mutate(f)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := Mainnet.CheckTemplates([]byte(`[]`)); err == nil {
		t.Errorf("non-object accepted")
	}
	// Other deployments substitute their Burrow account.
	other := *Mainnet
	other.Burrow = "burrow.test.near"
	if err := other.CheckTemplates(raw); err == nil {
		t.Errorf("mainnet templates accepted for another Burrow account")
	}
	if tpl, _ := other.PinnedTemplate(TemplateBurrowSupply); !strings.Contains(tpl, "burrow.test.near") {
		t.Errorf("pinned template not rewritten: %s", tpl)
	}
}
