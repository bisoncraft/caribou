// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build !harness && !rpclive

package eth

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/txdb"
)

type mockBridge struct {
	getCompletionDataFunc            func(ctx context.Context, txID string) ([]byte, error)
	getCompletionDataCalled          chan struct{}
	completeBridgeCalled             common.Hash
	requiresCompletionResult         bool
	requiresFollowUpCompletionResult bool
	verifyBridgeCompletionResult     bool
	verifyBridgeCompletionError      error
	getFollowUpCompletionDataResult  struct {
		required bool
		data     []byte
		err      error
	}
	completeFollowUpBridgeCalled    common.Hash
	completeBridgeGasResult         uint64
	followUpCompleteBridgeGasResult uint64
}

var _ bridge = (*mockBridge)(nil)

func (m *mockBridge) getCompletionData(ctx context.Context, sourceAssetID uint32, bridgeTxID string) ([]byte, error) {
	if m.getCompletionDataCalled != nil {
		m.getCompletionDataCalled <- struct{}{}
	}
	return m.getCompletionDataFunc(ctx, bridgeTxID)
}
func (m *mockBridge) bridgeContractAddr(ctx context.Context, assetID uint32) (common.Address, error) {
	panic("not implemented")
}
func (m *mockBridge) bridgeContractAllowance(ctx context.Context, assetID uint32) (*big.Int, error) {
	panic("not implemented")
}
func (m *mockBridge) approveBridgeContract(txOpts *bind.TransactOpts, amount *big.Int, assetID uint32) (*types.Transaction, error) {
	panic("not implemented")
}
func (m *mockBridge) requiresBridgeContractApproval(assetID uint32) bool { panic("not implemented") }
func (m *mockBridge) initiateBridge(txOpts *bind.TransactOpts, sourceAssetID, destAssetID uint32, amount *big.Int) (*types.Transaction, error) {
	panic("not implemented")
}
func (m *mockBridge) completeBridge(txOpts *bind.TransactOpts, destAssetID uint32, mintInfo []byte) (*types.Transaction, error) {
	tx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 0, nil, encode.RandomBytes(32))
	m.completeBridgeCalled = tx.Hash()
	return tx, nil
}
func (m *mockBridge) initiateBridgeGas(sourceAssetID uint32) uint64 { return 0 }
func (m *mockBridge) completeBridgeGas(destAssetID uint32) uint64   { return m.completeBridgeGasResult }
func (m *mockBridge) requiresCompletion(destAssetID uint32) bool {
	return m.requiresCompletionResult
}
func (m *mockBridge) verifyBridgeCompletion(ctx context.Context, data []byte) (bool, error) {
	return m.verifyBridgeCompletionResult, m.verifyBridgeCompletionError
}
func (m *mockBridge) supportedDestinations(sourceAssetID uint32) []uint32 {
	return []uint32{}
}
func (m *mockBridge) requiresFollowUpCompletion(destAssetID uint32) bool {
	return m.requiresFollowUpCompletionResult
}
func (m *mockBridge) getFollowUpCompletionData(ctx context.Context, completionTxID string) (required bool, data []byte, err error) {
	return m.getFollowUpCompletionDataResult.required, m.getFollowUpCompletionDataResult.data, m.getFollowUpCompletionDataResult.err
}
func (m *mockBridge) completeFollowUpBridge(txOpts *bind.TransactOpts, data []byte) (tx *types.Transaction, err error) {
	tx = types.NewTransaction(0, common.Address{}, big.NewInt(0), 0, nil, encode.RandomBytes(32))
	m.completeFollowUpBridgeCalled = tx.Hash()
	return tx, nil
}
func (m *mockBridge) followUpCompleteBridgeGas() uint64 { return m.followUpCompleteBridgeGasResult }
func (m *mockBridge) bridgeLimits(sourceAssetID, destAssetID uint32) (min, max *big.Int, hasLimits bool, err error) {
	return nil, nil, false, nil
}

