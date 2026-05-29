//go:build !spvlive && !harness

package btc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/bisoncraft/bosswallet/util"
	"github.com/bisoncraft/bosswallet/wallet/asset"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func TestPrivateSwap(t *testing.T) {
	runRubric(t, testPrivateSwap)
}

func testPrivateSwap(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// Setup a tx db for private swap functionality
	tempDir := t.TempDir()
	tLogger := util.StdOutLogger("TXDB", util.LevelTrace)
	txDB := NewBadgerTxDB(tempDir, tLogger)
	_, err := txDB.Connect(context.Background())
	if err != nil {
		t.Fatalf("error connecting to txDB: %v", err)
	}
	wallet.txHistoryDB.Store(txDB)

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

	// Generate random pub keys and nonces
	redeemPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating redeem private key: %v", err)
	}
	refundPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating refund private key: %v", err)
	}
	redeemPubKey := &privateSwapPubKey{
		pubKey: redeemPrivKey.PubKey(),
	}
	refundPubKey := &privateSwapPubKey{
		pubKey: refundPrivKey.PubKey(),
	}
	copy(redeemPubKey.pubNonce[:], randBytes(66))
	copy(refundPubKey.pubNonce[:], randBytes(66))
	redeemPubKeyAndNonce, err := redeemPubKey.encode()
	if err != nil {
		t.Fatalf("error encoding redeem pub key: %v", err)
	}
	refundPubKeyAndNonce, err := refundPubKey.encode()
	if err != nil {
		t.Fatalf("error encoding refund pub key: %v", err)
	}

	// Return the refund priv key when calling PrivKeyForAddr
	wif, err := btcutil.NewWIF(refundPrivKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error encoding wif: %v", err)
	}
	node.privKeyForAddr = wif

	contract := &asset.PrivateContract{
		LockTime:        uint64(time.Now().Unix()),
		Value:           swapVal,
		RedeemPublicKey: redeemPubKeyAndNonce,
		RefundPublicKey: refundPubKeyAndNonce,
	}

	swaps := &asset.PrivateSwaps{
		Version:    0,
		Inputs:     coins,
		Contracts:  []*asset.PrivateContract{contract},
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
	receipts, changeCoin, txData, feesPaid, err := wallet.SwapPrivate(t.Context(), swaps)
	if err != nil {
		t.Fatalf("private swap error: %v", err)
	}

	// Check that we got the expected number of receipts
	if len(receipts) != 1 {
		t.Fatalf("expected 1 receipt, got %d", len(receipts))
	}

	// Check that txData is not empty
	if len(txData) == 0 {
		t.Fatalf("expected non-empty tx data")
	}

	// Make sure the change coin is locked.
	if len(node.lockedCoins) != 1 {
		t.Fatalf("did not lock change coin")
	}
	txHash, _, _ := decodeCoinID(changeCoin.ID())
	if node.lockedCoins[0].TxID != txHash.String() {
		t.Fatalf("wrong coin locked during private swap")
	}

	// Fees should be returned.
	minFees := tBTC.MaxFeeRate * MsgTxVBytes(node.sentRawTx)
	if feesPaid < minFees {
		t.Fatalf("sent fees, %d, less than expected minimum fees, %d", feesPaid, minFees)
	}

	// Not enough funds
	swaps.Inputs = coins[:1]
	_, _, _, _, err = wallet.SwapPrivate(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for listunspent not enough funds")
	}
	swaps.Inputs = coins

	// AddressPKH error
	node.newAddressErr = tErr
	_, _, _, _, err = wallet.SwapPrivate(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for getnewaddress rpc error")
	}
	node.newAddressErr = nil

	// ChangeAddress error
	node.changeAddrErr = tErr
	_, _, _, _, err = wallet.SwapPrivate(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for getrawchangeaddress rpc error")
	}
	node.changeAddrErr = nil

	// SignTx error
	node.signTxErr = tErr
	_, _, _, _, err = wallet.SwapPrivate(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for signrawtransactionwithwallet rpc error")
	}
	node.signTxErr = nil

	// incomplete signatures
	node.sigIncomplete = true
	_, _, _, _, err = wallet.SwapPrivate(t.Context(), swaps)
	if err == nil {
		t.Fatalf("no error for incomplete signature rpc error")
	}
	node.sigIncomplete = false

	// Make sure we can succeed again.
	_, _, _, _, err = wallet.SwapPrivate(t.Context(), swaps)
	if err != nil {
		t.Fatalf("re-private-swap error: %v", err)
	}
}

func TestPubKeyForPrivateSwap(t *testing.T) {
	runRubric(t, testPubKeyForPrivateSwap)
}

