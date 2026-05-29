//go:build !spvlive && !harness

package btc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/bisoncraft/bosswallet/wallet/asset/broadcast"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func TestSwap(t *testing.T) {
	runRubric(t, testSwap)
}

func testSwap(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	swapVal := toSatoshi(5)
	coins := asset.Coins{
		NewOutput(tTxHash, 0, toSatoshi(3)),
		NewOutput(tTxHash, 0, toSatoshi(3)),
	}
	addrStr := tP2PKHAddr
	if segwit {
		addrStr = tP2WPKHAddr
	}

	node.newAddress = addrStr
	node.changeAddr = addrStr

	privBytes, _ := hex.DecodeString("b07209eec1a8fb6cfe5cb6ace36567406971a75c330db7101fb21bc679bc5330")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	wif, err := btcutil.NewWIF(privKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error encoding wif: %v", err)
	}
	node.privKeyForAddr = wif

	secretHash, _ := hex.DecodeString("5124208c80d33507befa517c08ed01aa8d33adbf37ecd70fb5f9352f7a51a88d")
	contract := &asset.Contract{
		Address:    addrStr,
		Value:      swapVal,
		SecretHash: secretHash,
		LockTime:   uint64(time.Now().Unix()),
	}

	swaps := &asset.Swaps{
		Inputs:     coins,
		Contracts:  []*asset.Contract{contract},
		LockChange: true,
		FeeRate:    tBTC.MaxFeeRate,
	}

	// Aim for 3 signature cycles.
	sigSizer := 0
	node.signFunc = func(tx *wire.MsgTx) {
		var sizeTweak int
		if sigSizer%2 == 0 {
			sizeTweak = -2
		}
		sigSizer++
		signFunc(tx, sizeTweak, wallet.segwit)
	}

	// This time should succeed.
	_, changeCoin, feesPaid, err := wallet.Swap(t.Context(), swaps)
	if err != nil {
		t.Fatalf("swap error: %v", err)
	}

	// Make sure the change coin is locked.
	if len(node.lockedCoins) != 1 {
		t.Fatalf("did not lock change coin")
	}
	txHash, _, _ := decodeCoinID(changeCoin.ID())
	if node.lockedCoins[0].TxID != txHash.String() {
		t.Fatalf("wrong coin locked during swap")
	}

	// Fees should be returned.
	minFees := tBTC.MaxFeeRate * MsgTxVBytes(node.sentRawTx)
	if feesPaid < minFees {
		t.Fatalf("sent fees, %d, less than required fees, %d", feesPaid, minFees)
	}

	// Not enough funds
	swaps.Inputs = coins[:1]
	_, _, _, err = wallet.Swap(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for listunspent not enough funds")
	}
	swaps.Inputs = coins

	// Clear the swap cache so error tests below go through the full code path.
	wallet.swapCache = broadcast.NewCache[*swapCacheEntry]()

	// AddressPKH error
	node.newAddressErr = tErr
	_, _, _, err = wallet.Swap(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for getnewaddress rpc error")
	}
	node.newAddressErr = nil

	// ChangeAddress error
	node.changeAddrErr = tErr
	_, _, _, err = wallet.Swap(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for getrawchangeaddress rpc error")
	}
	node.changeAddrErr = nil

	// SignTx error
	node.signTxErr = tErr
	_, _, _, err = wallet.Swap(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for signrawtransactionwithwallet rpc error")
	}
	node.signTxErr = nil

	// incomplete signatures
	node.sigIncomplete = true
	_, _, _, err = wallet.Swap(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for incomplete signature rpc error")
	}
	node.sigIncomplete = false

	// Make sure we can succeed again.
	_, _, _, err = wallet.Swap(t.Context(), swaps)
	if err != nil {
		t.Fatalf("re-swap error: %v", err)
	}
}

func TestRedeem(t *testing.T) {
	runRubric(t, testRedeem)
}

