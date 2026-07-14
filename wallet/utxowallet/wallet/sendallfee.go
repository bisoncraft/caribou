// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package wallet

import (
	"errors"

	"github.com/bisoncraft/bosswallet/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet/txrules"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/wallet/txsizes"
	"github.com/bisoncraft/bosswallet/wallet/utxowallet/walletdb"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// EstimateSendAllFee returns the fee for a transaction that spends every
// eligible unspent output to the provided outputs with no change — the shape a
// send takes when the requested value plus fee consumes the whole balance.
// Output eligibility matches txToOutputs' coin selection (same maturity,
// locked-outpoint, and account filters), and the size estimate uses the same
// txsizes math txauthor uses to compute fees during selection, so the estimate
// matches the fee such a send actually pays. The transaction is not
// constructed and the wallet is not modified; a locked wallet is fine.
func (w *Wallet) EstimateSendAllFee(coinSelectKeyScope *waddrmgr.KeyScope,
	account uint32, minconf int32, feeSatPerKB btcutil.Amount,
	outputs []*wire.TxOut) (btcutil.Amount, error) {

	chainClient, err := w.requireChainClient()
	if err != nil {
		return 0, err
	}
	bs, err := chainClient.BlockStamp()
	if err != nil {
		return 0, err
	}

	var nP2PKH, nP2TR, nP2WPKH, nNested int
	err = walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		eligible, err := w.findEligibleOutputs(
			dbtx, coinSelectKeyScope, account, minconf, bs, nil,
		)
		if err != nil {
			return err
		}
		if len(eligible) == 0 {
			return errors.New("no spendable outputs")
		}
		for i := range eligible {
			pkScript := eligible[i].PkScript
			switch {
			case txscript.IsPayToWitnessPubKeyHash(pkScript):
				nP2WPKH++
			case txscript.IsPayToTaproot(pkScript):
				nP2TR++
			case txscript.IsPayToScriptHash(pkScript):
				// Treated as nested P2WPKH, matching txauthor's
				// assumption for wallet-held script-hash outputs.
				nNested++
			default:
				nP2PKH++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	vSize := txsizes.EstimateVirtualSize(nP2PKH, nP2TR, nP2WPKH, nNested, outputs, 0)
	return txrules.FeeForSerializeSize(feeSatPerKB, vSize), nil
}
