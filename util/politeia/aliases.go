// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package pi

import pt "github.com/bisoncraft/caribou/util/politeia/pitypes"

// The Politeia data types live in the lexi-free pitypes leaf package so that
// consumers needing only the types (Core, the asset interface, the app server)
// can import pitypes without linking this package's Badger (lexi) storage.
// These aliases keep this package's own storage code, and the dcr wallet,
// referring to the types by their original names.
type (
	VotingWallet              = pt.VotingWallet
	WalletProposalVoteDetails = pt.WalletProposalVoteDetails
	Ticket                    = pt.Ticket
	Proposal                  = pt.Proposal
	ProposalMetadata          = pt.ProposalMetadata
	MiniProposal              = pt.MiniProposal
)

const (
	VoteBitYes = pt.VoteBitYes
	VoteBitNo  = pt.VoteBitNo
)

// VotesStatuses maps politeia vote status codes to human-readable strings.
var VotesStatuses = pt.VotesStatuses