func testRedeem(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()
	swapVal := toSatoshi(5)

	secret, _, _, contract, addr, _, lockTime := makeSwapContract(segwit, time.Hour*12)

	coin := NewOutput(tTxHash, 0, swapVal)
	ci := &asset.AuditInfo{
		Coin:       coin,
		Contract:   contract,
		Recipient:  addr.String(),
		Expiration: lockTime,
	}

	redemption := &asset.Redemption{
		Spends: ci,
		Secret: secret,
	}

	privBytes, _ := hex.DecodeString("b07209eec1a8fb6cfe5cb6ace36567406971a75c330db7101fb21bc679bc5330")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	wif, err := btcutil.NewWIF(privKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error encoding wif: %v", err)
	}

	addrStr := tP2PKHAddr
	if segwit {
		addrStr = tP2WPKHAddr
	}

	node.changeAddr = addrStr
	node.newAddress = addrStr
	node.privKeyForAddr = wif

	redemptions := &asset.RedeemForm{
		Redemptions: []*asset.Redemption{redemption},
	}

	_, _, feesPaid, err := wallet.Redeem(t.Context(), redemptions)
	if err != nil {
		t.Fatalf("redeem error: %v", err)
	}

	// Check that fees are returned.
	minFees := optimalFeeRate * MsgTxVBytes(node.sentRawTx)
	if feesPaid < minFees {
		t.Fatalf("sent fees, %d, less than expected minimum fees, %d", feesPaid, minFees)
	}

	// No audit info
	redemption.Spends = nil
	_, _, _, err = wallet.Redeem(t.Context(), redemptions)
	if err == nil {
		t.Fatalf("no error for nil AuditInfo")
	}
	redemption.Spends = ci

	// Spoofing AuditInfo is not allowed.
	redemption.Spends = &asset.AuditInfo{}
	_, _, _, err = wallet.Redeem(t.Context(), redemptions)
	if err == nil {
		t.Fatalf("no error for spoofed AuditInfo")
	}
	redemption.Spends = ci

	// Clear the redeem cache so error tests below go through the full code path.
	wallet.redeemCache = broadcast.NewCache[*redeemCacheEntry]()

	// Wrong secret hash
	redemption.Secret = randBytes(32)
	_, _, _, err = wallet.Redeem(t.Context(), redemptions)
	if err == nil {
		t.Fatalf("no error for wrong secret")
	}
	redemption.Secret = secret

	// too low of value
	coin.Val = 200
	_, _, _, err = wallet.Redeem(t.Context(), redemptions)
	if err == nil {
		t.Fatalf("no error for redemption not worth the fees")
	}
	coin.Val = swapVal

	// New address error
	node.newAddressErr = tErr
	_, _, _, err = wallet.Redeem(t.Context(), redemptions)
	if err == nil {
		t.Fatalf("no error for new address error")
	}
	node.newAddressErr = nil

	// Missing priv key error
	node.privKeyForAddrErr = tErr
	_, _, _, err = wallet.Redeem(t.Context(), redemptions)
	if err == nil {
		t.Fatalf("no error for missing private key")
	}
	node.privKeyForAddrErr = nil

	// Send error
	node.sendErr = tErr
	_, _, _, err = wallet.Redeem(t.Context(), redemptions)
	if err == nil {
		t.Fatalf("no error for send error")
	}
	node.sendErr = nil

	// Wrong hash
	var h chainhash.Hash
	h[0] = 0x01
	node.badSendHash = &h
	_, _, _, err = wallet.Redeem(t.Context(), redemptions)
	if err == nil {
		t.Fatalf("no error for wrong return hash")
	}
	node.badSendHash = nil
}

func TestAuditContract(t *testing.T) {
	runRubric(t, testAuditContract)
}

