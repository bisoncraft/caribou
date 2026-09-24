//go:build !harness && !rpclive

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"math/big"
	"testing"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/erc20"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// newTransferLog builds a real ABI-encoded Transfer(address,address,uint256)
// log, the same shape a live node would return, so parseTransfer is exercised
// through the real decode path (erc20.IERC20.ParseTransfer), not a stub.
func newTransferLog(t *testing.T, tokenAddr, from, to common.Address, rawValue *big.Int) types.Log {
	t.Helper()
	transferTopic := erc20.ERC20ABI.Events["Transfer"].ID
	data := make([]byte, 32)
	rawValue.FillBytes(data)
	return types.Log{
		Address: tokenAddr,
		Topics: []common.Hash{
			transferTopic,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(to.Bytes()),
		},
		Data: data,
	}
}

// TestParseTransferAtomizesRawValue is the regression test for a real bug: a
// LINK receive was displayed as $7,097,454,078.16 / 612,137,000 LINK instead
// of $7.10 / 0.612137 LINK — exactly 10^9 too large, matching LINK's EVMFactor
// of 9 (18 on-chain decimals against 9-decimal atomic nLINK). parseTransfer
// was returning transfer.Value.Uint64() directly: the token's raw on-chain
// amount, never atomized. Any token whose EVMFactor isn't 0 hit this; tokens
// configured with EVMFactor 0 (raw units already equal atomic units) masked
// it by coincidence.
func TestParseTransferAtomizesRawValue(t *testing.T) {
	tokenAddr := common.HexToAddress("0x514910771AF9Ca656af840dff83E8264EcF986CA") // LINK mainnet
	acctAddr := common.HexToAddress("0x922e91B18ba073e6067A0D0eDA6BD046296f5184")
	fromAddr := common.HexToAddress("0x1234")

	tokenContract, err := erc20.NewIERC20(tokenAddr, nil)
	if err != nil {
		t.Fatalf("erc20.NewIERC20: %v", err)
	}
	c := &erc20TokenContractor{
		tokenContract:     tokenContract,
		tokenContractAddr: tokenAddr,
		acctAddr:          acctAddr,
		atomize:           Tokens[linkTokenID].EVMToAtomic,
	}

	// 0.612137 LINK at 18 on-chain decimals.
	rawValue, ok := new(big.Int).SetString("612137000000000000", 10)
	if !ok {
		t.Fatal("bad test value")
	}
	log := newTransferLog(t, tokenAddr, fromAddr, acctAddr, rawValue)

	got, err := c.parseTransfer(&types.Receipt{Logs: []*types.Log{&log}})
	if err != nil {
		t.Fatalf("parseTransfer: %v", err)
	}
	// Atomic nLINK: 0.612137 LINK * 1e9 = 612,137,000 nLINK. Not
	// 612,137,000,000,000,000 (the raw on-chain value) and not
	// 612,137,000,000,000,000,000,000,000 (atomized twice).
	const want = 612_137_000
	if got != want {
		t.Fatalf("parseTransfer = %d, want %d (0.612137 LINK in atomic nLINK units)", got, want)
	}
}

// TestParseTransferSumsMultipleLogsBeforeAtomizing guards that multiple
// matching Transfer logs in one receipt are summed in raw units and atomized
// once, not atomized per-log then summed - both orders agree for values that
// divide evenly, so this specifically uses values that don't, to catch a
// regression to per-log atomization silently truncating each addend.
func TestParseTransferSumsMultipleLogsBeforeAtomizing(t *testing.T) {
	tokenAddr := common.HexToAddress("0x514910771AF9Ca656af840dff83E8264EcF986CA")
	acctAddr := common.HexToAddress("0x922e91B18ba073e6067A0D0eDA6BD046296f5184")
	fromAddr := common.HexToAddress("0x1234")

	tokenContract, err := erc20.NewIERC20(tokenAddr, nil)
	if err != nil {
		t.Fatalf("erc20.NewIERC20: %v", err)
	}
	c := &erc20TokenContractor{
		tokenContract:     tokenContract,
		tokenContractAddr: tokenAddr,
		acctAddr:          acctAddr,
		atomize:           Tokens[linkTokenID].EVMToAtomic,
	}

	// Two logs each carrying a sub-atomic-unit remainder (5e8 raw units =
	// 0.5 nLINK) that would round to zero if atomized individually, but sums
	// to exactly 1 nLINK raw (1e9) when combined first.
	half := big.NewInt(500_000_000)
	log1 := newTransferLog(t, tokenAddr, fromAddr, acctAddr, half)
	log2 := newTransferLog(t, tokenAddr, fromAddr, acctAddr, half)

	got, err := c.parseTransfer(&types.Receipt{Logs: []*types.Log{&log1, &log2}})
	if err != nil {
		t.Fatalf("parseTransfer: %v", err)
	}
	if got != 1 {
		t.Fatalf("parseTransfer = %d, want 1 (two 0.5-nLINK-raw logs summed before atomizing)", got)
	}
}

