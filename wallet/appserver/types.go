// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"github.com/bisoncraft/meshwallet/util/encode"
	pi "github.com/bisoncraft/meshwallet/util/politeia"
)

// standardResponse is a basic API response when no data needs to be returned.
type standardResponse struct {
	OK   bool   `json:"ok"`
	Msg  string `json:"msg,omitempty"`
	Code *int   `json:"code,omitempty"`
}

// simpleAck is a plain standardResponse with "ok" = true.
func simpleAck() *standardResponse {
	return &standardResponse{
		OK: true,
	}
}

// The loginForm is sent by the client to log in.
type loginForm struct {
	Pass encode.PassBytes `json:"pass"`
}

type sendTxFeeForm struct {
	Addr        string  `json:"addr"`
	Value       uint64  `json:"value"`
	Subtract    bool    `json:"subtract"`
	MaxWithdraw bool    `json:"maxWithdraw"`
	AssetID     *uint32 `json:"assetID,omitempty"`
}

type walletConfig struct {
	AssetID    uint32 `json:"assetID"`
	WalletType string `json:"walletType"`
	// These are only used if the Decred wallet does not already exist. In that
	// case, these parameters will be used to create the wallet.
	Config map[string]string `json:"config"`
}

// newWalletForm is information necessary to create a new wallet.
type newWalletForm struct {
	walletConfig
	Pass  encode.PassBytes `json:"pass"`
	AppPW encode.PassBytes `json:"appPass"`
}

// openWalletForm is information necessary to open a wallet.
type openWalletForm struct {
	AssetID uint32           `json:"assetID"`
	Pass    encode.PassBytes `json:"pass"` // Application password.
}

// walletStatusForm is information necessary to change a wallet's status.
type walletStatusForm struct {
	AssetID uint32 `json:"assetID"`
	Disable bool   `json:"disable"`
}

// sendForm is sent to initiate either send tx.
type sendForm struct {
	AssetID  uint32           `json:"assetID"`
	Value    uint64           `json:"value"`
	Address  string           `json:"address"`
	Subtract bool             `json:"subtract"`
	Pass     encode.PassBytes `json:"pw"`
}


type buildInfoResponse struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	Revision string `json:"revision"`
}

type proposalsMeta struct {
	ProposalsInProgress []*pi.MiniProposal `json:"proposalsInProgress"`
}
