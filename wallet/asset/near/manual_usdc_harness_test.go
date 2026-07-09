//go:build manual

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package near

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/bisoncraft/caribou/util"
	"github.com/decred/base58"
)

// Manual, not-committed harness driver for the NEP-141 token support added
// in 00f359b. Run against a live NEAR sandbox (util/testing/near/harness.sh):
//
//	go test -tags manual -run TestManualNEP141TokenRoundTrip -v ./wallet/asset/near
//
// The sandbox is a fresh ledger with no real USDC contract on it, so this
// deploys a minimal hand-rolled NEP-141-compatible contract (see
// wallet/asset/near/testdata/nep141_test_contract.wasm, built from
// contract.zig) exposing ft_balance_of/storage_balance_of/storage_deposit/
// ft_transfer plus a test-only `mint` method, then drives the real
// TokenWallet.Send/Balance code path against it.

const (
	manualHarnessRPC       = "http://localhost:23456"
	manualHarnessContainer = "near-sandbox"
	manualHarnessMasterID  = "test.near"
	manualValidatorKeyPath = "/root/.near/validator_key.json"
)

// manualMasterKey reads test.near's validator key directly out of the
// running sandbox container. util/testing/near/sendnear hardcodes this key
// for the amd64 "latest" image tag, but the "latest-aarch64" tag used on
// arm64 hosts (see util/testing/near/harness.sh) bakes in a different key,
// so that constant doesn't apply here.
func manualMasterKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	out, err := exec.Command("docker", "exec", manualHarnessContainer, "cat", manualValidatorKeyPath).Output()
	if err != nil {
		t.Fatalf("reading validator key from %s container: %v", manualHarnessContainer, err)
	}
	var keyFile struct {
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal(out, &keyFile); err != nil {
		t.Fatalf("decoding validator key file: %v", err)
	}
	const prefix = "ed25519:"
	keyData := base58.Decode(keyFile.SecretKey[len(prefix):])
	if len(keyData) != 64 {
		t.Fatalf("unexpected validator key length %d", len(keyData))
	}
	priv := ed25519.NewKeyFromSeed(keyData[:32])
	return priv, priv.Public().(ed25519.PublicKey)
}

func manualTestRPC(t *testing.T) *rpcClient {
	t.Helper()
	c, err := newRPCClient(manualHarnessRPC, "", util.Disabled)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	return c
}

func manualLatestBlockHash(t *testing.T, rpc *rpcClient) [32]byte {
	t.Helper()
	info, err := rpc.getBlock("final")
	if err != nil {
		t.Fatalf("getBlock: %v", err)
	}
	var bh [32]byte
	copy(bh[:], base58.Decode(info.Header.Hash))
	return bh
}