func testPubKeyForPrivateSwap(t *testing.T, segwit bool, walletType string) {
	// Generate a random private key for testing
	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating private key: %v", err)
	}

	// Create the address from the private key
	var addr btcutil.Address
	if segwit {
		pkh := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
		addr, err = btcutil.NewAddressWitnessPubKeyHash(pkh, &chaincfg.MainNetParams)
		if err != nil {
			t.Fatalf("error creating P2WPKH address: %v", err)
		}
	} else {
		pkh := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
		addr, err = btcutil.NewAddressPubKeyHash(pkh, &chaincfg.MainNetParams)
		if err != nil {
			t.Fatalf("error creating P2PKH address: %v", err)
		}
	}

	// Create a test wallet
	wallet, testData, cleanup := tNewWallet(segwit, walletType)
	defer cleanup()

	// Setup a tx db
	tempDir := t.TempDir()
	tLogger := util.StdOutLogger("TXDB", util.LevelTrace)
	txDB := NewBadgerTxDB(tempDir, tLogger)
	_, err = txDB.Connect(context.Background())
	if err != nil {
		t.Fatalf("error connecting to txDB: %v", err)
	}
	wallet.txHistoryDB.Store(txDB)

	// Set up the test data to return our address and private key
	testData.newAddress = addr.String()
	wif, err := btcutil.NewWIF(privKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error creating WIF: %v", err)
	}
	testData.privKeyForAddr = wif
	testData.locked = false

	// Call PrivateSwapPubKey
	pubKeyAndNonce, err := wallet.PrivateSwapPubKey()
	if err != nil {
		t.Fatalf("PrivateSwapPubKey failed: %v", err)
	}

	// Decode the returned value into a privateSwapPubKey
	var ps privateSwapPubKey
	err = ps.decode(pubKeyAndNonce)
	if err != nil {
		t.Fatalf("error decoding privateSwapPubKey: %v", err)
	}

	// Check that the pubKey field matches the public key of our randomly generated private key
	if !ps.pubKey.IsEqual(privKey.PubKey()) {
		t.Errorf("public key mismatch. expected: %v, got: %v",
			hex.EncodeToString(privKey.PubKey().SerializeCompressed()),
			hex.EncodeToString(ps.pubKey.SerializeCompressed()))
	}

	// Retrieve the stored secret nonce using the public nonce. Just check
	// that a value was returned, since the sec to pub nonce musig2 function
	// is not exported.
	encryptionKey := sha256.Sum256(privKey.Serialize())
	storedSecNonce, err := txDB.GetSecNonce(ps.pubNonce[:], encryptionKey[:])
	if err != nil {
		t.Fatalf("error retrieving stored secret nonce: %v", err)
	}
	if len(storedSecNonce) != 97 {
		t.Fatalf("stored secret nonce has wrong length: %d", len(storedSecNonce))
	}
}

func TestAuditPrivateContract(t *testing.T) {
	runRubric(t, testAuditPrivateContract)
}