// TestParseTransferIgnoresOtherRecipients guards that only logs sending to
// our own account are summed.
func TestParseTransferIgnoresOtherRecipients(t *testing.T) {
	tokenAddr := common.HexToAddress("0x514910771AF9Ca656af840dff83E8264EcF986CA")
	acctAddr := common.HexToAddress("0x922e91B18ba073e6067A0D0eDA6BD046296f5184")
	otherAddr := common.HexToAddress("0x9999")

	tokenContract, err := erc20.NewIERC20(tokenAddr, nil)
	if err != nil {
		t.Fatalf("erc20.NewIERC20: %v", err)
	}
	c := &erc20TokenContractor{
		tokenContract:     tokenContract,
		tokenContractAddr: tokenAddr,
		acctAddr:          acctAddr,
		atomize:           Tokens[linkTokenID].EVMToAtomic,
	}

	log := newTransferLog(t, tokenAddr, acctAddr, otherAddr, big.NewInt(1_000_000_000_000_000_000))
	if _, err := c.parseTransfer(&types.Receipt{Logs: []*types.Log{&log}}); err == nil {
		t.Fatal("expected an error when no log transfers to our account")
	}
}

// testTokenContractor builds an erc20TokenContractor directly (bypassing
// newERC20TokenContractor's network lookup), with a default EVMFactor of 9,
// matching most tokens (see Token.factor).
func testTokenContractor(t *testing.T, cb bind.ContractBackend, tokenAddr, acctAddr common.Address) *erc20TokenContractor {
	t.Helper()
	tokenContract, err := erc20.NewIERC20(tokenAddr, cb)
	if err != nil {
		t.Fatal(err)
	}
	tok := &Token{Token: &util.Token{Name: "Test Token"}}
	return &erc20TokenContractor{
		cb:                cb,
		tokenContract:     tokenContract,
		tokenContractAddr: tokenAddr,
		acctAddr:          acctAddr,
		atomize:           tok.EVMToAtomic,
	}
}

