// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/bisoncraft/caribou/util"
	"github.com/bisoncraft/caribou/wallet/asset"
	multibal "github.com/bisoncraft/caribou/wallet/asset/eth/contracts/multibalance"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// contractDeployer deploys the multibalance contract for an evm-compatible
// blockchain. contractDeployer is used by the cmd/deploy/deploy utility.
type contractDeployer byte

var ContractDeployer contractDeployer

func (contractDeployer) estimateDeployFunding(
	ctx context.Context,
	txData []byte,
	deploymentGas uint64,
	chain string,
	credentialsPath string,
	chainCfg *params.ChainConfig,
	ui *util.UnitInfo,
	log util.Logger,
	net util.Network,
) error {

	walletDir, err := os.MkdirTemp("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(walletDir)

	cl, maxFeeRate, _, err := ContractDeployer.nodeAndRate(ctx, chain, walletDir, credentialsPath, chainCfg, log, net)
	if err != nil {
		return err
	}
	defer cl.shutdown()

	log.Infof("Address: %s", cl.address())

	baseChainBal, err := cl.addressBalance(ctx, cl.address())
	if err != nil {
		return fmt.Errorf("error getting eth balance: %v", err)
	}

	log.Infof("Balance: %s %s", ui.ConventionalString(WeiToGwei(baseChainBal)), ui.Conventional.Unit)

	var gas uint64
	if baseChainBal.Cmp(new(big.Int)) > 0 {
		// We may be able to get a proper estimate.
		gas, err = cl.EstimateGas(ctx, ethereum.CallMsg{
			From: cl.creds.addr,
			To:   nil, // special value means deploy contract
			Data: txData,
		})
		gas = gas * 5 / 4 // 20% buffer on gas
		if err != nil {
			log.Debugf("EstimateGas error: %v", err)
			log.Info("Unable to get on-chain estimate. balance probably too low. Falling back to rough estimate")
		}
	}

	feeRate := WeiToGweiCeil(maxFeeRate)
	if gas == 0 {
		gas = deploymentGas
	}
	fees := feeRate * gas

	log.Infof("Estimated fees: %s", ui.ConventionalString(fees))

	gweiBal := WeiToGwei(baseChainBal)
	if fees < gweiBal {
		log.Infof("👍 Current balance (%s %s) sufficient for fees (%s)",
			ui.ConventionalString(gweiBal), ui.Conventional.Unit, ui.ConventionalString(fees))
		return nil
	}

	shortage := fees - gweiBal
	log.Infof("❌ Current balance (%[1]s %[2]s) insufficient for fees (%[3]s). Send %[4]s %[2]s to %[5]s",
		ui.ConventionalString(gweiBal), ui.Conventional.Unit, ui.ConventionalString(fees),
		ui.ConventionalString(shortage), cl.address())

	return nil
}

func (contractDeployer) EstimateMultiBalanceDeployFunding(
	ctx context.Context,
	chain string,
	credentialsPath string,
	chainCfg *params.ChainConfig,
	ui *util.UnitInfo,
	log util.Logger,
	net util.Network,
) error {
	const deploymentGas = 400_000 // 302_647 for https://goerli.etherscan.io/tx/0x540d3e82888b18f89566a988712a7c2ecd45bd2df472f8dd689e319ae9fa4445
	txData := common.FromHex(multibal.MultiBalanceV0MetaData.Bin)
	return ContractDeployer.estimateDeployFunding(ctx, txData, deploymentGas, chain, credentialsPath, chainCfg, ui, log, net)
}

type deployerFunc func(txOpts *bind.TransactOpts, cb bind.ContractBackend) (common.Address, *types.Transaction, error)

// DeployContract deployes a dcrdex swap contract.
func (contractDeployer) deployContract(
	ctx context.Context,
	txData []byte,
	deployer deployerFunc,
	chain string,
	credentialsPath string,
	chainCfg *params.ChainConfig,
	ui *util.UnitInfo,
	log util.Logger,
	net util.Network,
) error {

	walletDir, err := os.MkdirTemp("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(walletDir)

	cl, maxFeeRate, tipRate, err := ContractDeployer.nodeAndRate(ctx, chain, walletDir, credentialsPath, chainCfg, log, net)
	if err != nil {
		return err
	}
	defer cl.shutdown()

	log.Infof("Address: %s", cl.address())

	baseChainBal, err := cl.addressBalance(ctx, cl.address())
	if err != nil {
		return fmt.Errorf("error getting eth balance: %v", err)
	}

	log.Infof("Balance: %s %s", ui.ConventionalString(WeiToGwei(baseChainBal)), ui.Conventional.Unit)

	// We may be able to get a proper estimate.
	gas, err := cl.EstimateGas(ctx, ethereum.CallMsg{
		From: cl.address(),
		To:   nil, // special value means deploy contract
		Data: txData,
	})
	if err != nil {
		return fmt.Errorf("EstimateGas error: %v", err)
	}

	feeRate := WeiToGweiCeil(maxFeeRate)
	log.Infof("Estimated fees: %s gwei", ui.ConventionalString(feeRate*gas))

	gas = gas * 5 / 4 // Add 20% buffer
	feesWithBuffer := feeRate * gas

	gweiBal := WeiToGwei(baseChainBal)
	if feesWithBuffer >= gweiBal {
		shortage := feesWithBuffer - gweiBal
		return fmt.Errorf("❌ Current balance (%[1]s %[2]s) insufficient for fees (%[3]s). Send %[4]s %[2]s to %[5]s",
			ui.ConventionalString(gweiBal), ui.Conventional.Unit, ui.ConventionalString(feesWithBuffer),
			ui.ConventionalString(shortage), cl.address())
	}

	txOpts, err := cl.txOpts(ctx, 0, gas, GweiToWei(feeRate), tipRate, nil)
	if err != nil {
		return err
	}

	contractAddr, tx, err := deployer(txOpts, cl.contractBackend())
	if err != nil {
		return err
	}

	log.Infof("Contract %s launched with tx %s", contractAddr, tx.Hash())

	if err = waitForConfirmation(ctx, "deploy", cl, tx.Hash(), log); err != nil {
		return fmt.Errorf("error waiting for deployment transaction status: %w", err)
	}

	log.Info("👍 Transaction confirmed")

	return nil
}

// ReturnETH returns the remaining base asset balance from the deployment/getgas
// wallet to the specified return address.
func (contractDeployer) ReturnETH(
	ctx context.Context,
	chain string,
	returnAddr common.Address,
	credentialsPath string,
	chainCfg *params.ChainConfig,
	ui *util.UnitInfo,
	log util.Logger,
	net util.Network,
) error {

	walletDir, err := os.MkdirTemp("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(walletDir)

	cl, maxFeeRate, tipRate, err := ContractDeployer.nodeAndRate(ctx, chain, walletDir, credentialsPath, chainCfg, log, net)
	if err != nil {
		return err
	}
	defer cl.shutdown()

	return GetGas.returnFunds(ctx, cl, maxFeeRate, tipRate, returnAddr, nil, ui, log, net)
}

func (contractDeployer) nodeAndRate(
	ctx context.Context,
	chain string,
	walletDir,
	credentialsPath string,

	chainCfg *params.ChainConfig,
	log util.Logger,
	net util.Network,
) (*multiRPCClient, *big.Int, *big.Int, error) {

	seed, providers, err := getFileCredentials(chain, credentialsPath, net)
	if err != nil {
		return nil, nil, nil, err
	}

	pw := []byte("abc")
	chainID := chainCfg.ChainID.Int64()

	if err := CreateEVMWallet(chainID, &asset.CreateWalletParams{
		Type:     walletTypeRPC,
		Seed:     seed,
		Pass:     pw,
		Settings: map[string]string{providersKey: strings.Join(providers, " ")},
		DataDir:  walletDir,
		Net:      net,
		Logger:   log,
	}, nil /* we don't need the full api, skipConnect = true allows for nil compat */, true); err != nil {
		return nil, nil, nil, fmt.Errorf("error creating wallet: %w", err)
	}

	cl, err := newMultiRPCClient(walletDir, providers, log, chainCfg, 3, net, "")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error creating rpc client: %w", err)
	}

	if err := cl.unlock(string(pw)); err != nil {
		return nil, nil, nil, fmt.Errorf("error unlocking rpc client: %w", err)
	}

	if err = cl.connect(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("error connecting: %w", err)
	}

	baseRate, tipRate, err := cl.currentFees(ctx)
	if err != nil {
		cl.shutdown()
		return nil, nil, nil, fmt.Errorf("error estimating fee rate: %v", err)
	}

	maxFeeRate := new(big.Int).Add(tipRate, new(big.Int).Mul(baseRate, big.NewInt(2)))
	return cl, maxFeeRate, tipRate, nil
}

// DeployMultiBalance deployes a contract with a function for reading all
// balances in one call.
func (contractDeployer) DeployMultiBalance(
	ctx context.Context,
	chain string,
	credentialsPath string,
	chainCfg *params.ChainConfig,
	ui *util.UnitInfo,
	log util.Logger,
	net util.Network,
) error {
	txData := common.FromHex(multibal.MultiBalanceV0MetaData.Bin)
	deployer := func(txOpts *bind.TransactOpts, cb bind.ContractBackend) (common.Address, *types.Transaction, error) {
		contractAddr, tx, _, err := multibal.DeployMultiBalanceV0(txOpts, cb)
		return contractAddr, tx, err
	}
	return ContractDeployer.deployContract(ctx, txData, deployer, chain, credentialsPath, chainCfg, ui, log, net)
}