func testAuditPrivateContract(t *testing.T, segwit bool, walletType string) {
	wallet, _, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// Generate test private keys for redeem and refund parties
	redeemPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating redeem private key: %v", err)
	}
	refundPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating refund private key: %v", err)
	}

	// Generate nonces for musig2
	redeemNonces, err := musig2.GenNonces(musig2.WithPublicKey(redeemPrivKey.PubKey()))
	if err != nil {
		t.Fatalf("error generating redeem nonces: %v", err)
	}
	refundNonces, err := musig2.GenNonces(musig2.WithPublicKey(refundPrivKey.PubKey()))
	if err != nil {
		t.Fatalf("error generating refund nonces: %v", err)
	}

	// Create private swap public keys
	redeemPubKey := &privateSwapPubKey{
		pubKey:   redeemPrivKey.PubKey(),
		pubNonce: redeemNonces.PubNonce,
	}
	refundPubKey := &privateSwapPubKey{
		pubKey:   refundPrivKey.PubKey(),
		pubNonce: refundNonces.PubNonce,
	}

	// Encode the public keys
	redeemPubKeyEncoded, err := redeemPubKey.encode()
	if err != nil {
		t.Fatalf("error encoding redeem public key: %v", err)
	}
	refundPubKeyEncoded, err := refundPubKey.encode()
	if err != nil {
		t.Fatalf("error encoding refund public key: %v", err)
	}

	lockTime := time.Now().Add(time.Hour * 12)

	contract := &asset.PrivateContract{
		LockTime:        uint64(lockTime.Unix()),
		Value:           5 * tLotSize,
		RedeemPublicKey: redeemPubKeyEncoded,
		RefundPublicKey: refundPubKeyEncoded,
	}

	// Create the private swap output data
	outputData, err := privateSwapOutputDataFromContract(contract)
	if err != nil {
		t.Fatalf("error creating private swap output data: %v", err)
	}

	// Create the contract transaction
	contractTx := wire.NewMsgTx(wire.TxVersion)
	contractTx.AddTxIn(&wire.TxIn{})
	contractTx.AddTxOut(&wire.TxOut{
		Value:    int64(contract.Value),
		PkScript: outputData.pkScript,
	})

	// Serialize the transaction
	var contractTxData []byte
	if wallet.deserializeTx != nil {
		contractTxData, err = wallet.serializeTx(contractTx)
	} else {
		var buf bytes.Buffer
		err = contractTx.Serialize(&buf)
		contractTxData = buf.Bytes()
	}
	if err != nil {
		t.Fatalf("error serializing contract tx: %v", err)
	}

	contractHash := contractTx.TxHash()
	contractVout := uint32(0)
	contractCoinID := ToCoinID(&contractHash, contractVout)

	// Test successful audit
	err = wallet.AuditPrivateContract(contractCoinID, contractTxData, contract, true)
	if err != nil {
		t.Fatalf("audit error: %v", err)
	}

	// Test invalid coinID
	err = wallet.AuditPrivateContract(make([]byte, 15), contractTxData, contract, false)
	if err == nil {
		t.Fatalf("no error for bad coinID")
	}

	// Test wrong redeem public key
	wrongRedeemPrivKey, _ := btcec.NewPrivateKey()
	wrongRedeemNonces, _ := musig2.GenNonces(musig2.WithPublicKey(wrongRedeemPrivKey.PubKey()))
	wrongRedeemPubKey := &privateSwapPubKey{
		pubKey:   wrongRedeemPrivKey.PubKey(),
		pubNonce: wrongRedeemNonces.PubNonce,
	}
	wrongRedeemPubKeyEncoded, _ := wrongRedeemPubKey.encode()
	wrongContract := &asset.PrivateContract{
		LockTime:        uint64(lockTime.Unix()),
		Value:           5 * tLotSize,
		RedeemPublicKey: wrongRedeemPubKeyEncoded,
		RefundPublicKey: refundPubKeyEncoded,
	}
	err = wallet.AuditPrivateContract(contractCoinID, contractTxData, wrongContract, false)
	if err == nil {
		t.Fatalf("no error for wrong redeem public key")
	}

	// Test wrong refund public key
	wrongRefundPrivKey, _ := btcec.NewPrivateKey()
	wrongRefundNonces, _ := musig2.GenNonces(musig2.WithPublicKey(wrongRefundPrivKey.PubKey()))
	wrongRefundPubKey := &privateSwapPubKey{
		pubKey:   wrongRefundPrivKey.PubKey(),
		pubNonce: wrongRefundNonces.PubNonce,
	}
	wrongRefundPubKeyEncoded, _ := wrongRefundPubKey.encode()
	wrongContract = &asset.PrivateContract{
		LockTime:        uint64(lockTime.Unix()),
		Value:           5 * tLotSize,
		RedeemPublicKey: redeemPubKeyEncoded,
		RefundPublicKey: wrongRefundPubKeyEncoded,
	}
	err = wallet.AuditPrivateContract(contractCoinID, contractTxData, wrongContract, false)
	if err == nil {
		t.Fatalf("no error for wrong refund public key")
	}

	// Test wrong locktime
	wrongLockTimeContract := &asset.PrivateContract{
		LockTime:        uint64(time.Now().Add(time.Hour * 24).Unix()), // Different locktime
		Value:           5 * tLotSize,
		RedeemPublicKey: redeemPubKeyEncoded,
		RefundPublicKey: refundPubKeyEncoded,
	}
	err = wallet.AuditPrivateContract(contractCoinID, contractTxData, wrongLockTimeContract, false)
	if err == nil {
		t.Fatalf("no error for wrong locktime")
	}

	// Test no txdata
	err = wallet.AuditPrivateContract(contractCoinID, nil, contract, false)
	if err == nil {
		t.Fatalf("no error for no txdata")
	}

	// Test invalid txdata, zero inputs
	contractTx.TxIn = nil
	var invalidContractTxData []byte
	if wallet.deserializeTx != nil {
		invalidContractTxData, err = wallet.serializeTx(contractTx)
	} else {
		var buf bytes.Buffer
		err = contractTx.Serialize(&buf)
		invalidContractTxData = buf.Bytes()
	}
	if err != nil {
		t.Fatalf("error preparing invalid contract txdata: %v", err)
	}
	err = wallet.AuditPrivateContract(contractCoinID, invalidContractTxData, contract, false)
	if err == nil {
		t.Fatalf("no error for invalid txdata with zero inputs")
	}

	// Test wrong txdata, wrong output script (P2PKH instead of taproot)
	pkh := btcutil.Hash160(redeemPrivKey.PubKey().SerializeCompressed())
	wrongAddr, _ := btcutil.NewAddressPubKeyHash(pkh, &chaincfg.MainNetParams)
	wrongPkScript, _ := txscript.PayToAddrScript(wrongAddr)
	wrongContractTx := wire.NewMsgTx(wire.TxVersion)
	wrongContractTx.AddTxIn(&wire.TxIn{})
	wrongContractTx.AddTxOut(&wire.TxOut{
		Value:    int64(contract.Value),
		PkScript: wrongPkScript,
	})
	var wrongContractTxData []byte
	if wallet.deserializeTx != nil {
		wrongContractTxData, err = wallet.serializeTx(wrongContractTx)
	} else {
		var buf bytes.Buffer
		err = wrongContractTx.Serialize(&buf)
		wrongContractTxData = buf.Bytes()
	}
	if err != nil {
		t.Fatalf("error preparing wrong contract txdata: %v", err)
	}
	err = wallet.AuditPrivateContract(contractCoinID, wrongContractTxData, contract, false)
	if err == nil {
		t.Fatalf("no error for wrong output script")
	}

	// Test wrong txdata, wrong hash
	wrongHash := chainhash.Hash{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20}
	wrongCoinID := ToCoinID(&wrongHash, contractVout)
	err = wallet.AuditPrivateContract(wrongCoinID, contractTxData, contract, false)
	if err == nil {
		t.Fatalf("no error for wrong tx hash")
	}

	// Test invalid vout
	invalidVoutCoinID := ToCoinID(&contractHash, 999) // Non-existent vout
	err = wallet.AuditPrivateContract(invalidVoutCoinID, contractTxData, contract, false)
	if err == nil {
		t.Fatalf("no error for invalid vout")
	}

	// Test wrong value
	wrongValueContract := &asset.PrivateContract{
		LockTime:        uint64(lockTime.Unix()),
		Value:           10 * tLotSize, // Different value
		RedeemPublicKey: redeemPubKeyEncoded,
		RefundPublicKey: refundPubKeyEncoded,
	}
	err = wallet.AuditPrivateContract(contractCoinID, contractTxData, wrongValueContract, false)
	if err == nil {
		t.Fatalf("no error for wrong value")
	}
}

func TestGeneratePrivateKeyTweakedAdaptor(t *testing.T) {
	runRubric(t, testGeneratePrivateKeyTweakedAdaptor)
}

