// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package wallet

import (
	"errors"

	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txrules"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wallet/txsizes"
	"github.com/bisoncraft/caribou/wallet/utxowallet/walletdb"
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
// matches the fee such a send actually pays.
//
// The size includes space for a change output even though the constructed
// transaction has none: txauthor.NewUnsignedTransaction always requires inputs
// to cover the fee at the change-inclusive size before it will build the
// transaction, so a send of (balance − fee) only funds if fee was computed the
// same way. The built transaction then has exactly zero left over for change
// and pays this same fee (the change-inclusive rate applied to a changeless
// transaction). Change sizing assumes the default change scope, matching
// CreateSimpleTx called with no change-scope option.
//
// The transaction is not constructed and the wallet is not modified; a locked
// wallet is fine.
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

	var nP2PKH, nP2TR, nP2WPKH, nNested, changeScriptSize int
	err = walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		addrmgrNs := dbtx.ReadBucket(waddrmgrNamespaceKey)
		changeScriptSize, err = w.changeScriptSize(addrmgrNs, nil, account)
		if err != nil {
			return err
		}
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

	vSize := txsizes.EstimateVirtualSize(nP2PKH, nP2TR, nP2WPKH, nNested, outputs, changeScriptSize)
	return txrules.FeeForSerializeSize(feeSatPerKB, vSize), nil
}
