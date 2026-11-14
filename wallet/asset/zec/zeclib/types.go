// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"fmt"
	"slices"
	"time"
)

// NetworkType identifies a Zcash network.
type NetworkType uint8

const (
	// NetworkMainnet is the Zcash main network.
	NetworkMainnet NetworkType = iota
	// NetworkTestnet is the public Zcash test network.
	NetworkTestnet
	// NetworkRegtest is a local regression-test chain.
	NetworkRegtest
)

// String returns the name of the network type.
func (t NetworkType) String() string {
	switch t {
	case NetworkMainnet:
		return "mainnet"
	case NetworkTestnet:
		return "testnet"
	case NetworkRegtest:
		return "regtest"
	default:
		return fmt.Sprintf("NetworkType(%d)", uint8(t))
	}
}

// ActivationHeights is the upgrade schedule of a regtest chain: the height
// at which each network upgrade activates. Zero means the upgrade never
// activates. No upgrade can activate at the genesis block, so zero is not
// otherwise a meaningful height.
//
// The heights must match the configuration of the node. A wallet with a
// different schedule misreads the chain and builds transactions that the
// node rejects.
type ActivationHeights struct {
	Overwinter uint32
	Sapling    uint32
	Blossom    uint32
	Heartwood  uint32
	Canopy     uint32
	NU5        uint32
	NU6        uint32
	NU6_1      uint32
	NU6_2      uint32
	// NU6_3 adds the Ironwood shielded pool. It has to activate: a wallet
	// cannot be opened for a chain without the pool.
	NU6_3 uint32
	NU7   uint32
}

// AllUpgradesAt returns a schedule that activates every network upgrade at
// the same height.
func AllUpgradesAt(height uint32) ActivationHeights {
	return ActivationHeights{
		Overwinter: height,
		Sapling:    height,
		Blossom:    height,
		Heartwood:  height,
		Canopy:     height,
		NU5:        height,
		NU6:        height,
		NU6_1:      height,
		NU6_2:      height,
		NU6_3:      height,
		NU7:        height,
	}
}

// Network selects the chain a wallet follows.
type Network struct {
	Type NetworkType
	// Regtest is the upgrade schedule of a regtest chain. It is ignored for
	// the other network types, whose schedules are fixed.
	Regtest ActivationHeights
}

var (
	// Mainnet is the Zcash main network.
	Mainnet = Network{Type: NetworkMainnet}
	// Testnet is the public Zcash test network.
	Testnet = Network{Type: NetworkTestnet}
)

// Regtest returns a regtest network with the given upgrade schedule.
func Regtest(heights ActivationHeights) Network {
	return Network{Type: NetworkRegtest, Regtest: heights}
}

// String returns the name of the network.
func (n Network) String() string { return n.Type.String() }

const (
	mainnetSaplingActivation uint32 = 419_200
	testnetSaplingActivation uint32 = 280_000

	mainnetIronwoodActivation uint32 = 3_428_143
	testnetIronwoodActivation uint32 = 4_134_000
)

// saplingActivation is the height below which the note commitment trees are
// empty. Servers do not serve tree states below it.
func (n Network) saplingActivation() uint32 {
	switch n.Type {
	case NetworkMainnet:
		return mainnetSaplingActivation
	case NetworkTestnet:
		return testnetSaplingActivation
	default:
		return n.Regtest.Sapling
	}
}

// ironwoodActivation is the activation height of NU6.3, which is the first
// block that can hold funds in the Ironwood pool. No account begins below
// it. It is zero for a regtest network on which NU6.3 never activates, which
// a wallet cannot be opened for.
func (n Network) ironwoodActivation() uint32 {
	switch n.Type {
	case NetworkMainnet:
		return mainnetIronwoodActivation
	case NetworkTestnet:
		return testnetIronwoodActivation
	default:
		return n.Regtest.NU6_3
	}
}

// matchesChainName reports whether name is how a lightwalletd server on this
// network identifies its chain.
func (n Network) matchesChainName(name string) bool {
	switch n.Type {
	case NetworkMainnet:
		return name == "main"
	case NetworkTestnet:
		return name == "test"
	default:
		// zebrad reports a regtest chain as "test".
		return name == "regtest" || name == "test"
	}
}

// Account is an account of a wallet. An account has its own keys, addresses
// and balances.
type Account struct {
	// ID identifies the account within its wallet. It is a UUID that the
	// wallet assigns when the account is created, so the same seed gets a
	// different ID in another wallet database.
	ID string
	// Name is the label given when the account was created.
	Name string
	// Birthday is the height of the first block the wallet scans for the
	// account. Funds received in earlier blocks are not found.
	Birthday uint32
	// UFVK is the account's unified full viewing key. It reveals every
	// transaction of the account and cannot spend.
	UFVK string
	// Derived reports whether the account was derived from a seed. When it
	// is, HDIndex is its ZIP 32 account index and SeedFingerprint is the
	// ZIP 32 fingerprint of the seed.
	Derived         bool
	HDIndex         uint32
	SeedFingerprint []byte
	// KeySource is an application-defined label for where the account's
	// spending keys are held.
	KeySource string
}