func testGeneratePrivateKeyTweakedAdaptor(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// Add a txDB to the wallet
	tempDir := t.TempDir()
	txDB := NewBadgerTxDB(tempDir, tLogger)
	_, err := txDB.Connect(context.Background())
	if err != nil {
		t.Fatalf("error connecting to txDB: %v", err)
	}
	wallet.txHistoryDB.Store(txDB)

	// Generate two random private keys for redeem and refund parties
	redeemPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("failed to generate redeem private key: %v", err)
	}
	refundPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("failed to generate refund private key: %v", err)
	}

	// Generate nonces for musig2
	redeemNonces, err := musig2.GenNonces(musig2.WithPublicKey(redeemPrivKey.PubKey()))
	if err != nil {
		t.Fatalf("failed to generate redeem nonces: %v", err)
	}
	refundNonces, err := musig2.GenNonces(musig2.WithPublicKey(refundPrivKey.PubKey()))
	if err != nil {
		t.Fatalf("failed to generate refund nonces: %v", err)
	}

	// Create private swap public keys
	redeemPubKey := &privateSwapPubKey{
		pubKey:   redeemPrivKey.PubKey(),
		pubNonce: redeemNonces.PubNonce,
	}
	refundPubKey := &privateSwapPubKey{
		pubKey:   refundPrivKey.PubKey(),
		pubNonce: refundNonces.PubNonce,
	}

	// Encode the public keys
	redeemPubKeyEncoded, err := redeemPubKey.encode()
	if err != nil {
		t.Fatalf("failed to encode redeem public key: %v", err)
	}
	refundPubKeyEncoded, err := refundPubKey.encode()
	if err != nil {
		t.Fatalf("failed to encode refund public key: %v", err)
	}

	// Create a private contract
	lockTime := time.Now().Add(time.Hour * 12)
	contract := &asset.PrivateContract{
		LockTime:        uint64(lockTime.Unix()),
		Value:           5 * tLotSize,
		RedeemPublicKey: redeemPubKeyEncoded,
		RefundPublicKey: refundPubKeyEncoded,
	}

	// Generate a dummy unsigned redeem transaction
	redeemTx := wire.NewMsgTx(wire.TxVersion)
	txHash := &chainhash.Hash{}
	copy(txHash[:], randBytes(32))
	redeemTx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(txHash, 0), nil, nil))
	pkh := randBytes(20)
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pkh, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("failed to create address: %v", err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("failed to create pkScript: %v", err)
	}
	feeRate := uint64(10)
	txSize := redeemTx.SerializeSize() + (PrivateRedeemWitnessSize+2+3)/4 + P2WPKHOutputSize
	fee := feeRate * uint64(txSize)
	outputValue := contract.Value - fee
	redeemTx.AddTxOut(&wire.TxOut{
		Value:    int64(outputValue),
		PkScript: pkScript,
	})
	unsignedRedeemB, err := serializeMsgTx(redeemTx)
	if err != nil {
		t.Fatalf("failed to serialize redeem tx: %v", err)
	}

	// Generate a random adaptor secret
	var adaptorSecret *btcec.ModNScalar
	for {
		adaptorSecret = &btcec.ModNScalar{}
		adaptorSecretBytes := randBytes(32)
		adaptorSecret.SetByteSlice(adaptorSecretBytes)
		valid, err := wallet.ValidateAdaptorSecret(adaptorSecret, unsignedRedeemB, contract)
		if err != nil {
			t.Fatalf("failed to validate adaptor secret: %v", err)
		}
		if valid {
			break
		}
	}

	// Store the secret nonces in the txDB for both keys
	redeemEncryptionKey := sha256.Sum256(redeemPrivKey.Serialize())
	refundEncryptionKey := sha256.Sum256(refundPrivKey.Serialize())
	err = txDB.StoreSecNonce(redeemPubKey.pubNonce[:], redeemNonces.SecNonce[:], redeemEncryptionKey[:])
	if err != nil {
		t.Fatalf("failed to store redeem secret nonce: %v", err)
	}
	err = txDB.StoreSecNonce(refundPubKey.pubNonce[:], refundNonces.SecNonce[:], refundEncryptionKey[:])
	if err != nil {
		t.Fatalf("failed to store refund secret nonce: %v", err)
	}

	// Override the PrivKeyForAddress method to return the redeemer's private key
	redeemWif, err := btcutil.NewWIF(redeemPrivKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error encoding wif: %v", err)
	}
	node.privKeyForAddr = redeemWif
	node.privKeyForAddrErr = nil

	// Test as redeemer
	adaptorSigB, err := wallet.GeneratePrivateKeyTweakedAdaptor(unsignedRedeemB, contract, adaptorSecret, true)
	if err != nil {
		t.Fatalf("GeneratePrivateKeyTweakedAdaptor error: %v", err)
	}

	// Verify the adaptor signature
	adaptorSig := new(musig2.PartialSignature)
	err = adaptorSig.Decode(bytes.NewReader(adaptorSigB))
	if err != nil {
		t.Fatalf("failed to parse adaptor signature: %v", err)
	}
	outputData, err := privateSwapOutputDataFromContract(contract)
	if err != nil {
		t.Fatalf("failed to create contract output data: %v", err)
	}
	sigHash, err := tapRootSigHash(outputData.pkScript, contract.Value, redeemTx)
	if err != nil {
		t.Fatalf("failed to calculate signature hash: %v", err)
	}
	combinedNonce, err := musig2.AggregateNonces([][66]byte{
		outputData.redeemPubKey.pubNonce,
		outputData.refundPubKey.pubNonce,
	})
	if err != nil {
		t.Fatalf("failed to aggregate nonces: %v", err)
	}
	adaptorPub := new(btcec.JacobianPoint)
	btcec.ScalarBaseMultNonConst(adaptorSecret, adaptorPub)
	valid := adaptorSig.Verify(
		outputData.redeemPubKey.pubNonce,
		combinedNonce,
		[]*btcec.PublicKey{outputData.redeemPubKey.pubKey, outputData.refundPubKey.pubKey},
		outputData.redeemPubKey.pubKey,
		sigHash,
		musig2.WithTaprootSignTweak(outputData.tapscriptRootHash),
		musig2.WithAdaptorSign(adaptorPub),
		musig2.WithSortedKeys(),
	)
	if !valid {
		t.Fatalf("adaptor signature verification failed")
	}

	// Validate the adaptor signature using the redeemer public key
	valid, err = wallet.ValidateAdaptorSig(unsignedRedeemB, adaptorSigB, adaptorPub, contract, true)
	if err != nil {
		t.Fatalf("failed to validate adaptor signature: %v", err)
	}
	if !valid {
		t.Fatalf("adaptor signature validation failed")
	}

	// Make sure ValidateAdaptorSig fails to validate using the incorrect
	// public key.
	valid, err = wallet.ValidateAdaptorSig(unsignedRedeemB, adaptorSigB, adaptorPub, contract, false)
	if err != nil {
		t.Fatalf("failed to validate adaptor signature: %v", err)
	}
	if valid {
		t.Fatalf("adaptor signature validation for redeemer should have failed using refunder public key")
	}

	// Test as refunder (using refund public key)
	// Override PrivKeyForAddress for refund address
	refundWif, err := btcutil.NewWIF(refundPrivKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error encoding wif: %v", err)
	}
	node.privKeyForAddr = refundWif
	node.privKeyForAddrErr = nil

	adaptorSigB2, err := wallet.GeneratePrivateKeyTweakedAdaptor(unsignedRedeemB, contract, adaptorSecret, false)
	if err != nil {
		t.Fatalf("GeneratePrivateKeyTweakedAdaptor as refunder error: %v", err)
	}

	// Verify the second adaptor signature
	adaptorSig2 := new(musig2.PartialSignature)
	err = adaptorSig2.Decode(bytes.NewReader(adaptorSigB2))
	if err != nil {
		t.Fatalf("failed to parse second adaptor signature: %v", err)
	}

	valid = adaptorSig2.Verify(
		outputData.refundPubKey.pubNonce,
		combinedNonce,
		[]*btcec.PublicKey{outputData.redeemPubKey.pubKey, outputData.refundPubKey.pubKey},
		outputData.refundPubKey.pubKey,
		sigHash,
		musig2.WithTaprootSignTweak(outputData.tapscriptRootHash),
		musig2.WithAdaptorSign(adaptorPub),
		musig2.WithSortedKeys(),
	)
	if !valid {
		t.Fatalf("second adaptor signature verification failed")
	}

	// Make sure ValidateAdaptorSig correctly validates
	valid, err = wallet.ValidateAdaptorSig(unsignedRedeemB, adaptorSigB2, adaptorPub, contract, false)
	if err != nil {
		t.Fatalf("failed to validate adaptor signature: %v", err)
	}
	if !valid {
		t.Fatalf("adaptor signature validation failed")
	}

	// Make sure ValidateAdaptorSig fails to validate using the incorrect
	// public key.
	valid, err = wallet.ValidateAdaptorSig(unsignedRedeemB, adaptorSigB2, adaptorPub, contract, true)
	if err != nil {
		t.Fatalf("failed to validate adaptor signature: %v", err)
	}
	if valid {
		t.Fatalf("adaptor signature validation for refunder should have failed using redeemer public key")
	}

	// Test error cases
	// Test with invalid unsigned redeem transaction
	_, err = wallet.GeneratePrivateKeyTweakedAdaptor([]byte{0x01, 0x02, 0x03}, contract, adaptorSecret, true)
	if err == nil {
		t.Fatalf("expected error for invalid unsigned redeem transaction")
	}

	// Test with invalid public key in contract
	invalidContract := &asset.PrivateContract{
		LockTime:        uint64(lockTime.Unix()),
		Value:           5 * tLotSize,
		RedeemPublicKey: []byte{0x01, 0x02, 0x03}, // Invalid public key
		RefundPublicKey: refundPubKeyEncoded,
	}
	_, err = wallet.GeneratePrivateKeyTweakedAdaptor(unsignedRedeemB, invalidContract, adaptorSecret, true)
	if err == nil {
		t.Fatalf("expected error for invalid public key in contract")
	}

	// Test with address not found in wallet
	node.privKeyForAddrErr = fmt.Errorf("address not found")
	_, err = wallet.GeneratePrivateKeyTweakedAdaptor(unsignedRedeemB, contract, adaptorSecret, true)
	if err == nil {
		t.Fatalf("expected error when address not found in wallet")
	}
	node.privKeyForAddrErr = nil

	// Test with secret nonce not found in txDB
	wrongEncryptionKey := sha256.Sum256([]byte("wrong key"))
	err = txDB.StoreSecNonce(redeemPubKey.pubNonce[:], redeemNonces.SecNonce[:], wrongEncryptionKey[:])
	if err != nil {
		t.Fatalf("failed to store wrong secret nonce: %v", err)
	}

	// This should fail because the encryption key doesn't match
	_, err = wallet.GeneratePrivateKeyTweakedAdaptor(unsignedRedeemB, contract, adaptorSecret, true)
	if err == nil {
		t.Fatalf("expected error when secret nonce not found in txDB")
	}

	// Restore the correct secret nonce
	err = txDB.StoreSecNonce(redeemPubKey.pubNonce[:], redeemNonces.SecNonce[:], redeemEncryptionKey[:])
	if err != nil {
		t.Fatalf("failed to restore redeem secret nonce: %v", err)
	}
}