func TestBridgeCompletionFees(t *testing.T) {
	t.Parallel()
	const bridgeName = "mock"
	const maxFeeRateGwei = 202

	tests := []struct {
		name              string
		assetID           uint32
		completeBridgeGas uint64
		followUpGas       uint64
		balance           uint64
		parentBalance     uint64

		expectFees          uint64
		expectSufficientBal bool
		expectErr           bool
		bridgeNotFound      bool
	}{
		{
			name:                "base asset - sufficient balance",
			assetID:             BipID,
			completeBridgeGas:   100_000,
			followUpGas:         50_000,
			balance:             150_000 * maxFeeRateGwei,
			expectFees:          150_000 * maxFeeRateGwei,
			expectSufficientBal: true,
		},
		{
			name:                "base asset - insufficient balance",
			assetID:             BipID,
			completeBridgeGas:   100_000,
			followUpGas:         50_000,
			balance:             150_000*maxFeeRateGwei - 1,
			expectFees:          150_000 * maxFeeRateGwei,
			expectSufficientBal: false,
		},
		{
			name:                "base asset - zero completion gas",
			assetID:             BipID,
			completeBridgeGas:   0,
			followUpGas:         0,
			balance:             0,
			expectFees:          0,
			expectSufficientBal: true,
		},
		{
			name:                "token - sufficient parent balance",
			assetID:             usdcEthID,
			completeBridgeGas:   200_000,
			followUpGas:         0,
			balance:             1e6,
			parentBalance:       200_000 * maxFeeRateGwei,
			expectFees:          200_000 * maxFeeRateGwei,
			expectSufficientBal: true,
		},
		{
			name:                "token - insufficient parent balance",
			assetID:             usdcEthID,
			completeBridgeGas:   200_000,
			followUpGas:         0,
			balance:             1e9,
			parentBalance:       200_000*maxFeeRateGwei - 1,
			expectFees:          200_000 * maxFeeRateGwei,
			expectSufficientBal: false,
		},
		{
			name:           "bridge not found",
			assetID:        BipID,
			bridgeNotFound: true,
			expectErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, aw, node, shutdown := tassetWallet(tt.assetID)
			defer shutdown()

			node.bal = GweiToWei(tt.balance)

			// Set up parent balance for tokens
			if tt.assetID != BipID && node.tokenParent != nil {
				parentNode := newTestNode(BipID)
				parentNode.bal = GweiToWei(tt.parentBalance)
				node.tokenParent.node = parentNode
			}

			if !tt.bridgeNotFound {
				mb := &mockBridge{
					completeBridgeGasResult:         tt.completeBridgeGas,
					followUpCompleteBridgeGasResult: tt.followUpGas,
				}
				aw.bridges = map[string]bridge{bridgeName: mb}
			}

			fees, hasSufficientBal, err := aw.BridgeCompletionFees(bridgeName)

			if tt.expectErr {
				if err == nil {
					t.Fatal("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if fees != tt.expectFees {
				t.Errorf("expected fees %d, got %d", tt.expectFees, fees)
			}

			if hasSufficientBal != tt.expectSufficientBal {
				t.Errorf("expected hasSufficientBalance=%v, got %v", tt.expectSufficientBal, hasSufficientBal)
			}
		})
	}
}

func TestBridgeManager(t *testing.T) {
	t.Parallel()
	// bridgeMonitorInterval is both the bridge manager's poll interval and
	// the unit that negative-assertion waits below are sized against. Keeping
	// them in one place makes the coupling explicit so a future bump to the
	// interval can't silently shrink the wait to under a single tick.
	const bridgeMonitorInterval = 100 * time.Millisecond
	negativeAssertionWait := 3 * bridgeMonitorInterval

	setupWithPendingBridges := func(t *testing.T, pendingBridges []*extendedWalletTx) (*bridgeManager, *mockBridge, chan asset.WalletNotification, *tTxDB, util.Logger) {
		log := util.StdOutLogger("TEST", util.LevelDebug)
		notificationChan := make(chan asset.WalletNotification, 10)
		emitter := asset.NewWalletEmitter(notificationChan, BipID, log)

		// Create a mock bridge with default "not ready" behavior
		mb := &mockBridge{
			getCompletionDataFunc: func(ctx context.Context, txID string) ([]byte, error) {
				return nil, errors.New("not ready")
			},
			getCompletionDataCalled: make(chan struct{}, 10),
		}

		db := &tTxDB{}
		db.pendingBridges = pendingBridges
		bridges := map[string]bridge{"mock": mb}

		getConfirmedTxFunc := func(txHash common.Hash, callback func(*extendedWalletTx)) bool {
			tx := &extendedWalletTx{
				WalletTransaction: &txdb.WalletTransaction{
					ID:        txHash.Hex(),
					Confirmed: true,
				},
			}
			callback(tx)
			return true
		}

		bm, err := newBridgeManager(&bridgeManagerConfig{
			ctx:             context.Background(),
			baseChainID:     BipID,
			bridges:         bridges,
			emit:            emitter,
			txDB:            db,
			monitorInterval: bridgeMonitorInterval,
			log:             log,
			getTxFunc:       getConfirmedTxFunc,
		})
		if err != nil {
			t.Fatalf("error creating bridge manager: %v", err)
		}

		return bm, mb, notificationChan, db, log
	}

	setup := func(t *testing.T) (*bridgeManager, *mockBridge, chan asset.WalletNotification, *tTxDB, util.Logger) {
		bm, mb, notificationChan, db, log := setupWithPendingBridges(t, nil)
		return bm, mb, notificationChan, db, log
	}

	// Test: Adding and monitoring a pending bridge
	t.Run("AddAndMonitor", func(t *testing.T) {
		t.Parallel()
		bm, mb, _, _, _ := setup(t)

		// Add a pending bridge
		burnTxID := "tx1"
		sourceAssetID := uint32(BipID)
		destAssetID := uint32(123)
		amount := uint64(1e9)
		bridgeName := "mock"
		bm.addPendingBridge(burnTxID, sourceAssetID, destAssetID, amount, bridgeName)

		// Wait for getMintInfo to be called at least twice
		for i := 0; i < 2; i++ {
			select {
			case <-mb.getCompletionDataCalled:
				// Continue
			case <-time.After(1 * time.Second):
				t.Fatalf("timeout waiting for getCompletionData call %d", i+1)
			}
		}
	})

	// Test: Successful mint info retrieval and notification
	t.Run("SuccessfulMintInfo", func(t *testing.T) {
		t.Parallel()
		bm, mb, notificationChan, _, _ := setup(t)

		// Configure getMintInfo to succeed
		completionData := []byte("completionData")
		mb.getCompletionDataFunc = func(ctx context.Context, txID string) ([]byte, error) {
			return completionData, nil
		}

		// Add a pending bridge
		burnTxID := "tx2"
		sourceAssetID := uint32(BipID)
		destAssetID := uint32(60001)
		amount := uint64(2e9)
		bridgeName := "mock"
		bm.addPendingBridge(burnTxID, sourceAssetID, destAssetID, amount, bridgeName)

		// Wait for and verify the notification
		select {
		case note := <-notificationChan:
			bridgeNote, ok := note.(*asset.BridgeReadyToCompleteNote)
			if !ok {
				t.Fatalf("expected *BridgeReadyToCompleteNote, got %T", note)
			}
			if bridgeNote.DestAssetID != destAssetID {
				t.Errorf("expected destAssetID %d, got %d", destAssetID, bridgeNote.DestAssetID)
			}
			if string(bridgeNote.Data) != string(completionData) {
				t.Errorf("expected completionData %q, got %q", completionData, bridgeNote.Data)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("timeout waiting for BridgeReadyToComplete notification")
		}
	})

	t.Run("MarkAsComplete", func(t *testing.T) {
		t.Parallel()
		bm, mb, _, db, _ := setup(t)

		// Add a pending bridge
		burnTxID := "tx3"
		sourceAssetID := uint32(BipID)
		destAssetID := uint32(789)
		amount := uint64(3e9)
		bridgeName := "mock"
		bm.addPendingBridge(burnTxID, sourceAssetID, destAssetID, amount, bridgeName)

		db.txToGet = &extendedWalletTx{
			WalletTransaction: &txdb.WalletTransaction{
				ID: burnTxID,
				BridgeCounterpartTx: &txdb.BridgeCounterpartTx{
					AssetID: destAssetID,
				},
			},
		}

		// Wait for at least one getMintInfo call
		select {
		case <-mb.getCompletionDataCalled:
			// Proceed
		case <-time.After(1 * time.Second):
			t.Fatal("timeout waiting for initial getCompletionData call")
		}

		// Remove the bridge
		bm.markBridgeComplete(burnTxID, []string{"mintTxID"}, 1000000, 1000, true)

		// Wait long enough that the monitor would have ticked at least
		// twice if it were still polling, then assert no further calls.
		time.Sleep(negativeAssertionWait)
		select {
		case <-mb.getCompletionDataCalled:
			t.Error("getCompletionData was called after removing all pending bridges")
		default:
			// No additional calls, as expected
		}

		if len(db.storedTx.BridgeCounterpartTx.IDs) == 0 || db.storedTx.BridgeCounterpartTx.IDs[len(db.storedTx.BridgeCounterpartTx.IDs)-1] != "mintTxID" {
			t.Errorf("expected last BridgeCounterpartTx.ID to be mintTxID, got %v", db.storedTx.BridgeCounterpartTx.IDs)
		}
	})

	t.Run("LoadPendingBridges", func(t *testing.T) {
		t.Parallel()
		destAssetID := uint32(123)
		burnTxID := "tx4"
		bm, _, _, _, _ := setupWithPendingBridges(t, []*extendedWalletTx{
			{
				WalletTransaction: &txdb.WalletTransaction{
					Type: txdb.InitiateBridge,
					ID:   burnTxID,
					BridgeCounterpartTx: &txdb.BridgeCounterpartTx{
						AssetID: destAssetID,
					},
				},
			},
		})

		if len(bm.pendingBridges) != 1 {
			t.Fatalf("expected 1 pending bridge, got %d", len(bm.pendingBridges))
		}
		if bm.pendingBridges[burnTxID].destAssetID != destAssetID {
			t.Fatalf("expected pending bridge ID to be %s, and dest asset ID to be %d", burnTxID, destAssetID)
		}
	})

	t.Run("SkipUnconfirmedTx", func(t *testing.T) {
		t.Parallel()
		log := util.StdOutLogger("TEST", util.LevelDebug)
		notificationChan := make(chan asset.WalletNotification, 10)
		emitter := asset.NewWalletEmitter(notificationChan, BipID, log)

		mb := &mockBridge{
			getCompletionDataFunc: func(ctx context.Context, txID string) ([]byte, error) {
				return []byte("completion data"), nil
			},
			getCompletionDataCalled: make(chan struct{}, 10),
		}

		db := &tTxDB{}
		bridges := map[string]bridge{"mock": mb}

		// Create getTxFunc that returns an unconfirmed transaction
		getTxFunc := func(txHash common.Hash, callback func(*extendedWalletTx)) bool {
			tx := &extendedWalletTx{
				WalletTransaction: &txdb.WalletTransaction{
					ID:        txHash.Hex(),
					Confirmed: false, // Not confirmed
				},
			}
			callback(tx)
			return true
		}

		bm, err := newBridgeManager(&bridgeManagerConfig{
			ctx:             context.Background(),
			baseChainID:     BipID,
			bridges:         bridges,
			emit:            emitter,
			txDB:            db,
			monitorInterval: bridgeMonitorInterval,
			log:             log,
			getTxFunc:       getTxFunc,
		})
		if err != nil {
			t.Fatalf("error creating bridge manager: %v", err)
		}

		// Add a pending bridge
		burnTxID := "unconfirmed_tx"
		sourceAssetID := uint32(BipID)
		destAssetID := uint32(456)
		amount := uint64(1e9)
		bridgeName := "mock"
		bm.addPendingBridge(burnTxID, sourceAssetID, destAssetID, amount, bridgeName)

		// Wait long enough that the monitor would have ticked at least
		// twice if it were going to poll, then assert it stayed quiet.
		time.Sleep(negativeAssertionWait)
		select {
		case <-mb.getCompletionDataCalled:
			t.Error("getCompletionData was called for unconfirmed transaction")
		default:
			// Expected behavior - no call made
		}

		// Ensure no notification is sent
		select {
		case <-notificationChan:
			t.Error("unexpected notification for unconfirmed transaction")
		default:
			// Expected behavior - no notification
		}
	})
}

func TestCompleteBridge(t *testing.T) {
	t.Parallel()
	initiationTx := &txdb.BridgeCounterpartTx{
		IDs:     []string{"initiation-tx-id"},
		AssetID: 123,
	}
	timestamp := uint64(time.Now().Unix())
	confirmedTx := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			ID:                  "complete-tx-id",
			Amount:              1e9,
			Fees:                5000, // 5000 gwei fees
			Confirmed:           true,
			Type:                txdb.CompleteBridge,
			BridgeCounterpartTx: initiationTx,
			Timestamp:           timestamp,
		},
		Nonce:     new(big.Int),
		savedToDB: true,
	}
	// Previous completion tx for follow-up scenarios
	initialCompletionTx := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			ID:                  "initial-completion-tx-id",
			Amount:              0,
			Fees:                3000, // 3000 gwei fees
			Confirmed:           true,
			Type:                txdb.CompleteBridge,
			BridgeCounterpartTx: initiationTx,
			Timestamp:           timestamp,
		},
		Nonce:     new(big.Int),
		savedToDB: true,
	}
	confirmedTxWithFollowUpData := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			ID:                  "follow-up-tx-id",
			Amount:              1e9,
			Fees:                2000, // 2000 gwei fees for follow-up
			Confirmed:           true,
			Type:                txdb.CompleteBridge,
			BridgeCounterpartTx: initiationTx,
			Timestamp:           timestamp,
		},
		Nonce:                      new(big.Int),
		PreviousBridgeCompletionID: "initial-completion-tx-id",
		BridgeFollowUpData:         []byte("verification-data"),
		savedToDB:                  true,
	}
	pendingTx := &extendedWalletTx{
		WalletTransaction: &txdb.WalletTransaction{
			ID:                  "pending-tx-id",
			Amount:              1e9,
			Fees:                0, // No fees yet - not confirmed
			Confirmed:           false,
			Type:                txdb.CompleteBridge,
			BridgeCounterpartTx: initiationTx,
			Timestamp:           timestamp,
		},
		Nonce:     new(big.Int),
		savedToDB: true,
	}

	tests := []struct {
		name                    string
		dbTx                    *extendedWalletTx
		extraTxs                map[common.Hash]*extendedWalletTx // additional txs for DB lookup
		pendingTx               *extendedWalletTx
		dbErr                   error
		bridge                  *mockBridge
		expectComplete          bool
		expectCompleteFollowUp  bool
		expectErr               bool
		expectNote              bool
		expectedCompletionTxIDs []string
		expectedIsComplete      bool
		expectedFees            uint64
	}{
		{
			name: "new completion - requires completion",
			bridge: &mockBridge{
				requiresCompletionResult: true,
			},
			expectComplete:          true,
			expectNote:              true,
			expectedCompletionTxIDs: []string{"mock-tx-id"}, // Will be generated by mock
			expectedIsComplete:      false,                  // Not complete until confirmed
			expectedFees:            0,                      // No fees until confirmed
		},
		{
			name: "bridge does not require completion - verification successful",
			bridge: &mockBridge{
				requiresCompletionResult:     false,
				verifyBridgeCompletionResult: true,
			},
			expectNote:              true,
			expectedCompletionTxIDs: []string{}, // No completion transactions
			expectedIsComplete:      true,
			expectedFees:            0, // No completion tx, no fees
		},
		{
			name: "bridge does not require completion - verification failed",
			bridge: &mockBridge{
				requiresCompletionResult:     false,
				verifyBridgeCompletionResult: false,
			},
			expectNote:              false,      // No note when verification fails
			expectedCompletionTxIDs: []string{}, // No completion transactions
			expectedIsComplete:      false,
		},
		{
			name:      "pending completion",
			pendingTx: pendingTx,
			bridge: &mockBridge{
				requiresCompletionResult: true,
			},
			expectNote:              false,      // No note when pending
			expectedCompletionTxIDs: []string{}, // No completion TxIDs when pending
			expectedIsComplete:      false,
		},
		{
			name: "confirmed completion - no follow-up required",
			dbTx: confirmedTx,
			bridge: &mockBridge{
				requiresCompletionResult:         true,
				requiresFollowUpCompletionResult: false,
			},
			expectNote:              true,
			expectedCompletionTxIDs: []string{"complete-tx-id"},
			expectedIsComplete:      true,
			expectedFees:            5000, // confirmedTx.Fees
		},
		{
			name: "confirmed completion - follow-up required, verification complete",
			dbTx: confirmedTxWithFollowUpData,
			extraTxs: map[common.Hash]*extendedWalletTx{
				common.HexToHash("initial-completion-tx-id"): initialCompletionTx,
			},
			bridge: &mockBridge{
				requiresCompletionResult:         true,
				requiresFollowUpCompletionResult: true,
				verifyBridgeCompletionResult:     true,
			},
			expectNote:              true,
			expectedCompletionTxIDs: []string{"initial-completion-tx-id", "follow-up-tx-id"},
			expectedIsComplete:      true,
			expectedFees:            5000, // initialCompletionTx.Fees (3000) + confirmedTxWithFollowUpData.Fees (2000)
		},
		{
			name: "confirmed completion - follow-up required, verification incomplete",
			dbTx: confirmedTxWithFollowUpData,
			bridge: &mockBridge{
				requiresCompletionResult:         true,
				requiresFollowUpCompletionResult: true,
				verifyBridgeCompletionResult:     false,
			},
			expectNote:              false, // No note when verification incomplete
			expectedCompletionTxIDs: []string{"initial-completion-tx-id", "follow-up-tx-id"},
			expectedIsComplete:      false,
		},
		{
			name: "confirmed completion - follow-up required, no verification data, not required",
			dbTx: confirmedTx,
			bridge: &mockBridge{
				requiresCompletionResult:         true,
				requiresFollowUpCompletionResult: true,
				getFollowUpCompletionDataResult: struct {
					required bool
					data     []byte
					err      error
				}{
					required: false,
					data:     nil,
					err:      nil,
				},
			},
			expectNote:              true,
			expectedCompletionTxIDs: []string{"complete-tx-id"},
			expectedIsComplete:      true,
			expectedFees:            5000, // confirmedTx.Fees
		},
		{
			name: "confirmed completion - follow-up required, no verification data, follow-up required",
			dbTx: confirmedTx,
			bridge: &mockBridge{
				requiresCompletionResult:         true,
				requiresFollowUpCompletionResult: true,
				getFollowUpCompletionDataResult: struct {
					required bool
					data     []byte
					err      error
				}{
					required: true,
					data:     []byte("follow-up-data"),
					err:      nil,
				},
			},
			expectNote:              true,
			expectCompleteFollowUp:  true,
			expectedCompletionTxIDs: []string{"complete-tx-id", "mock-tx-id"}, // Initial + new follow-up
			expectedIsComplete:      false,                                    // Not complete until follow-up verified
			expectedFees:            0,                                        // No fees until follow-up confirmed
		},
		{
			name:  "db error",
			dbErr: errors.New("db error"),
			bridge: &mockBridge{
				requiresCompletionResult: true,
			},
			expectErr:               true,
			expectNote:              false,      // No note on error
			expectedCompletionTxIDs: []string{}, // Error before any completion
			expectedIsComplete:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, _, _, shutdown := tassetWallet(BipID)
			defer shutdown()

			ethWallet := w.(*ETHWallet)
			// Set up bridges map with the test bridge
			ethWallet.bridges = map[string]bridge{"mock": tt.bridge}

			emitChan := make(chan asset.WalletNotification, 128)
			ethWallet.emit = asset.NewWalletEmitter(emitChan, BipID, ethWallet.log)

			txDB := &tTxDB{
				txToGet:  tt.dbTx,
				txs:      tt.extraTxs,
				getTxErr: tt.dbErr,
			}
			ethWallet.txDB = txDB

			if tt.pendingTx != nil {
				ethWallet.pendingTxs = []*extendedWalletTx{tt.pendingTx}
			} else {
				ethWallet.pendingTxs = []*extendedWalletTx{}
			}

			err := ethWallet.CompleteBridge(context.Background(), initiationTx, 1e9, []byte("completionData"), "mock")

			if tt.expectErr {
				if err == nil {
					t.Fatalf("expected error but got none")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var expectedMockTxID common.Hash
			if tt.expectCompleteFollowUp {
				expectedMockTxID = tt.bridge.completeFollowUpBridgeCalled
			} else {
				expectedMockTxID = tt.bridge.completeBridgeCalled
			}
			if tt.expectComplete == (tt.bridge.completeBridgeCalled == (common.Hash{})) {
				t.Fatalf("completeBridge called = %v, want %v", tt.bridge.completeBridgeCalled, tt.expectComplete)
			}
			if tt.expectCompleteFollowUp == (tt.bridge.completeFollowUpBridgeCalled == (common.Hash{})) {
				t.Fatalf("completeFollowUpBridge called = %v, want %v", tt.bridge.completeFollowUpBridgeCalled, tt.expectCompleteFollowUp)
			}

			// Check if notification was emitted based on expectNote
			var note *asset.BridgeCompletedNote
		noteLoop:
			for {
				select {
				case n := <-emitChan:
					if n, ok := n.(*asset.BridgeCompletedNote); ok {
						note = n
						break noteLoop
					}
				default:
					break noteLoop
				}
			}

			if tt.expectNote {
				if note == nil {
					t.Fatalf("expected BridgeCompletedNote to be emitted, but got none")
				}
			} else {
				if note != nil {
					t.Fatalf("expected no BridgeCompletedNote to be emitted, but got one: %+v", note)
				}
				return
			}

			expectedInitiationTxID := initiationTx.IDs[0]
			if note.InitiationTxID != expectedInitiationTxID {
				t.Fatalf("expected InitiationTxID = %s, got %s", expectedInitiationTxID, note.InitiationTxID)
			}

			if note.SourceAssetID != initiationTx.AssetID {
				t.Fatalf("expected SourceAssetID = %d, got %d", initiationTx.AssetID, note.SourceAssetID)
			}

			if note.Complete != tt.expectedIsComplete {
				t.Fatalf("expected Complete = %v, got %v", tt.expectedIsComplete, note.Complete)
			}

			if len(note.CompletionTxIDs) != len(tt.expectedCompletionTxIDs) {
				t.Fatalf("expected %d completion TxIDs, got %d: %v", len(tt.expectedCompletionTxIDs), len(note.CompletionTxIDs), note.CompletionTxIDs)
			}

			for i, expectedID := range tt.expectedCompletionTxIDs {
				actualID := note.CompletionTxIDs[i]
				if expectedID == "mock-tx-id" {
					if actualID != expectedMockTxID.Hex() {
						t.Fatalf("expected mock-tx-id = %s, got %s", expectedMockTxID.Hex(), actualID)
					}
				} else if actualID != expectedID {
					t.Fatalf("expected CompletionTxID[%d] = %s, got %s", i, expectedID, actualID)
				}
			}

			if note.Fees != tt.expectedFees {
				t.Fatalf("expected Fees = %d, got %d", tt.expectedFees, note.Fees)
			}
		})
	}
}