func testAuditContract(t *testing.T, segwit bool, walletType string) {
	wallet, _, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()
	secretHash, _ := hex.DecodeString("5124208c80d33507befa517c08ed01aa8d33adbf37ecd70fb5f9352f7a51a88d")
	lockTime := time.Now().Add(time.Hour * 12)
	addr, _ := btcutil.DecodeAddress(tP2PKHAddr, &chaincfg.MainNetParams)
	if segwit {
		addr, _ = btcutil.DecodeAddress(tP2WPKHAddr, &chaincfg.MainNetParams)
	}

	contract, err := MakeContract(addr, addr, secretHash, lockTime.Unix(), segwit, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("error making swap contract: %v", err)
	}

	var contractAddr btcutil.Address
	if segwit {
		h := sha256.Sum256(contract)
		contractAddr, _ = btcutil.NewAddressWitnessScriptHash(h[:], &chaincfg.MainNetParams)
	} else {
		contractAddr, _ = btcutil.NewAddressScriptHash(contract, &chaincfg.MainNetParams)
	}
	pkScript, _ := txscript.PayToAddrScript(contractAddr)
	tx := makeRawTx([]util.Bytes{pkScript}, []*wire.TxIn{dummyInput()})
	txData, err := serializeMsgTx(tx)
	if err != nil {
		t.Fatalf("error making contract tx data: %v", err)
	}

	txHash := tx.TxHash()
	const vout = 0

	audit, err := wallet.AuditContract(ToCoinID(&txHash, vout), contract, txData, false)
	if err != nil {
		t.Fatalf("audit error: %v", err)
	}
	if audit.Recipient != addr.String() {
		t.Fatalf("wrong recipient. wanted '%s', got '%s'", addr, audit.Recipient)
	}
	if !bytes.Equal(audit.Contract, contract) {
		t.Fatalf("contract not set to coin redeem script")
	}
	if audit.Expiration.Equal(lockTime) {
		t.Fatalf("wrong lock time. wanted %d, got %d", lockTime.Unix(), audit.Expiration.Unix())
	}

	// Invalid txid
	_, err = wallet.AuditContract(make([]byte, 15), contract, txData, false)
	if err == nil {
		t.Fatalf("no error for bad txid")
	}

	// Wrong contract
	pkh, _ := hex.DecodeString("c6a704f11af6cbee8738ff19fc28cdc70aba0b82")
	wrongAddr, _ := btcutil.NewAddressPubKeyHash(pkh, &chaincfg.MainNetParams)
	badContract, _ := txscript.PayToAddrScript(wrongAddr)
	_, err = wallet.AuditContract(ToCoinID(&txHash, vout), badContract, nil, false)
	if err == nil {
		t.Fatalf("no error for wrong contract")
	}
}

func TestFindRedemption(t *testing.T) {
	runRubric(t, testFindRedemption)
}