func TestNewERC20TokenContractor(t *testing.T) {
	t.Parallel()
	tokenAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	acctAddr := common.HexToAddress("0x2222222222222222222222222222222222222222")
	tok := &Token{
		Token:     &util.Token{Name: "Test Token"},
		NetTokens: map[util.Network]*NetToken{util.Simnet: {Address: tokenAddr}},
	}

	c, err := newERC20TokenContractor(util.Simnet, tok, acctAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.tokenAddress(); got != tokenAddr {
		t.Errorf("tokenAddress() = %s, want %s", got, tokenAddr)
	}

	if _, err := newERC20TokenContractor(util.Testnet, tok, acctAddr, nil); err == nil {
		t.Fatal("expected an error for a network the token has no NetTokens entry for")
	}
}

func TestTokenContractorValue(t *testing.T) {
	t.Parallel()
	tokenAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	acctAddr := common.HexToAddress("0x2222222222222222222222222222222222222222")
	otherAddr := common.HexToAddress("0x3333333333333333333333333333333333333333")
	c := testTokenContractor(t, nil, tokenAddr, acctAddr)

	// 5e9 raw units / 1e9 (default EVMFactor) = 5 atomic units.
	amount := big.NewInt(5_000_000_000)

	newTx := func(to *common.Address, data []byte) *types.Transaction {
		return types.NewTx(&types.LegacyTx{To: to, Data: data})
	}

	t.Run("nil recipient is not a token call", func(t *testing.T) {
		t.Parallel()
		in, out, err := c.value(context.Background(), newTx(nil, nil))
		if err != nil || in != 0 || out != 0 {
			t.Fatalf("value = (%d, %d, %v), want (0, 0, nil)", in, out, err)
		}
	})

	t.Run("call to a different contract is not a token call", func(t *testing.T) {
		t.Parallel()
		data, err := erc20.ERC20ABI.Pack("transfer", otherAddr, amount)
		if err != nil {
			t.Fatal(err)
		}
		in, out, err := c.value(context.Background(), newTx(&otherAddr, data))
		if err != nil || in != 0 || out != 0 {
			t.Fatalf("value = (%d, %d, %v), want (0, 0, nil)", in, out, err)
		}
	})

	t.Run("transfer call is outgoing", func(t *testing.T) {
		t.Parallel()
		data, err := erc20.ERC20ABI.Pack("transfer", otherAddr, amount)
		if err != nil {
			t.Fatal(err)
		}
		in, out, err := c.value(context.Background(), newTx(&tokenAddr, data))
		if err != nil {
			t.Fatal(err)
		}
		if in != 0 || out != 5 {
			t.Fatalf("value = (%d, %d), want (0, 5)", in, out)
		}
	})

	t.Run("transferFrom with us as sender is outgoing", func(t *testing.T) {
		t.Parallel()
		data, err := erc20.ERC20ABI.Pack("transferFrom", acctAddr, otherAddr, amount)
		if err != nil {
			t.Fatal(err)
		}
		in, out, err := c.value(context.Background(), newTx(&tokenAddr, data))
		if err != nil {
			t.Fatal(err)
		}
		if in != 0 || out != 5 {
			t.Fatalf("value = (%d, %d), want (0, 5)", in, out)
		}
	})

	t.Run("transferFrom with someone else as sender parses as nothing", func(t *testing.T) {
		t.Parallel()
		data, err := erc20.ERC20ABI.Pack("transferFrom", otherAddr, acctAddr, amount)
		if err != nil {
			t.Fatal(err)
		}
		in, out, err := c.value(context.Background(), newTx(&tokenAddr, data))
		if err != nil || in != 0 || out != 0 {
			t.Fatalf("value = (%d, %d, %v), want (0, 0, nil)", in, out, err)
		}
	})

	t.Run("call to an unrelated method parses as nothing", func(t *testing.T) {
		t.Parallel()
		data, err := erc20.ERC20ABI.Pack("approve", otherAddr, amount)
		if err != nil {
			t.Fatal(err)
		}
		in, out, err := c.value(context.Background(), newTx(&tokenAddr, data))
		if err != nil || in != 0 || out != 0 {
			t.Fatalf("value = (%d, %d, %v), want (0, 0, nil)", in, out, err)
		}
	})
}

func TestTokenContractorBalance(t *testing.T) {
	t.Parallel()
	s := newTestMultiRPC(t)
	tokenAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	c := testTokenContractor(t, s.m, tokenAddr, s.creds.addr)

	s.node.setTokenBalance(tokenAddr, s.creds.addr, big.NewInt(12_345))
	bal, err := c.balance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bal.Cmp(big.NewInt(12_345)) != 0 {
		t.Errorf("balance() = %s, want 12345", bal)
	}
}

func TestTokenContractorEstimateTransferGas(t *testing.T) {
	t.Parallel()
	s := newTestMultiRPC(t)
	tokenAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	c := testTokenContractor(t, s.m, tokenAddr, s.creds.addr)

	to := common.HexToAddress("0x3333333333333333333333333333333333333333")
	gas, err := c.estimateTransferGas(context.Background(), to, big.NewInt(1_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	// The fake node's EstimateGas derives its answer from the call data
	// length (see fakeEthNode.EstimateGas), so this pins the exact packed
	// size of an ERC20 transfer(address,uint256) call: a 4-byte selector
	// plus two 32-byte arguments. A wrong method or argument count would
	// change this value instead of silently passing.
	const wantGas = 21_000 + 16*(4+32+32)
	if gas != wantGas {
		t.Errorf("estimateTransferGas = %d, want %d", gas, wantGas)
	}
}

func TestTokenContractorTransfer(t *testing.T) {
	t.Parallel()
	s := newTestMultiRPC(t)
	tokenAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	c := testTokenContractor(t, s.m, tokenAddr, s.creds.addr)

	txOpts, err := s.m.txOpts(context.Background(), 0, 100_000, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress("0x3333333333333333333333333333333333333333")
	tx, err := c.transfer(txOpts, to, big.NewInt(1_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if tx == nil {
		t.Fatal("transfer returned a nil transaction")
	}
	if got := *tx.To(); got != tokenAddr {
		t.Errorf("tx.To() = %s, want the token contract %s", got, tokenAddr)
	}
	if !s.node.hasTx(tx.Hash()) {
		t.Error("the fake node did not record the sent transaction")
	}
}