func TestGeneratePublicKeyTweakedAdaptor(t *testing.T) {
	runRubric(t, testGeneratePublicKeyTweakedAdaptor)
}

func testGeneratePublicKeyTweakedAdaptor(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// Add a txDB to the wallet
	tempDir := t.TempDir()
	txDB := NewBadgerTxDB(tempDir, tLogger)
	_, err := txDB.Connect(context.Background())
	if err != nil {
		t.Fatalf("error connecting to txDB: %v", err)
	}
	wallet.txHistoryDB.Store(txDB)

	// Generate two random private keys for redeem and refund parties
	redeemPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("failed to generate redeem private key: %v", err)
	}
	refundPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("failed to generate refund private key: %v", err)
	}

	// Generate nonces for musig2
	redeemNonces, err := musig2.GenNonces(musig2.WithPublicKey(redeemPrivKey.PubKey()))
	if err != nil {
		t.Fatalf("failed to generate redeem nonces: %v", err)
	}
	refundNonces, err := musig2.GenNonces(musig2.WithPublicKey(refundPrivKey.PubKey()))
	if err != nil {
		t.Fatalf("failed to generate refund nonces: %v", err)
	}

	// Create private swap public keys
	redeemPubKey := &privateSwapPubKey{
		pubKey:   redeemPrivKey.PubKey(),
		pubNonce: redeemNonces.PubNonce,
	}
	refundPubKey := &privateSwapPubKey{
		pubKey:   refundPrivKey.PubKey(),
		pubNonce: refundNonces.PubNonce,
	}

	// Encode the public keys
	redeemPubKeyEncoded, err := redeemPubKey.encode()
	if err != nil {
		t.Fatalf("failed to encode redeem public key: %v", err)
	}
	refundPubKeyEncoded, err := refundPubKey.encode()
	if err != nil {
		t.Fatalf("failed to encode refund public key: %v", err)
	}

	// Create a private contract
	lockTime := time.Now().Add(time.Hour * 12)
	contract := &asset.PrivateContract{
		LockTime:        uint64(lockTime.Unix()),
		Value:           5 * tLotSize,
		RedeemPublicKey: redeemPubKeyEncoded,
		RefundPublicKey: refundPubKeyEncoded,
	}

	// Generate a dummy unsigned redeem transaction
	redeemTx := wire.NewMsgTx(wire.TxVersion)
	txHash := &chainhash.Hash{}
	copy(txHash[:], randBytes(32))
	redeemTx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(txHash, 0), nil, nil))
	pkh := randBytes(20)
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pkh, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("failed to create address: %v", err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("failed to create pkScript: %v", err)
	}
	feeRate := uint64(10)
	txSize := redeemTx.SerializeSize() + (PrivateRedeemWitnessSize+2+3)/4 + P2WPKHOutputSize
	fee := feeRate * uint64(txSize)
	outputValue := contract.Value - fee
	redeemTx.AddTxOut(&wire.TxOut{
		Value:    int64(outputValue),
		PkScript: pkScript,
	})
	unsignedRedeemB, err := serializeMsgTx(redeemTx)
	if err != nil {
		t.Fatalf("failed to serialize redeem tx: %v", err)
	}

	// Generate a random adaptor secret and public key
	var adaptorSecret *btcec.ModNScalar
	for {
		adaptorSecret = &btcec.ModNScalar{}
		adaptorSecretBytes := randBytes(32)
		adaptorSecret.SetByteSlice(adaptorSecretBytes)
		valid, err := wallet.ValidateAdaptorSecret(adaptorSecret, unsignedRedeemB, contract)
		if err != nil {
			t.Fatalf("failed to validate adaptor secret: %v", err)
		}
		if valid {
			break
		}
	}

	// Create the adaptor public key (T = t * G)
	adaptorPub := new(btcec.JacobianPoint)
	btcec.ScalarBaseMultNonConst(adaptorSecret, adaptorPub)

	// Store the secret nonces in the txDB for both keys
	redeemEncryptionKey := sha256.Sum256(redeemPrivKey.Serialize())
	refundEncryptionKey := sha256.Sum256(refundPrivKey.Serialize())
	err = txDB.StoreSecNonce(redeemPubKey.pubNonce[:], redeemNonces.SecNonce[:], redeemEncryptionKey[:])
	if err != nil {
		t.Fatalf("failed to store redeem secret nonce: %v", err)
	}
	err = txDB.StoreSecNonce(refundPubKey.pubNonce[:], refundNonces.SecNonce[:], refundEncryptionKey[:])
	if err != nil {
		t.Fatalf("failed to store refund secret nonce: %v", err)
	}

	// Override the PrivKeyForAddress method to return the refunder's private key
	// (GeneratePublicKeyTweakedAdaptor is called by the party who doesn't know the secret)
	refundWif, err := btcutil.NewWIF(refundPrivKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error encoding wif: %v", err)
	}
	node.privKeyForAddr = refundWif
	node.privKeyForAddrErr = nil

	// Generate the public key tweaked adaptor signature
	adaptorSigB, err := wallet.GeneratePublicKeyTweakedAdaptor(unsignedRedeemB, contract, adaptorPub)
	if err != nil {
		t.Fatalf("GeneratePublicKeyTweakedAdaptor error: %v", err)
	}

	// Verify the adaptor signature
	adaptorSig := new(musig2.PartialSignature)
	err = adaptorSig.Decode(bytes.NewReader(adaptorSigB))
	if err != nil {
		t.Fatalf("failed to parse adaptor signature: %v", err)
	}

	// Verify the adaptor signature using musig2
	outputData, err := privateSwapOutputDataFromContract(contract)
	if err != nil {
		t.Fatalf("failed to create contract output data: %v", err)
	}
	sigHash, err := tapRootSigHash(outputData.pkScript, contract.Value, redeemTx)
	if err != nil {
		t.Fatalf("failed to calculate signature hash: %v", err)
	}
	combinedNonce, err := musig2.AggregateNonces([][66]byte{
		outputData.redeemPubKey.pubNonce,
		outputData.refundPubKey.pubNonce,
	})
	if err != nil {
		t.Fatalf("failed to aggregate nonces: %v", err)
	}

	valid := adaptorSig.Verify(
		outputData.refundPubKey.pubNonce,
		combinedNonce,
		[]*btcec.PublicKey{outputData.redeemPubKey.pubKey, outputData.refundPubKey.pubKey},
		outputData.refundPubKey.pubKey,
		sigHash,
		musig2.WithTaprootSignTweak(outputData.tapscriptRootHash),
		musig2.WithAdaptorSign(adaptorPub),
		musig2.WithSortedKeys(),
	)
	if !valid {
		t.Fatalf("adaptor signature verification failed")
	}
}