func testFindRedemption(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	contractHeight := node.GetBestBlockHeight() + 1
	otherTxid := "7a7b3b5c3638516bc8e7f19b4a3dec00f052a599fed5036c2b89829de2367bb6"
	otherTxHash, _ := chainhash.NewHashFromStr(otherTxid)
	contractVout := uint32(1)

	secret, _, pkScript, contract, addr, _, _ := makeSwapContract(segwit, time.Hour*12)
	otherScript, _ := txscript.PayToAddrScript(addr)

	var redemptionWitness, otherWitness [][]byte
	var redemptionSigScript, otherSigScript []byte
	if segwit {
		redemptionWitness = RedeemP2WSHContract(contract, randBytes(73), randBytes(33), secret)
		otherWitness = [][]byte{randBytes(73), randBytes(33)}
	} else {
		redemptionSigScript, _ = RedeemP2SHContract(contract, randBytes(73), randBytes(33), secret)
		otherSigScript, _ = txscript.NewScriptBuilder().
			AddData(randBytes(73)).
			AddData(randBytes(33)).
			Script()
	}

	// Prepare the "blockchain"
	inputs := []*wire.TxIn{makeRPCVin(otherTxHash, 0, otherSigScript, otherWitness)}
	// Add the contract transaction. Put the pay-to-contract script at index 1.
	contractTx := makeRawTx([]util.Bytes{otherScript, pkScript}, inputs)
	contractTxHash := contractTx.TxHash()
	coinID := ToCoinID(&contractTxHash, contractVout)
	blockHash, _ := node.addRawTx(contractHeight, contractTx)
	txHex, err := makeTxHex([]util.Bytes{otherScript, pkScript}, inputs)
	if err != nil {
		t.Fatalf("error generating hex for contract tx: %v", err)
	}
	getTxRes := &GetTransactionResult{
		BlockHash:  blockHash.String(),
		BlockIndex: contractHeight,
		Bytes:      txHex,
	}
	node.getTransactionMap = map[string]*GetTransactionResult{
		"any": getTxRes}

	// Add an intermediate block for good measure.
	node.addRawTx(contractHeight+1, makeRawTx([]util.Bytes{otherScript}, inputs))

	// Now add the redemption.
	redeemVin := makeRPCVin(&contractTxHash, contractVout, redemptionSigScript, redemptionWitness)
	inputs = append(inputs, redeemVin)
	redeemBlockHash, _ := node.addRawTx(contractHeight+2, makeRawTx([]util.Bytes{otherScript}, inputs))
	node.getCFilterScripts[*redeemBlockHash] = [][]byte{pkScript}

	// Update currentTip from "RPC". Normally run() would do this.
	wallet.reportNewTip(tCtx, &BlockVector{
		Hash:   *redeemBlockHash,
		Height: contractHeight + 2,
	})

	// Check find redemption result.
	_, checkSecret, err := wallet.FindRedemption(tCtx, coinID, nil)
	if err != nil {
		t.Fatalf("error finding redemption: %v", err)
	}
	if !bytes.Equal(checkSecret, secret) {
		t.Fatalf("wrong secret. expected %x, got %x", secret, checkSecret)
	}

	// gettransaction error
	node.getTransactionErr = tErr
	_, _, err = wallet.FindRedemption(tCtx, coinID, nil)
	if err == nil {
		t.Fatalf("no error for gettransaction rpc error")
	}
	node.getTransactionErr = nil

	// timeout finding missing redemption
	redeemVin.PreviousOutPoint.Hash = *otherTxHash
	delete(node.getCFilterScripts, *redeemBlockHash)
	timedCtx, cancel := context.WithTimeout(tCtx, 500*time.Millisecond) // 0.5 seconds is long enough
	defer cancel()
	_, k, err := wallet.FindRedemption(timedCtx, coinID, nil)
	if timedCtx.Err() == nil || k != nil {
		// Expected ctx to cancel after timeout and no secret should be found.
		t.Fatalf("unexpected result for missing redemption: secret: %v, err: %v", k, err)
	}

	node.blockchainMtx.Lock()
	redeemVin.PreviousOutPoint.Hash = contractTxHash
	node.getCFilterScripts[*redeemBlockHash] = [][]byte{pkScript}
	node.blockchainMtx.Unlock()

	// Canceled context
	deadCtx, cancelCtx := context.WithCancel(tCtx)
	cancelCtx()
	_, _, err = wallet.FindRedemption(deadCtx, coinID, nil)
	if err == nil {
		t.Fatalf("no error for canceled context")
	}

	// Expect FindRedemption to error because of bad input sig.
	node.blockchainMtx.Lock()
	redeemVin.Witness = [][]byte{randBytes(100)}
	redeemVin.SignatureScript = randBytes(100)

	node.blockchainMtx.Unlock()
	_, _, err = wallet.FindRedemption(tCtx, coinID, nil)
	if err == nil {
		t.Fatalf("no error for wrong redemption")
	}
	node.blockchainMtx.Lock()
	redeemVin.Witness = redemptionWitness
	redeemVin.SignatureScript = redemptionSigScript
	node.blockchainMtx.Unlock()

	// Sanity check to make sure it passes again.
	_, _, err = wallet.FindRedemption(tCtx, coinID, nil)
	if err != nil {
		t.Fatalf("error after clearing errors: %v", err)
	}
}

func TestRefund(t *testing.T) {
	runRubric(t, testRefund)
}