// PoolBalance is the balance of an account within one value pool, in
// zatoshis.
type PoolBalance struct {
	// Spendable is the value that can be spent now.
	Spendable uint64
	// ChangePending is change from the wallet's own transactions that does
	// not yet have enough confirmations to be spent.
	ChangePending uint64
	// ValuePending is received value that cannot be spent yet, because it
	// lacks confirmations or because the wallet has not scanned enough of
	// the chain to prove that it exists.
	ValuePending uint64
	// Uneconomic is the value of outputs that cost more in fees to spend
	// than they hold. It is not part of Total.
	Uneconomic uint64
	// Locked is value reserved by an operation in progress.
	Locked uint64
	// Total is the sum of Spendable, ChangePending, ValuePending and Locked.
	Total uint64
}

// AccountBalance is the balances of one account, per value pool.
//
// There are no Sapling or Orchard balances. An account's addresses have no
// Sapling receiver, and from NU6.3 on a payment to the Orchard receiver of a
// unified address lands in the Ironwood pool.
type AccountBalance struct {
	// AccountID is the ID of the account, as in Account.
	AccountID   string
	Ironwood    PoolBalance
	Transparent PoolBalance
	// Total is the sum of the pool totals.
	Total uint64
}

// Ratio is an amount of work done out of an amount of work to do. The
// denominator is zero when there is nothing to do.
type Ratio struct {
	Numerator   uint64
	Denominator uint64
}

// Fraction returns the ratio as a number from 0 to 1. A ratio with nothing
// to do is complete, so it returns 1.
func (r Ratio) Fraction() float64 {
	if r.Denominator == 0 {
		return 1
	}
	if r.Numerator >= r.Denominator {
		return 1
	}
	return float64(r.Numerator) / float64(r.Denominator)
}

// Summary is the balances and scan state of a wallet as of the chain tip it
// knows about.
//
// Balances can overstate what an account holds until the wallet is fully
// scanned, because the wallet may not have seen a spend yet.
type Summary struct {
	// Balances holds one entry per account.
	Balances []AccountBalance
	// ChainTip is the height of the chain tip the wallet was last told
	// about.
	ChainTip uint32
	// FullyScanned is the height up to which every block since the wallet's
	// birthday has been scanned.
	FullyScanned uint32
	// ScanProgress measures scanning between the recovery height and the
	// chain tip, in notes scanned out of notes on the chain. For a wallet
	// that is not being recovered, the range starts at the birthday.
	ScanProgress Ratio
	// RecoveryProgress measures scanning between the birthday and the
	// height at which the wallet was restored from its seed. Recovering
	// reports whether the wallet has such a range.
	RecoveryProgress Ratio
	Recovering       bool

	// nextSubtree is the index at which the next request for subtree roots
	// of the Ironwood note commitment tree starts.
	nextSubtree uint64
}

// Balance returns the balances of the account, and false if the summary has
// none for it.
func (s *Summary) Balance(accountID string) (AccountBalance, bool) {
	for i := range s.Balances {
		if s.Balances[i].AccountID == accountID {
			return s.Balances[i], true
		}
	}
	return AccountBalance{}, false
}

// ReceiverSet selects the receivers of a new unified address.
//
// The shielded receiver of an address is its Orchard receiver, which
// receives into the Ironwood pool from NU6.3 on. No address has a Sapling
// receiver.
type ReceiverSet uint8

const (
	// ReceiversAll is an Ironwood and a transparent receiver.
	ReceiversAll ReceiverSet = iota
	// ReceiversShielded is an Ironwood receiver only.
	ReceiversShielded
)

// scanPriority orders the block ranges a wallet wants scanned. Greater
// values are more urgent.
type scanPriority uint8

const (
	priorityIgnored scanPriority = iota
	priorityScanned
	priorityHistoric
	priorityOpenAdjacent
	priorityFoundNote
	priorityChainTip
	// priorityVerify marks a range that was scanned before and has to be
	// checked against the current chain. Such a range is always suggested
	// first.
	priorityVerify
)

func (p scanPriority) String() string {
	switch p {
	case priorityIgnored:
		return "ignored"
	case priorityScanned:
		return "scanned"
	case priorityHistoric:
		return "historic"
	case priorityOpenAdjacent:
		return "open-adjacent"
	case priorityFoundNote:
		return "found-note"
	case priorityChainTip:
		return "chain-tip"
	case priorityVerify:
		return "verify"
	default:
		return fmt.Sprintf("scanPriority(%d)", uint8(p))
	}
}