func TestSecretNonceCleanup(t *testing.T) {
	runRubric(t, testSecretNonceCleanup)
}

func testSecretNonceCleanup(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// Setup a tx db for private swap functionality
	tempDir := t.TempDir()
	tLogger := util.StdOutLogger("TXDB", util.LevelTrace)
	txDB := NewBadgerTxDB(tempDir, tLogger)
	_, err := txDB.Connect(context.Background())
	if err != nil {
		t.Fatalf("error connecting to txDB: %v", err)
	}
	wallet.txHistoryDB.Store(txDB)

	// Generate redeem priv key, address, and WIF
	redeemPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating redeem private key: %v", err)
	}
	redeemAddr, err := pubKeyToP2WPKHAddress(redeemPrivKey.PubKey(), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("error creating redeem address: %v", err)
	}
	redeemWIF, err := btcutil.NewWIF(redeemPrivKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error creating redeem WIF: %v", err)
	}

	// Generate refund priv key, address, and WIF
	refundPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating refund private key: %v", err)
	}
	refundAddr, err := pubKeyToP2WPKHAddress(refundPrivKey.PubKey(), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("error creating refund address: %v", err)
	}
	refundWIF, err := btcutil.NewWIF(refundPrivKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error creating refund WIF: %v", err)
	}

	node.newAddress = redeemAddr.String()
	node.privKeyForAddr = redeemWIF
	redeemPubKeyAndNonce, err := wallet.PrivateSwapPubKey()
	if err != nil {
		t.Fatalf("error generating redeem pub key: %v", err)
	}

	node.newAddress = refundAddr.String()
	node.privKeyForAddr = refundWIF
	refundPubKeyAndNonce, err := wallet.PrivateSwapPubKey()
	if err != nil {
		t.Fatalf("error generating refund pub key: %v", err)
	}

	// Create a contract with both pub keys
	contract := &asset.PrivateContract{
		LockTime:        uint64(time.Now().Unix()),
		Value:           toSatoshi(5),
		RedeemPublicKey: redeemPubKeyAndNonce,
		RefundPublicKey: refundPubKeyAndNonce,
	}

	// Verify that secret nonces are stored in the txDB
	redeemEncryptionKey := sha256.Sum256(redeemPrivKey.Serialize())
	refundEncryptionKey := sha256.Sum256(refundPrivKey.Serialize())

	// Extract pub nonces from the encoded data
	var redeemPubKey privateSwapPubKey
	err = redeemPubKey.decode(redeemPubKeyAndNonce)
	if err != nil {
		t.Fatalf("error decoding redeem pub key: %v", err)
	}
	var refundPubKey privateSwapPubKey
	err = refundPubKey.decode(refundPubKeyAndNonce)
	if err != nil {
		t.Fatalf("error decoding refund pub key: %v", err)
	}

	// Verify secret nonces are stored
	_, err = txDB.GetSecNonce(redeemPubKey.pubNonce[:], redeemEncryptionKey[:])
	if err != nil {
		t.Fatalf("redeem secret nonce not found in txDB: %v", err)
	}
	_, err = txDB.GetSecNonce(refundPubKey.pubNonce[:], refundEncryptionKey[:])
	if err != nil {
		t.Fatalf("refund secret nonce not found in txDB: %v", err)
	}

	// Test MarkPrivateSwapComplete with isRedeemer = true
	// This should delete the redeem secret nonce
	wallet.MarkPrivateSwapComplete(contract, true)

	// Verify redeem secret nonce is deleted
	_, err = txDB.GetSecNonce(redeemPubKey.pubNonce[:], redeemEncryptionKey[:])
	if err == nil {
		t.Fatalf("redeem secret nonce should have been deleted but was found")
	}

	// Verify refund secret nonce is still present
	_, err = txDB.GetSecNonce(refundPubKey.pubNonce[:], refundEncryptionKey[:])
	if err != nil {
		t.Fatalf("refund secret nonce should still be present but was not found: %v", err)
	}

	// Test MarkPrivateSwapComplete with isRedeemer = false
	// This should delete the refund secret nonce
	wallet.MarkPrivateSwapComplete(contract, false)

	// Verify refund secret nonce is now deleted
	_, err = txDB.GetSecNonce(refundPubKey.pubNonce[:], refundEncryptionKey[:])
	if err == nil {
		t.Fatalf("refund secret nonce should have been deleted but was found")
	}
}

