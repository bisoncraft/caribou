// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// stubContractBackend is a bind.ContractBackend whose only live method is
// CallContract. The embedded interface is nil; any other method call would
// panic, which is fine because L1FeeForCalldata only calls CallContract.
type stubContractBackend struct {
	bind.ContractBackend
	callResult []byte
	callErr    error
	gotCall    ethereum.CallMsg
}

func (s *stubContractBackend) CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error) {
	s.gotCall = call
	return s.callResult, s.callErr
}

func TestWrapCalldata(t *testing.T) {
	t.Parallel()
	calldata := []byte{0xde, 0xad, 0xbe, 0xef}
	raw, err := WrapCalldata(calldata)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("WrapCalldata returned empty bytes")
	}

	// The wrapper must round-trip back to a DynamicFeeTx carrying the original
	// calldata and the documented zero envelope fields.
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatalf("result is not a valid serialized tx: %v", err)
	}
	if tx.Type() != types.DynamicFeeTxType {
		t.Errorf("tx type = %d, want DynamicFeeTx (%d)", tx.Type(), types.DynamicFeeTxType)
	}
	if !bytes.Equal(tx.Data(), calldata) {
		t.Errorf("tx data = %x, want %x", tx.Data(), calldata)
	}
	if tx.ChainId().Sign() != 0 {
		t.Errorf("chainID = %s, want 0", tx.ChainId())
	}
	if tx.Value().Sign() != 0 {
		t.Errorf("value = %s, want 0", tx.Value())
	}
	if to := tx.To(); to == nil || (*to != common.Address{}) {
		t.Errorf("to = %v, want zero address", to)
	}
}

func TestWrapCalldataEmpty(t *testing.T) {
	t.Parallel()
	raw, err := WrapCalldata(nil)
	if err != nil {
		t.Fatal(err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatalf("result is not a valid serialized tx: %v", err)
	}
	if len(tx.Data()) != 0 {
		t.Errorf("data = %x, want empty", tx.Data())
	}
}

func TestL1FeeForCalldata(t *testing.T) {
	t.Parallel()
	calldata := []byte{0x01, 0x02, 0x03}

	t.Run("returns decoded fee", func(t *testing.T) {
		t.Parallel()
		want := big.NewInt(123456789)
		// Encode the fee exactly as the precompile would return it.
		packed, err := gasPriceOracleABI.Methods["getL1Fee"].Outputs.Pack(want)
		if err != nil {
			t.Fatal(err)
		}
		cb := &stubContractBackend{callResult: packed}

		got, err := L1FeeForCalldata(context.Background(), cb, calldata)
		if err != nil {
			t.Fatal(err)
		}
		if got.Cmp(want) != 0 {
			t.Errorf("fee = %s, want %s", got, want)
		}
		// The call must target the GasPriceOracle precompile.
		if cb.gotCall.To == nil || *cb.gotCall.To != GasPriceOracleAddr {
			t.Errorf("call.To = %v, want %s", cb.gotCall.To, GasPriceOracleAddr)
		}
	})

	t.Run("call error propagates", func(t *testing.T) {
		t.Parallel()
		cb := &stubContractBackend{callErr: errors.New("rpc down")}
		if _, err := L1FeeForCalldata(context.Background(), cb, calldata); err == nil {
			t.Error("expected error when CallContract fails")
		}
	})

	t.Run("undecodable result errors", func(t *testing.T) {
		t.Parallel()
		// Too few bytes to unpack a uint256.
		cb := &stubContractBackend{callResult: []byte{0x01}}
		if _, err := L1FeeForCalldata(context.Background(), cb, calldata); err == nil {
			t.Error("expected error for malformed result")
		}
	})

	t.Run("empty result errors", func(t *testing.T) {
		t.Parallel()
		cb := &stubContractBackend{callResult: []byte{}}
		if _, err := L1FeeForCalldata(context.Background(), cb, calldata); err == nil {
			t.Error("expected error for empty result")
		}
	})
}