// scanRange is a range of block heights to scan, from start up to but
// excluding end.
type scanRange struct {
	start    uint32
	end      uint32
	priority scanPriority
}

func (r scanRange) String() string {
	return fmt.Sprintf("[%d, %d) %s", r.start, r.end, r.priority)
}

// watchedAddress is a transparent address that the wallet watches.
type watchedAddress struct {
	address string
	// change is set for an address on the change chain of its account.
	// The other chain holds the addresses handed out for receiving.
	change bool
	// index is the index of the address on its chain.
	index uint32
	// unused is set for an address at or above the index where the unused
	// addresses of its chain begin: the first index from which a whole gap
	// limit of addresses has no known use.
	unused bool
}

// scanSummary is the range of heights that scanning a batch of blocks
// covered.
type scanSummary struct {
	// start is the first height scanned and end is one past the last.
	start uint32
	end   uint32
}

// subtreeRoot is the root of one completed subtree of a note commitment
// tree.
type subtreeRoot struct {
	hash []byte
	// completingHeight is the height of the block that completed the
	// subtree.
	completingHeight uint32
}

// subtreeRoots holds consecutive subtree roots of the Ironwood note
// commitment tree, the first one being the root of the subtree at index
// start.
type subtreeRoots struct {
	start uint64
	roots []subtreeRoot
}

// utxo is an unspent transparent output paying an address of the wallet.
type utxo struct {
	// txid is in the byte order lightwalletd reports it in.
	txid   []byte
	index  uint32
	script []byte
	value  uint64
	height uint32
}

// txRequestKind says what a transaction data request asks for.
type txRequestKind uint8

const (
	// txRequestStatus asks whether and where a transaction was mined.
	txRequestStatus txRequestKind = iota
	// txRequestEnhancement asks for a full transaction.
	txRequestEnhancement
	// txRequestAddress asks for the transactions that involve a transparent
	// address within a block range.
	txRequestAddress
)

// txStatusFilter and outputStatusFilter qualify a txRequestAddress request.
// The lightwalletd protocol cannot express them, so they are only carried
// back to the wallet when the request is closed.
type (
	txStatusFilter     uint8
	outputStatusFilter uint8
)

const (
	txStatusMined txStatusFilter = iota
	txStatusMempool
	txStatusAll
)

const (
	outputStatusUnspent outputStatusFilter = iota
	outputStatusAll
)

// txRequest is transaction data that the wallet needs and that compact
// blocks do not carry.
type txRequest struct {
	kind txRequestKind
	// txid is set for txRequestStatus and txRequestEnhancement.
	txid []byte

	// The remaining fields are set for txRequestAddress.
	address string
	start   uint32
	// end is one past the last height of the range. bounded is false for a
	// range without an upper limit.
	end     uint32
	bounded bool
	// requestAt is the earliest time to make the request. The zero time
	// means now.
	requestAt    time.Time
	txStatus     txStatusFilter
	outputStatus outputStatusFilter
}

// String describes the request for a log message.
func (r *txRequest) String() string {
	switch r.kind {
	case txRequestStatus, txRequestEnhancement:
		// A transaction ID is shown in the reverse of its byte order on
		// the wire.
		display := slices.Clone(r.txid)
		slices.Reverse(display)
		what := "the status"
		if r.kind == txRequestEnhancement {
			what = "the data"
		}
		return fmt.Sprintf("%s of transaction %x", what, display)
	case txRequestAddress:
		if r.bounded {
			return fmt.Sprintf("the transactions of %s in blocks %d to %d", r.address, r.start, r.end-min(r.end, 1))
		}
		return fmt.Sprintf("the transactions of %s from block %d on", r.address, r.start)
	default:
		return fmt.Sprintf("transaction data of unknown kind %d", r.kind)
	}
}

// key identifies the request for the purpose of not repeating it.
func (r *txRequest) key() string {
	switch r.kind {
	case txRequestAddress:
		return fmt.Sprintf("a:%s:%d:%d:%t", r.address, r.start, r.end, r.bounded)
	default:
		return fmt.Sprintf("%d:%x", r.kind, r.txid)
	}
}

// txStatusKind is what a server knows about a transaction.
type txStatusKind uint8

const (
	// txNotRecognized means the server has no record of the transaction.
	txNotRecognized txStatusKind = iota
	// txNotInMainChain means the transaction is known but not mined in the
	// main chain. It may be in the mempool.
	txNotInMainChain
	// txMined means the transaction is mined in the main chain.
	txMined
)

// txStatus is the status of a transaction, with the height it was mined at
// when it is mined.
type txStatus struct {
	kind   txStatusKind
	height uint32
}