func testRefund(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	_, _, pkScript, contract, addr, _, _ := makeSwapContract(segwit, time.Hour*12)

	bigTxOut := newTxOutResult(nil, 1e8, 2)
	node.txOutRes = bigTxOut // rpc
	node.changeAddr = addr.String()
	node.newAddress = addr.String()
	const feeSuggestion = 100

	privBytes, _ := hex.DecodeString("b07209eec1a8fb6cfe5cb6ace36567406971a75c330db7101fb21bc679bc5330")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	wif, err := btcutil.NewWIF(privKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error encoding wif: %v", err)
	}
	node.privKeyForAddr = wif

	tx := makeRawTx([]util.Bytes{pkScript}, []*wire.TxIn{dummyInput()})
	const vout = 0
	tx.TxOut[vout].Value = 1e8
	txHash := tx.TxHash()
	outPt := NewOutPoint(&txHash, vout)
	blockHash, _ := node.addRawTx(1, tx)
	node.getCFilterScripts[*blockHash] = [][]byte{pkScript}
	node.getTransactionErr = WalletTransactionNotFound

	contractOutput := NewOutput(&txHash, 0, 1e8)
	_, err = wallet.Refund(t.Context(), contractOutput.ID(), contract, feeSuggestion)
	if err != nil {
		t.Fatalf("refund error: %v", err)
	}

	// Invalid coin
	badReceipt := &tReceipt{
		coin: &tCoin{id: make([]byte, 15)},
	}
	_, err = wallet.Refund(t.Context(), badReceipt.coin.id, badReceipt.Contract(), feeSuggestion)
	if err == nil {
		t.Fatalf("no error for bad receipt")
	}

	// Clear the refund cache so error tests below go through the full code path.
	wallet.refundCache = broadcast.NewCache[*refundCacheEntry]()

	ensureErr := func(tag string) {
		delete(node.checkpoints, outPt)
		_, err = wallet.Refund(t.Context(), contractOutput.ID(), contract, feeSuggestion)
		if err == nil {
			t.Fatalf("no error for %q", tag)
		}
	}

	// gettxout error
	node.txOutErr = tErr
	node.getCFilterScripts[*blockHash] = nil
	ensureErr("no utxo")
	node.getCFilterScripts[*blockHash] = [][]byte{pkScript}
	node.txOutErr = nil

	// bad contract
	badContractOutput := NewOutput(tTxHash, 0, 1e8)
	badContract := randBytes(50)
	_, err = wallet.Refund(t.Context(), badContractOutput.ID(), badContract, feeSuggestion)
	if err == nil {
		t.Fatalf("no error for bad contract")
	}

	// Too small.
	node.txOutRes = newTxOutResult(nil, 100, 2)
	tx.TxOut[0].Value = 2
	ensureErr("value < fees")
	node.txOutRes = bigTxOut
	tx.TxOut[0].Value = 1e8

	// getrawchangeaddress error
	node.newAddressErr = tErr
	ensureErr("new address error")
	node.newAddressErr = nil

	// signature error
	node.privKeyForAddrErr = tErr
	ensureErr("dumpprivkey error")
	node.privKeyForAddrErr = nil

	// send error
	node.sendErr = tErr
	ensureErr("send error")
	node.sendErr = nil

	// bad checkhash
	var badHash chainhash.Hash
	badHash[0] = 0x05
	node.badSendHash = &badHash
	ensureErr("checkhash error")
	node.badSendHash = nil

	// Sanity check that we can succeed again.
	_, err = wallet.Refund(t.Context(), contractOutput.ID(), contract, feeSuggestion)
	if err != nil {
		t.Fatalf("re-refund error: %v", err)
	}

	// TODO test spv spent
}

func TestTryRedemptionRequests(t *testing.T) {
	// runRubric(t, testTryRedemptionRequests)
	testTryRedemptionRequests(t, true, walletTypeSPV)
}

