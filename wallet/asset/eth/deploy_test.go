// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"bytes"
	"testing"

	erc20v0 "github.com/bisoncraft/bosswallet/wallet/asset/erc20/contracts/v0"
	ethv0 "github.com/bisoncraft/bosswallet/wallet/asset/eth/contracts/v0"
	ethv1 "github.com/bisoncraft/bosswallet/wallet/asset/eth/contracts/v1"
	"github.com/ethereum/go-ethereum/common"
)

func TestContractDeployerTxData(t *testing.T) {
	tokenAddr := common.HexToAddress("0xdac17f958d2ee523a2206206994597c13d831ec7")

	t.Run("base v0 is ETHSwap v0 bytecode", func(t *testing.T) {
		data, err := ContractDeployer.TxData(0, common.Address{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, common.FromHex(ethv0.ETHSwapBin)) {
			t.Error("base v0 txData != ETHSwap v0 bytecode")
		}
	})

	t.Run("base v1 is ETHSwap v1 bytecode", func(t *testing.T) {
		data, err := ContractDeployer.TxData(1, common.Address{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, common.FromHex(ethv1.ETHSwapBin)) {
			t.Error("base v1 txData != ETHSwap v1 bytecode")
		}
	})

	t.Run("base unknown version errors", func(t *testing.T) {
		if _, err := ContractDeployer.TxData(2, common.Address{}); err == nil {
			t.Error("expected error for unknown base contract version")
		}
	})

	t.Run("token v0 appends abi-packed address", func(t *testing.T) {
		data, err := ContractDeployer.TxData(0, tokenAddr)
		if err != nil {
			t.Fatal(err)
		}
		bytecode := common.FromHex(erc20v0.ERC20SwapBin)
		if !bytes.HasPrefix(data, bytecode) {
			t.Fatal("token txData does not start with the ERC20Swap bytecode")
		}
		// The constructor arg is a single address, ABI-encoded as a 32-byte
		// left-padded word appended to the bytecode.
		arg := data[len(bytecode):]
		if len(arg) != 32 {
			t.Fatalf("appended arg length = %d, want 32", len(arg))
		}
		if !bytes.Equal(arg[12:], tokenAddr.Bytes()) {
			t.Errorf("arg trailing bytes = %x, want %x", arg[12:], tokenAddr.Bytes())
		}
		if !bytes.Equal(arg[:12], make([]byte, 12)) {
			t.Errorf("arg not left-padded with zeros: %x", arg[:12])
		}
	})

	t.Run("token v1 unsupported errors", func(t *testing.T) {
		if _, err := ContractDeployer.TxData(1, tokenAddr); err == nil {
			t.Error("expected error for token contract version 1")
		}
	})
}

func TestBuildDeployTxData(t *testing.T) {
	t.Run("empty address uses base contract", func(t *testing.T) {
		data, err := BuildDeployTxData(0, "")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, common.FromHex(ethv0.ETHSwapBin)) {
			t.Error("empty-address v0 txData != base ETHSwap bytecode")
		}
	})

	t.Run("valid token address uses token contract", func(t *testing.T) {
		data, err := BuildDeployTxData(0, "0xdac17f958d2ee523a2206206994597c13d831ec7")
		if err != nil {
			t.Fatal(err)
		}
		// Token path = ERC20 bytecode + a 32-byte packed address.
		if len(data) != len(common.FromHex(erc20v0.ERC20SwapBin))+32 {
			t.Errorf("token txData length = %d, want bytecode + 32", len(data))
		}
	})

	t.Run("invalid token address errors", func(t *testing.T) {
		if _, err := BuildDeployTxData(0, "not-an-address"); err == nil {
			t.Error("expected error for invalid token address")
		}
	})

	t.Run("unknown version errors", func(t *testing.T) {
		if _, err := BuildDeployTxData(9, ""); err == nil {
			t.Error("expected error for unknown contract version")
		}
	})
}