func TestRedeemPrivate(t *testing.T) {
	runRubric(t, testRedeemPrivate)
}

func testRedeemPrivate(t *testing.T, segwit bool, walletType string) {
	wallet, node, shutdown := tNewWallet(segwit, walletType)
	defer shutdown()

	// Setup a tx db for private swap functionality
	tempDir := t.TempDir()
	tLogger := util.StdOutLogger("TXDB", util.LevelTrace)
	txDB := NewBadgerTxDB(tempDir, tLogger)
	_, err := txDB.Connect(context.Background())
	if err != nil {
		t.Fatalf("error connecting to txDB: %v", err)
	}
	wallet.txHistoryDB.Store(txDB)

	// Generate redeem priv key, address, and WIF
	redeemPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating redeem private key: %v", err)
	}
	redeemAddr, err := pubKeyToP2WPKHAddress(redeemPrivKey.PubKey(), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("error creating redeem address: %v", err)
	}
	redeemWIF, err := btcutil.NewWIF(redeemPrivKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error creating redeem WIF: %v", err)
	}

	// Generate refund priv key, address, and WIF
	refundPrivKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("error generating refund private key: %v", err)
	}
	refundAddr, err := pubKeyToP2WPKHAddress(refundPrivKey.PubKey(), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("error creating refund address: %v", err)
	}
	refundWIF, err := btcutil.NewWIF(refundPrivKey, &chaincfg.MainNetParams, true)
	if err != nil {
		t.Fatalf("error creating refund WIF: %v", err)
	}

	// Generate public keys and nonces for both parties
	node.newAddress = redeemAddr.String()
	node.privKeyForAddr = redeemWIF
	redeemPubKeyAndNonce, err := wallet.PrivateSwapPubKey()
	if err != nil {
		t.Fatalf("error generating redeem pub key: %v", err)
	}

	node.newAddress = refundAddr.String()
	node.privKeyForAddr = refundWIF
	refundPubKeyAndNonce, err := wallet.PrivateSwapPubKey()
	if err != nil {
		t.Fatalf("error generating refund pub key: %v", err)
	}

	// Create a contract with both pub keys
	contract := &asset.PrivateContract{
		LockTime:        uint64(time.Now().Unix()),
		Value:           toSatoshi(5),
		RedeemPublicKey: redeemPubKeyAndNonce,
		RefundPublicKey: refundPubKeyAndNonce,
	}

	// Generate an adaptor secret and validate it
	var adaptorSecret *btcec.ModNScalar
	for {
		adaptorSecret = new(btcec.ModNScalar)
		adaptorSecret.SetByteSlice(randBytes(32))

		// Create a dummy unsigned redeem transaction for validation
		coinID := ToCoinID(tTxHash, 0)
		unsignedRedeemB, err := wallet.GenerateUnsignedRedeemTx(coinID, contract, optimalFeeRate)
		if err != nil {
			t.Fatalf("failed to generate unsigned redeem tx: %v", err)
		}

		valid, err := wallet.ValidateAdaptorSecret(adaptorSecret, unsignedRedeemB, contract)
		if err != nil {
			t.Fatalf("failed to validate adaptor secret: %v", err)
		}
		if valid {
			break
		}
	}

	// Create the adaptor public key (T = t * G)
	adaptorPub := new(btcec.JacobianPoint)
	btcec.ScalarBaseMultNonConst(adaptorSecret, adaptorPub)

	// Generate unsigned redeem transaction
	coinID := ToCoinID(tTxHash, 0)
	unsignedRedeemB, err := wallet.GenerateUnsignedRedeemTx(coinID, contract, optimalFeeRate)
	if err != nil {
		t.Fatalf("failed to generate unsigned redeem tx: %v", err)
	}

	// Override the PrivKeyForAddress method to return the refunder's private key
	// (GeneratePublicKeyTweakedAdaptor is called by the party who doesn't know the secret)
	node.privKeyForAddr = refundWIF
	node.privKeyForAddrErr = nil

	// Generate the public key tweaked adaptor signature
	adaptorSigB, err := wallet.GeneratePublicKeyTweakedAdaptor(unsignedRedeemB, contract, adaptorPub)
	if err != nil {
		t.Fatalf("GeneratePublicKeyTweakedAdaptor error: %v", err)
	}

	// Now test RedeemPrivate - switch back to redeemer's private key
	node.privKeyForAddr = redeemWIF
	node.privKeyForAddrErr = nil

	// Test successful redemption
	out, feesPaid, txData, err := wallet.RedeemPrivate(t.Context(), contract, unsignedRedeemB, adaptorSigB, adaptorSecret)
	if err != nil {
		t.Fatalf("RedeemPrivate error: %v", err)
	}

	// Verify the returned values
	if out == nil {
		t.Fatalf("expected non-nil output coin")
	}
	expectedTxSize := MinimumTxOverhead + 1 + TxInOverhead + P2WPKHOutputSize
	expectedTxSize += (PrivateRedeemWitnessSize + 2 + 3) / 4
	expectedFee := optimalFeeRate * uint64(expectedTxSize)
	if feesPaid != expectedFee {
		t.Fatalf("expected %d fees paid, got %d", expectedFee, feesPaid)
	}
	if txData == nil {
		t.Fatalf("expected non-nil tx data")
	}
	sentTxB, err := serializeMsgTx(node.sentRawTx)
	if err != nil {
		t.Fatalf("error serializing sent transaction: %v", err)
	}
	if !bytes.Equal(txData, sentTxB) {
		t.Fatalf("expected tx data to be equal to sent transaction: %x != %x", txData, sentTxB)
	}

	// Test error cases

	// Test with nil unsigned redeem transaction
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, nil, adaptorSigB, adaptorSecret)
	if err == nil {
		t.Fatalf("expected error for nil unsigned redeem transaction")
	}

	// Test with nil adaptor signature
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, unsignedRedeemB, nil, adaptorSecret)
	if err == nil {
		t.Fatalf("expected error for nil adaptor signature")
	}

	// Test with nil adaptor secret
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, unsignedRedeemB, adaptorSigB, nil)
	if err == nil {
		t.Fatalf("expected error for nil adaptor secret")
	}

	// Test with invalid unsigned redeem transaction
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, []byte("invalid"), adaptorSigB, adaptorSecret)
	if err == nil {
		t.Fatalf("expected error for invalid unsigned redeem transaction")
	}

	// Test with invalid adaptor signature
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, unsignedRedeemB, []byte("invalid"), adaptorSecret)
	if err == nil {
		t.Fatalf("expected error for invalid adaptor signature")
	}

	// Test with wrong adaptor secret
	wrongSecret := new(btcec.ModNScalar)
	wrongSecret.SetByteSlice(randBytes(32))
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, unsignedRedeemB, adaptorSigB, wrongSecret)
	if err == nil {
		t.Fatalf("expected error for wrong adaptor secret")
	}

	// Test with missing private key
	node.privKeyForAddrErr = tErr
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, unsignedRedeemB, adaptorSigB, adaptorSecret)
	if err == nil {
		t.Fatalf("expected error for missing private key")
	}
	node.privKeyForAddrErr = nil

	// Test with broadcast error
	node.sendErr = tErr
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, unsignedRedeemB, adaptorSigB, adaptorSecret)
	if err == nil {
		t.Fatalf("expected error for broadcast failure")
	}
	node.sendErr = nil

	// Test with wrong hash returned from broadcast
	var h chainhash.Hash
	h[0] = 0x01
	node.badSendHash = &h
	_, _, _, err = wallet.RedeemPrivate(t.Context(), contract, unsignedRedeemB, adaptorSigB, adaptorSecret)
	if err == nil {
		t.Fatalf("expected error for wrong broadcast hash")
	}
	node.badSendHash = nil
}