func testTryRedemptionRequests(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	const swapVout = 1

	randHash := func() *chainhash.Hash {
		var h chainhash.Hash
		copy(h[:], randBytes(32))
		return &h
	}

	otherScript, _ := txscript.PayToAddrScript(btcAddr(segwit))
	otherInput := []*wire.TxIn{makeRPCVin(randHash(), 0, randBytes(5), nil)}
	otherTx := func() *wire.MsgTx {
		return makeRawTx([]util.Bytes{otherScript}, otherInput)
	}

	addBlocks := func(n int) {
		var h int64
		// Make dummy transactions.
		for i := 0; i < n; i++ {
			node.addRawTx(h, otherTx())
			h++
		}
	}

	getTx := func(blockHeight int64, txIdx int) (*wire.MsgTx, *chainhash.Hash) {
		if blockHeight == -1 {
			// mempool
			txHash := randHash()
			tx := otherTx()
			node.mempoolTxs[*txHash] = tx
			return tx, nil
		}
		blockHash, blk := node.getBlockAtHeight(blockHeight)
		for len(blk.msgBlock.Transactions) <= txIdx {
			blk.msgBlock.Transactions = append(blk.msgBlock.Transactions, otherTx())
		}
		return blk.msgBlock.Transactions[txIdx], blockHash
	}

	type tRedeem struct {
		redeemTxIdx, redeemVin        int
		swapHeight, redeemBlockHeight int64
		notRedeemed                   bool
	}

	redeemReq := func(r *tRedeem) *FindRedemptionReq {
		var swapBlockHash *chainhash.Hash
		var swapHeight int64
		if r.swapHeight >= 0 {
			swapHeight = r.swapHeight
			swapBlockHash, _ = node.getBlockAtHeight(swapHeight)
		}

		swapTxHash := randHash()
		secret, _, pkScript, contract, _, _, _ := makeSwapContract(segwit, time.Hour*12)

		if !r.notRedeemed {
			redeemTx, redeemBlockHash := getTx(r.redeemBlockHeight, r.redeemTxIdx)

			// redemptionSigScript, _ := RedeemP2SHContract(contract, randBytes(73), randBytes(33), secret)
			for len(redeemTx.TxIn) < r.redeemVin {
				redeemTx.TxIn = append(redeemTx.TxIn, makeRPCVin(randHash(), 0, nil, nil))
			}

			var redemptionSigScript []byte
			var redemptionWitness [][]byte
			if segwit {
				redemptionWitness = RedeemP2WSHContract(contract, randBytes(73), randBytes(33), secret)
			} else {
				redemptionSigScript, _ = RedeemP2SHContract(contract, randBytes(73), randBytes(33), secret)
			}

			redeemTx.TxIn = append(redeemTx.TxIn, makeRPCVin(swapTxHash, swapVout, redemptionSigScript, redemptionWitness))
			if redeemBlockHash != nil {
				node.getCFilterScripts[*redeemBlockHash] = [][]byte{pkScript}
			}
		}

		req := &FindRedemptionReq{
			outPt:        NewOutPoint(swapTxHash, swapVout),
			blockHash:    swapBlockHash,
			blockHeight:  int32(swapHeight),
			resultChan:   make(chan *FindRedemptionResult, 1),
			pkScript:     pkScript,
			contractHash: hashContract(segwit, contract),
		}
		wallet.rf.redemptions[req.outPt] = req
		return req
	}

	type test struct {
		numBlocks        int
		startBlockHeight int64
		redeems          []*tRedeem
		forcedErr        bool
		canceledCtx      bool
	}

	isMempoolTest := func(tt *test) bool {
		for _, r := range tt.redeems {
			if r.redeemBlockHeight == -1 {
				return true
			}
		}
		return false
	}

	tests := []*test{
		{ // Normal redemption
			numBlocks: 2,
			redeems: []*tRedeem{{
				redeemBlockHeight: 1,
				redeemTxIdx:       1,
				redeemVin:         1,
			}},
		},
		{ // Mempool redemption
			numBlocks: 2,
			redeems: []*tRedeem{{
				redeemBlockHeight: -1,
				redeemTxIdx:       2,
				redeemVin:         2,
			}},
		},
		{ // A couple of redemptions, both in tip.
			numBlocks:        3,
			startBlockHeight: 1,
			redeems: []*tRedeem{{
				redeemBlockHeight: 2,
			}, {
				redeemBlockHeight: 2,
			}},
		},
		{ // A couple of redemptions, spread apart.
			numBlocks: 6,
			redeems: []*tRedeem{{
				redeemBlockHeight: 2,
			}, {
				redeemBlockHeight: 4,
			}},
		},
		{ // nil start block
			numBlocks:        5,
			startBlockHeight: -1,
			redeems: []*tRedeem{{
				swapHeight:        4,
				redeemBlockHeight: 4,
			}},
		},
		{ // A mix of mined and mempool redeems
			numBlocks:        3,
			startBlockHeight: 1,
			redeems: []*tRedeem{{
				redeemBlockHeight: -1,
			}, {
				redeemBlockHeight: 2,
			}},
		},
		{ // One found, one not found.
			numBlocks:        4,
			startBlockHeight: 1,
			redeems: []*tRedeem{{
				redeemBlockHeight: 2,
			}, {
				notRedeemed: true,
			}},
		},
		{ // One found in mempool, one not found.
			numBlocks:        4,
			startBlockHeight: 1,
			redeems: []*tRedeem{{
				redeemBlockHeight: -1,
			}, {
				notRedeemed: true,
			}},
		},
		{ // Swap not mined.
			numBlocks:        3,
			startBlockHeight: 1,
			redeems: []*tRedeem{{
				swapHeight:        -1,
				redeemBlockHeight: -1,
			}},
		},
		{ // Fatal error
			numBlocks: 2,
			forcedErr: true,
			redeems: []*tRedeem{{
				redeemBlockHeight: 1,
			}},
		},
		{ // Canceled context.
			numBlocks:   2,
			canceledCtx: true,
			redeems: []*tRedeem{{
				redeemBlockHeight: 1,
			}},
		},
	}

	for _, tt := range tests {
		// Skip tests where we're expected to see mempool in SPV.
		if walletType == walletTypeSPV && isMempoolTest(tt) {
			continue
		}

		node.truncateChains()
		wallet.rf.redemptions = make(map[OutPoint]*FindRedemptionReq)
		node.blockchainMtx.Lock()
		node.getBestBlockHashErr = nil
		if tt.forcedErr {
			node.getBestBlockHashErr = tErr
		}
		node.blockchainMtx.Unlock()
		addBlocks(tt.numBlocks)
		var startBlock *chainhash.Hash
		if tt.startBlockHeight >= 0 {
			startBlock, _ = node.getBlockAtHeight(tt.startBlockHeight)
		}

		ctx := tCtx
		if tt.canceledCtx {
			timedCtx, cancel := context.WithTimeout(tCtx, time.Second)
			ctx = timedCtx
			cancel()
		}

		reqs := make([]*FindRedemptionReq, 0, len(tt.redeems))
		for _, redeem := range tt.redeems {
			reqs = append(reqs, redeemReq(redeem))
		}

		wallet.rf.tryRedemptionRequests(ctx, startBlock, reqs)

		for i, req := range reqs {
			select {
			case res := <-req.resultChan:
				if res.err != nil {
					if !tt.forcedErr {
						t.Fatalf("result error: %v", res.err)
					}
				} else if tt.canceledCtx {
					t.Fatalf("got success with canceled context")
				}
			default:
				redeem := tt.redeems[i]
				if !redeem.notRedeemed && !tt.canceledCtx {
					t.Fatalf("redemption not found")
				}
			}
		}
	}
}

