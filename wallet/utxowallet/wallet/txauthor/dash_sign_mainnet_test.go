// Copyright 2026 The Caribou Developers.
// Use of this source code is governed by an ISC license that can be found in
// the LICENSE file.

package txauthor

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/txscript"
)

// TestDashSignatureHashMatchesDashd is a known-answer test: it verifies the
// signatures dashd-era wallets put on real mainnet special transactions
// against dashSignatureHash. A change to the packed version|type field or the
// payload placement makes every signature below fail to verify.
func TestDashSignatureHashMatchesDashd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		txid  string
		typ   uint16
		rawTx string
		// prevScripts holds the script of the output each input spends, in
		// input order.
		prevScripts []string
	}{{
		name:  "ProRegTx with 4 p2pkh inputs",
		txid:  "078cbad766a69ce1eaf274112e7abadad7761689a03f4e3c895fc4e2d1a8ad31",
		typ:   1,
		rawTx: "0300010004eb490f5c3508b186a63beda261e9c338729f641c161ec0bd04481aef7b56a90d000000006a4730440220729288d6f6defdc7fe44b15eb9496dab78280df60a923744e72068f079e114c9022030e5bca307069f4b5421056800f304199c989b25d872a1381df55084db873c85012102917702a60c80985eb0a5e9228d9a3479ed9b5bdd1ce068ddedd67d4721c9dfb0feffffffe3d477677378585e279b2bfafd1015fe53a93adf643729cf83af8d691d8de717000000006a473044022005458fee356b13603d11e32f4595c9cd88905bd652d5c31f6e0c45c4f29ca29102202388b8187ffa4245e7297267fe4377cb1580e48697730ebca4492a8691637e02012102917702a60c80985eb0a5e9228d9a3479ed9b5bdd1ce068ddedd67d4721c9dfb0feffffffeea6f0ec7a0963a359e34ac8fe55e2eb6cda4bb6f1137be7debb8ae0f28a2b4a000000006a47304402207e50504194658c99e2b4c29307ec71178e08e0e60beb72cdbeffcb4fb082b441022068310617472fe1c106426b544873b08ce68f5badc800e775d42d4a681a358885012102917702a60c80985eb0a5e9228d9a3479ed9b5bdd1ce068ddedd67d4721c9dfb0feffffffec918a806c8486ce2bebc67a587747f00cf768ea3209642539e1d9e9ea2481c7000000006a47304402205ba481a1b6f7b40934d0af235cb24c62b1d796714691b50ac84d215dad3ff6b702206cb35fa1a5484617d97ede5a98137cf374edfb8db81e271a3a3a42e499c47212012102917702a60c80985eb0a5e9228d9a3479ed9b5bdd1ce068ddedd67d4721c9dfb0feffffff02a8211931000000001976a9140403b1a73534abcb19166a1da90952ffeb9eee9488ac00a0db215d0000001976a914ddaa259aaa7695764e399c305b432b7368dab97a88ac00000000e902000100000000000000000000000000000000000000000000000000000000000000000000000100000000000000000000000000ffff59238327270f7d31786c35e5430878e3454e818b3b70a126de10b42fe40d93079ca80e7e97df31fd820c66673a1d97c594bbdb7d93d47e760a911b793668786848cd0b5e415d62e3e9a9e2656c41cd8e7ebff38480a6c3eb04a68f6142b000001976a9140403b1a73534abcb19166a1da90952ffeb9eee9488ace6b403c3dd6fe8a4a120aaffca3a41463f6455605589465a16926cfed1fb6ee11dc4d96efcf4f85633274a609bba8ade47cd72432068bb0100",
		prevScripts: []string{
			"76a9140403b1a73534abcb19166a1da90952ffeb9eee9488ac",
			"76a9140403b1a73534abcb19166a1da90952ffeb9eee9488ac",
			"76a9140403b1a73534abcb19166a1da90952ffeb9eee9488ac",
			"76a9140403b1a73534abcb19166a1da90952ffeb9eee9488ac",
		},
	}, {
		name:  "asset lock with 2 p2pkh inputs",
		txid:  "ba9eeb130fdb163bcb02d804872f028d65ddd2a8e377e039ad52ef897ceeedab",
		typ:   8,
		rawTx: "030008000270ce4b2ba1ed09b56180dd9f6166b1f585fe064466200c9b0ffaf42fbe64fefb010000006a47304402207cc1df1121e5c42831223b0804c6f39baf706ad347733f372f59cb83afc85e7702205fb7cc93cf8d9f9e9e54053a1f0734a0b72227aeb9897b288bf8ba8b603540cf0121021f7f6e2e727d928155f9bc1bc730b8e5b4460c9e8f0c60625c3b856355e06d35ffffffffdc80966af33c236259209803003a4e737cf95c4a02a85abad2fd46bff6d63157000000006a4730440220618ee60118d21e49e995b274df855bab968ef44f2f7af3c33c1e80d507e53bab02200bc36437370c87b676dca30a103d0f3d1995c004b8cb5fe1da9664ae21e6b90e0121021f7f6e2e727d928155f9bc1bc730b8e5b4460c9e8f0c60625c3b856355e06d35ffffffff0138a880b601000000026a000000000024010138a880b6010000001976a914b6a2f731af8af4554b20ad419e107d41580f84ed88ac",
		prevScripts: []string{
			"76a914e80e661d51e6ca6e6444878ef575bae05ac18eca88ac",
			"76a914e80e661d51e6ca6e6444878ef575bae05ac18eca88ac",
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, err := hex.DecodeString(tc.rawTx)
			if err != nil {
				t.Fatal(err)
			}
			tx := &bisonwire.Tx{Chain: "dash"}
			if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
				t.Fatal(err)
			}
			if tx.Type != tc.typ || len(tx.ExtraPayload) == 0 {
				t.Fatalf("type = %d payload = %d bytes, want type %d with a payload",
					tx.Type, len(tx.ExtraPayload), tc.typ)
			}
			if got := tx.TxHash().String(); got != tc.txid {
				t.Fatalf("TxHash = %s, want %s", got, tc.txid)
			}
			if len(tx.TxIn) != len(tc.prevScripts) {
				t.Fatalf("%d inputs, %d prev scripts", len(tx.TxIn), len(tc.prevScripts))
			}

			for i, in := range tx.TxIn {
				prev, err := hex.DecodeString(tc.prevScripts[i])
				if err != nil {
					t.Fatal(err)
				}
				pushes, err := txscript.PushedData(in.SignatureScript)
				if err != nil || len(pushes) != 2 {
					t.Fatalf("input %d: sig script pushes = %d, err = %v", i, len(pushes), err)
				}
				sigBytes, pubBytes := pushes[0], pushes[1]
				if sigBytes[len(sigBytes)-1] != byte(txscript.SigHashAll) {
					t.Fatalf("input %d: not SIGHASH_ALL", i)
				}
				sig, err := ecdsa.ParseDERSignature(sigBytes[:len(sigBytes)-1])
				if err != nil {
					t.Fatalf("input %d: %v", i, err)
				}
				pub, err := btcec.ParsePubKey(pubBytes)
				if err != nil {
					t.Fatalf("input %d: %v", i, err)
				}

				hash, err := dashSignatureHash(tx, i, prev)
				if err != nil {
					t.Fatal(err)
				}
				if !sig.Verify(hash, pub) {
					t.Errorf("input %d: signature does not verify against dashSignatureHash", i)
				}
			}
		})
	}
}
