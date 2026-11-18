package app

import (
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/rhea"
)

func TestCoreRheaTest(t *testing.T) {
	cfg := &Config{}
	cfg.IntentsAPI = "http://127.0.0.1:17781/"
	cfg.Net = util.Simnet
	rt := cfg.Core(nil).RheaTest
	if rt == nil || rt.Deployment.AccountManager != rhea.Simnet.AccountManager ||
		rt.Deployment.RelayerAPI != "http://127.0.0.1:17781/rhea/relayer" ||
		len(rt.RPCEndpoints) != 1 || rt.RPCEndpoints[0] != "http://127.0.0.1:17781/rhea/rpc" {
		t.Fatalf("simnet lending config %+v", rt)
	}
	// The shared deployment is not modified.
	if rhea.Simnet.RelayerAPI != "http://127.0.0.1:17781/rhea/relayer" {
		t.Fatalf("rhea.Simnet modified")
	}
	cfg.Net = util.Mainnet
	if cfg.Core(nil).RheaTest != nil {
		t.Fatalf("lending test config on mainnet")
	}
}