func TestGenerateUnsignedRedeemTx(t *testing.T) {
	runRubric(t, testGenerateUnsignedRedeemTx)
}

func testGenerateUnsignedRedeemTx(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// Test data
	swapVal := 5 * tLotSize
	lockTime := time.Now().Add(time.Hour * 12)

	// Create a private contract
	privateContract := &asset.PrivateContract{
		LockTime:        uint64(lockTime.Unix()),
		Value:           swapVal,
		RedeemPublicKey: randBytes(33), // Compressed public key
		RefundPublicKey: randBytes(33), // Compressed public key
	}

	// Create a coin ID for the swap tx
	txHash := &chainhash.Hash{}
	copy(txHash[:], randBytes(32))
	vout := uint32(0)
	coinID := ToCoinID(txHash, vout)

	// Mock the wallet's ExternalAddress method
	if segwit {
		node.newAddress = tP2WPKHAddr
	} else {
		node.newAddress = tP2PKHAddr
	}

	// Test successful generation
	feeRate := uint64(10)
	unsignedRedeemTx, err := wallet.GenerateUnsignedRedeemTx(coinID, privateContract, feeRate)
	if err != nil {
		t.Fatalf("GenerateUnsignedRedeemTx error: %v", err)
	}

	// Deserialize and verify the transaction structure
	msgTx, err := msgTxFromBytes(unsignedRedeemTx)
	if err != nil {
		t.Fatalf("failed to deserialize unsigned redeem tx: %v", err)
	}

	// Check that the transaction has exactly one input that references
	// the correct outpoint
	if len(msgTx.TxIn) != 1 {
		t.Fatalf("expected 1 input, got %d", len(msgTx.TxIn))
	}
	input := msgTx.TxIn[0]
	if !input.PreviousOutPoint.Hash.IsEqual(txHash) {
		t.Fatalf("input hash mismatch. expected %v, got %v", txHash, input.PreviousOutPoint.Hash)
	}
	if input.PreviousOutPoint.Index != vout {
		t.Fatalf("input vout mismatch. expected %d, got %d", vout, input.PreviousOutPoint.Index)
	}

	// Check that the transaction has exactly one output that pays to the
	// redeem address
	if len(msgTx.TxOut) != 1 {
		t.Fatalf("expected 1 output, got %d", len(msgTx.TxOut))
	}
	// Check that the output value is correct (contract value minus fees)
	expectedTxSize := MinimumTxOverhead + 1 + TxInOverhead + P2WPKHOutputSize
	expectedTxSize += (PrivateRedeemWitnessSize + 2 + 3) / 4
	expectedFee := feeRate * uint64(expectedTxSize)
	expectedOutputValue := privateContract.Value - expectedFee
	output := msgTx.TxOut[0]
	if uint64(output.Value) != expectedOutputValue {
		t.Fatalf("output value mismatch. expected %d, got %d", expectedOutputValue, output.Value)
	}

	// Test with invalid coin ID
	invalidCoinID := []byte{0x01, 0x02, 0x03} // Too short
	_, err = wallet.GenerateUnsignedRedeemTx(invalidCoinID, privateContract, feeRate)
	if err == nil {
		t.Fatalf("expected error for invalid coin ID")
	}

	// Test with fee rate that makes the transaction not worth the fees
	// Calculate a fee rate that would make the total fees exceed the contract value
	maxFeeRate := (privateContract.Value / uint64(expectedTxSize))
	_, err = wallet.GenerateUnsignedRedeemTx(coinID, privateContract, maxFeeRate)
	if err != nil {
		t.Fatalf("unexpected error for max fee rate: %v", err)
	}
	_, err = wallet.GenerateUnsignedRedeemTx(coinID, privateContract, maxFeeRate+1)
	if err == nil {
		t.Fatalf("expected error for fee rate too high")
	}

	// Test with ExternalAddress error
	node.newAddressErr = tErr
	_, err = wallet.GenerateUnsignedRedeemTx(coinID, privateContract, maxFeeRate)
	if err == nil {
		t.Fatalf("expected error for ExternalAddress failure")
	}
	node.newAddressErr = nil

	// Test with zero fee rate
	_, err = wallet.GenerateUnsignedRedeemTx(coinID, privateContract, 0)
	if err != nil {
		t.Fatalf("unexpected error for zero fee rate: %v", err)
	}
}