// manualViewAccessKeyOptimistic queries an access key at "optimistic"
// finality rather than production rpc.viewAccessKey's "final". "final"
// lags "optimistic" by ~2 blocks (NEAR's finality gadget), so re-querying
// "final" immediately after a just-executed prior transaction from the same
// account (as this harness does back-to-back) can still see the pre-tx
// nonce and produce an InvalidNonce rejection. Production code doesn't hit
// this: reserveNonce (near.go) tracks the last-issued nonce locally and
// advances past both the queried value and its own state, exactly to
// tolerate a lagging view. This harness has no such tracking, so it side
// steps the lag by reading the freshest (optimistic) value instead.
func manualViewAccessKeyOptimistic(rpc *rpcClient, accountID, pubKeyBase58 string) (*accessKeyInfo, error) {
	result, err := rpc.call("query", map[string]interface{}{
		"request_type": "view_access_key",
		"finality":     "optimistic",
		"account_id":   accountID,
		"public_key":   pubKeyBase58,
	})
	if err != nil {
		return nil, err
	}
	var info accessKeyInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// manualSubmit signs actions from signerAccountID to receiverAccountID and
// waits for the transaction to execute, failing the test on any on-chain
// error.
func manualSubmit(t *testing.T, rpc *rpcClient, signerAccountID string, signerPub ed25519.PublicKey, signerPriv ed25519.PrivateKey, receiverAccountID string, actions []action) {
	t.Helper()

	akInfo, err := manualViewAccessKeyOptimistic(rpc, signerAccountID, "ed25519:"+base58.Encode(signerPub))
	if err != nil {
		t.Fatalf("viewAccessKey(%s): %v", signerAccountID, err)
	}
	blockHash := manualLatestBlockHash(t, rpc)

	var pubKey32 [32]byte
	copy(pubKey32[:], signerPub)

	tx := &nearTransaction{
		signerID:   signerAccountID,
		publicKey:  pubKey32,
		nonce:      akInfo.Nonce + 1,
		receiverID: receiverAccountID,
		blockHash:  blockHash,
		actions:    actions,
	}
	txBytes := serializeTransaction(tx)
	hash := sha256.Sum256(txBytes)
	sig := ed25519.Sign(signerPriv, hash[:])
	signedTx := serializeSignedTransaction(txBytes, sig)
	encoded := encodeSignedTx(signedTx)

	// broadcast_tx_commit (synchronous) rather than the production
	// broadcastTxAsync: this single-node, zero-peer sandbox has been
	// observed to silently drop async-submitted transactions carrying a
	// larger payload (e.g. DeployContract's ~2.7KB wasm) without ever
	// including them in a block. That's sandbox-only test-setup plumbing;
	// the actual Send() call under test still goes through the real
	// production async path.
	result, err := rpc.call("broadcast_tx_commit", []string{encoded})
	if err != nil {
		t.Fatalf("broadcast_tx_commit: %v", err)
	}
	var res txResult
	if err := json.Unmarshal(result, &res); err != nil {
		t.Fatalf("decoding broadcast_tx_commit result: %v", err)
	}
	if res.Status.Failure != nil {
		t.Fatalf("tx failed on-chain: %s", res.Status.Failure)
	}
}

// manualTxStatus queries transaction status using the legacy positional-array
// "tx" RPC param shape ([tx_hash, sender_id]). The near/sandbox harness image
// only accepts this older shape; production rpc.txStatus's object-param
// shape (tx_hash/sender_account_id/wait_until) is confirmed to work against
// real NEAR mainnet/testnet nodes, so this is a sandbox-only workaround, not
// a production bug.
func manualTxStatus(rpc *rpcClient, txHash, senderID string) (*txResult, error) {
	result, err := rpc.call("tx", []string{txHash, senderID})
	if err != nil {
		return nil, err
	}
	var res txResult
	if err := json.Unmarshal(result, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// deployContractAction builds a DeployContract action (Borsh variant 1: a
// Vec<u8> of the wasm code). Test-only; production code never deploys
// contracts.
func deployContractAction(code []byte) action {
	return action{
		variant: actionDeployContract,
		data:    appendBytes(nil, code),
	}
}

func manualFtBalance(t *testing.T, rpc *rpcClient, contractID, accountID string) uint64 {
	t.Helper()
	raw, err := rpc.callFunction(contractID, "ft_balance_of", map[string]string{"account_id": accountID})
	if err != nil {
		t.Fatalf("ft_balance_of(%s): %v", accountID, err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decoding ft_balance_of(%s) = %q: %v", accountID, raw, err)
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("parsing ft_balance_of(%s) = %q: %v", accountID, s, err)
	}
	return v
}

// manualWaitForBalance polls ft_balance_of until it reaches want or times
// out. A "tx confirmed" response from the sandbox's legacy status query
// (manualTxStatus) can resolve slightly before the FunctionCall's storage
// writes are visible to a subsequent view call, so balance assertions poll
// rather than read once immediately after confirmation.
func manualWaitForBalance(t *testing.T, rpc *rpcClient, contractID, accountID string, want uint64) uint64 {
	t.Helper()
	var got uint64
	for i := 0; i < 40; i++ {
		got = manualFtBalance(t, rpc, contractID, accountID)
		if got == want {
			return got
		}
		time.Sleep(500 * time.Millisecond)
	}
	return got
}

func TestManualNEP141TokenRoundTrip(t *testing.T) {
	wasmPath := "testdata/nep141_test_contract.wasm"
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading %s: %v (build it from contract.zig first)", wasmPath, err)
	}

	rpc := manualTestRPC(t)

	// --- master account (test.near), baked into the sandbox image ---
	masterPriv, masterPub := manualMasterKey(t)
	const masterAccountID = manualHarnessMasterID

	// --- sender, recipient, and contract-deployer implicit accounts ---
	senderPriv := ed25519.NewKeyFromSeed([]byte("nep141-sender-seed-32-bytes-lon!"))
	senderPub := senderPriv.Public().(ed25519.PublicKey)
	senderAccountID := hex.EncodeToString(senderPub)

	recipientPriv := ed25519.NewKeyFromSeed([]byte("nep141-recip-seed-32-bytes-long!"))
	recipientPub := recipientPriv.Public().(ed25519.PublicKey)
	recipientAccountID := hex.EncodeToString(recipientPub)

	deployerPriv := ed25519.NewKeyFromSeed([]byte("nep141-deploy-seed-32-bytes-lon!"))
	deployerPub := deployerPriv.Public().(ed25519.PublicKey)
	deployerAccountID := hex.EncodeToString(deployerPub)

	oneNEAR, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 1e24 yoctoNEAR

	// --- fund sender, recipient, and the contract-deployer from test.near ---
	for _, accountID := range []string{senderAccountID, recipientAccountID, deployerAccountID} {
		manualSubmit(t, rpc, masterAccountID, masterPub, masterPriv, accountID, []action{transferAction(oneNEAR)})
		t.Logf("funded %s with 1 NEAR", accountID)
	}

	// --- deploy the test NEP-141 contract to its own account ---
	manualSubmit(t, rpc, deployerAccountID, deployerPub, deployerPriv, deployerAccountID, []action{deployContractAction(wasmBytes)})
	t.Logf("NEP-141 test contract deployed to %s", deployerAccountID)
	contractID := deployerAccountID

	// --- mint a starting balance to the sender ---
	const initialSupply = 50_000_000 // 50 tokens at 6 decimals
	mintArgs, err := json.Marshal(map[string]string{
		"account_id": senderAccountID,
		"amount":     "50000000",
	})
	if err != nil {
		t.Fatalf("marshal mint args: %v", err)
	}
	manualSubmit(t, rpc, senderAccountID, senderPub, senderPriv, contractID,
		[]action{functionCallAction("mint", mintArgs, 30_000_000_000_000, big.NewInt(0))})
	t.Logf("minted %d atomic units to sender %s", initialSupply, senderAccountID)

	// --- drive the real production TokenWallet.Send/Balance path ---
	token := &Token{
		Name:     "USDC",
		Decimals: 6,
		NetContracts: map[util.Network]string{
			util.Simnet: contractID,
		},
	}
	parent := &NearWallet{
		pubKey:    senderPub,
		privKey:   senderPriv,
		accountID: senderAccountID,
		rpc:       rpc,
		log:       util.Disabled,
		net:       util.Simnet,
	}
	// Send() reads a cached tipHash normally kept fresh by the background
	// tip poller started in Connect(), which this harness never runs.
	// Without this, tipHash is the zero value, and NEAR silently rejects
	// (never includes) a transaction referencing a bogus all-zero block
	// hash: exactly the "doesn't exist" symptom seen from broadcastTxAsync.
	parent.tipHash = manualLatestBlockHash(t, rpc)
	tw := &TokenWallet{
		parent:     parent,
		assetID:    0,
		token:      token,
		contractID: contractID,
	}

	if got := manualWaitForBalance(t, rpc, contractID, senderAccountID, initialSupply); got != initialSupply {
		t.Fatalf("sender balance after mint = %d, want %d (contract state never settled)", got, initialSupply)
	}

	preBalance, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance (pre-send): %v", err)
	}
	if preBalance.Available != initialSupply {
		t.Fatalf("sender balance before send = %d, want %d", preBalance.Available, initialSupply)
	}
	t.Logf("sender balance before send: %d", preBalance.Available)

	// Recipient has no storage registration yet: Send must bundle
	// storage_deposit ahead of ft_transfer in the same transaction.
	const sendAmount = 10_000_000 // 10 tokens
	coin, err := tw.Send(recipientAccountID, sendAmount, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	t.Logf("Send submitted, coin value %d, txID %s", coin.Value(), coin.String())

	// broadcastTxAsync doesn't wait for confirmation; poll until the
	// balances settle.
	var senderBal, recipientBal uint64
	for i := 0; i < 30; i++ {
		senderBal = manualFtBalance(t, rpc, contractID, senderAccountID)
		recipientBal = manualFtBalance(t, rpc, contractID, recipientAccountID)
		if recipientBal == sendAmount {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if recipientBal != sendAmount {
		if res, statusErr := manualTxStatus(rpc, coin.String(), senderAccountID); statusErr == nil {
			t.Logf("Send tx status: %+v", res)
		} else {
			t.Logf("Send tx status query error: %v", statusErr)
		}
		t.Fatalf("recipient balance = %d, want %d", recipientBal, sendAmount)
	}
	if senderBal != initialSupply-sendAmount {
		t.Fatalf("sender balance after send = %d, want %d", senderBal, initialSupply-sendAmount)
	}
	t.Logf("CONFIRMED: sender balance %d, recipient balance %d", senderBal, recipientBal)

	// --- confirm Balance() also reflects the new sender balance ---
	postBalance, err := tw.Balance()
	if err != nil {
		t.Fatalf("Balance (post-send): %v", err)
	}
	if postBalance.Available != senderBal {
		t.Fatalf("Balance() after send = %d, want %d", postBalance.Available, senderBal)
	}
}
