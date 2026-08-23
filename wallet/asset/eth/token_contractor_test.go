//go:build !harness && !rpclive

package eth

import (
	"math/big"
	"testing"

	"github.com/bisoncraft/caribou/wallet/asset/erc20"
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
