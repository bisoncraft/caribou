// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"fmt"
	"math/big"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset/erc20"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// tokenContractor interacts with an ERC20 token contract for the balance,
// transfer, and transaction-history features of the wallet. It is keyed on the
// token's own ERC20 contract address and has no dependency on any swap
// contract.
type tokenContractor interface {
	// tokenAddress returns the token's ERC20 contract address.
	tokenAddress() common.Address
	// balance returns the account's token balance.
	balance(context.Context) (*big.Int, error)
	// transfer calls the transfer method of the erc20 token contract. Used for
	// sends or withdrawals.
	transfer(*bind.TransactOpts, common.Address, *big.Int) (*types.Transaction, error)
	// parseTransfer sums incoming transfer value to this account from a
	// receipt's logs. Used for transaction history.
	parseTransfer(*types.Receipt) (uint64, error)
	// value checks the incoming or outgoing token-transfer value of a tx. It is
	// not an error if the transaction is not a transfer; the returned values
	// will be zero in that case.
	value(context.Context, *types.Transaction) (incoming, outgoing uint64, err error)
	// estimateTransferGas estimates the gas needed for a transfer tx. The
	// account needs to have > amount tokens to use this method.
	estimateTransferGas(context.Context, *big.Int) (uint64, error)
}

// erc20TokenContractor is a standalone tokenContractor backed solely by the
// token's own ERC20 contract.
type erc20TokenContractor struct {
	cb                bind.ContractBackend
	tokenContract     *erc20.IERC20
	tokenContractAddr common.Address
	acctAddr          common.Address
	atomize           func(*big.Int) uint64
}

var _ tokenContractor = (*erc20TokenContractor)(nil)

// newERC20TokenContractor constructs an erc20TokenContractor for the given
// token on the given network. The contractor is keyed on the token's ERC20
// contract address (NetToken.Address).
func newERC20TokenContractor(net util.Network, token *Token, acctAddr common.Address, cb bind.ContractBackend) (tokenContractor, error) {
	netToken, found := token.NetTokens[net]
	if !found {
		return nil, fmt.Errorf("token %s has no network %s", token.Name, net)
	}
	tokenAddr := netToken.Address

	tokenContract, err := erc20.NewIERC20(tokenAddr, cb)
	if err != nil {
		return nil, err
	}

	return &erc20TokenContractor{
		cb:                cb,
		tokenContract:     tokenContract,
		tokenContractAddr: tokenAddr,
		acctAddr:          acctAddr,
		atomize:           token.EVMToAtomic,
	}, nil
}

// tokenAddress returns the token's ERC20 contract address.
func (c *erc20TokenContractor) tokenAddress() common.Address {
	return c.tokenContractAddr
}

// balance exposes the read-only balanceOf method of the erc20 token contract.
func (c *erc20TokenContractor) balance(ctx context.Context) (*big.Int, error) {
	callOpts := &bind.CallOpts{
		From:    c.acctAddr,
		Context: ctx,
	}
	return c.tokenContract.BalanceOf(callOpts, c.acctAddr)
}

// transfer calls the transfer method of the erc20 token contract.
func (c *erc20TokenContractor) transfer(txOpts *bind.TransactOpts, addr common.Address, amount *big.Int) (*types.Transaction, error) {
	return c.tokenContract.Transfer(txOpts, addr, amount)
}

// estimateTransferGas estimates the gas needed for a transfer tx.
func (c *erc20TokenContractor) estimateTransferGas(ctx context.Context, amount *big.Int) (uint64, error) {
	return estimateGas(ctx, c.acctAddr, c.tokenContractAddr, erc20.ERC20ABI, c.cb, new(big.Int), "transfer", c.acctAddr, amount)
}

// parseTransfer sums incoming transfer value to this account from the receipt's
// logs.
func (c *erc20TokenContractor) parseTransfer(receipt *types.Receipt) (uint64, error) {
	var transferredAmt uint64
	for _, log := range receipt.Logs {
		if log.Address != c.tokenContractAddr {
			continue
		}
		transfer, err := c.tokenContract.ParseTransfer(*log)
		if err != nil {
			continue
		}
		if transfer.To == c.acctAddr {
			transferredAmt += transfer.Value.Uint64()
		}
	}

	if transferredAmt > 0 {
		return transferredAmt, nil
	}

	return 0, fmt.Errorf("transfer log to %s not found", c.tokenContractAddr)
}

// value finds incoming or outgoing value for a tx to the erc20 token contract.
// Only transfer and transferFrom are parsed. It is not an error if this tx is a
// call to another method of the token contract, but no values will be parsed.
func (c *erc20TokenContractor) value(_ context.Context, tx *types.Transaction) (in, out uint64, err error) {
	if tx.To() == nil || *tx.To() != c.tokenContractAddr {
		return 0, 0, nil
	}

	if sender, _, value, err := erc20.ParseTransferFromData(tx.Data()); err == nil && sender == c.acctAddr {
		return 0, c.atomize(value), nil
	}

	if _, value, err := erc20.ParseTransferData(tx.Data()); err == nil {
		return 0, c.atomize(value), nil
	}

	return 0, 0, nil
}

// estimateGas estimates the gas used to call a method against a contract.
func estimateGas(ctx context.Context, from, to common.Address, contractABI *abi.ABI, cb bind.ContractBackend, value *big.Int, method string, args ...any) (uint64, error) {
	data, err := contractABI.Pack(method, args...)
	if err != nil {
		return 0, fmt.Errorf("pack error: %v", err)
	}

	return cb.EstimateGas(ctx, ethereum.CallMsg{
		From:  from,
		To:    &to,
		Data:  data,
		Value: value,
	})
}
