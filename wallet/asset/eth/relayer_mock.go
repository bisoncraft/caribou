// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"github.com/bisoncraft/bosswallet/util/evmrelay"
	swapv1 "github.com/bisoncraft/bosswallet/wallet/asset/eth/contracts/v1"
	"github.com/ethereum/go-ethereum/common"
)

// MockSignedRedeemCalldata builds conservative synthetic redeemWithSignature
// calldata for OP Stack L1 fee estimation. It uses the real ABI to get the
// exact calldata length, then randomizes the payload bytes after the selector
// so the result is hard to compress.
//
// Lives in package eth (not evmrelay) so evmrelay can stay free of any
// dependency on this package.
func MockSignedRedeemCalldata(numRedemptions int) ([]byte, error) {
	if numRedemptions <= 0 || numRedemptions > evmrelay.MaxSignedRedeemBatch {
		return nil, fmt.Errorf("numRedemptions must be between 1 and %d", evmrelay.MaxSignedRedeemBatch)
	}

	redemptions := make([]swapv1.ETHSwapRedemption, numRedemptions)
	for i := range redemptions {
		if _, err := rand.Read(redemptions[i].V.SecretHash[:]); err != nil {
			return nil, fmt.Errorf("error generating redemption secret hash: %w", err)
		}
		if _, err := rand.Read(redemptions[i].V.Initiator[:]); err != nil {
			return nil, fmt.Errorf("error generating redemption initiator: %w", err)
		}
		if _, err := rand.Read(redemptions[i].V.Participant[:]); err != nil {
			return nil, fmt.Errorf("error generating redemption participant: %w", err)
		}
		redemptions[i].V.Value = new(big.Int).SetUint64(1e18)
		redemptions[i].V.RefundTimestamp = uint64(time.Now().Unix())
		if _, err := rand.Read(redemptions[i].Secret[:]); err != nil {
			return nil, fmt.Errorf("error generating redemption secret: %w", err)
		}
	}

	var feeRecipient common.Address
	if _, err := rand.Read(feeRecipient[:]); err != nil {
		return nil, fmt.Errorf("error generating fee recipient: %w", err)
	}

	sig := make([]byte, 65)
	if _, err := rand.Read(sig); err != nil {
		return nil, fmt.Errorf("error generating signature: %w", err)
	}

	calldata, err := ABIs[1].Pack(RedeemWithSignatureMethodName,
		redemptions, feeRecipient, new(big.Int), new(big.Int), new(big.Int), sig,
	)
	if err != nil {
		return nil, err
	}

	if len(calldata) > 4 {
		if _, err := rand.Read(calldata[4:]); err != nil {
			return nil, fmt.Errorf("error randomizing calldata payload: %w", err)
		}
	}

	return calldata, nil
}
