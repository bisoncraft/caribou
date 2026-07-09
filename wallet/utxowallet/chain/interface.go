package chain

import (
	"time"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/waddrmgr"
	"github.com/bisoncraft/caribou/wallet/utxowallet/wtxmgr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

// BackEnds returns a list of the available back ends.
// TODO: Refactor each into a driver and use dynamic registration.
func BackEnds() []string {
	return []string{
		"bitcoind",
		"btcd",
		"neutrino",
		"bitcoind-rpc-polling",
		ElectrumBackEnd,
	}
}

// Backend allows more than one backing blockchain source, such as a
// btcd RPC chain server, or an SPV library, as long as we write a driver for
// it.
type Backend interface {
	Start() error
	Stop()
	WaitForShutdown()
	GetBestBlock() (*chainhash.Hash, int32, error)
	GetBlock(*chainhash.Hash) (*bisonwire.BlockWithHeight, error)
	GetBlockHash(int64) (*chainhash.Hash, error)
	GetBlockHeader(*chainhash.Hash) (*wire.BlockHeader, error)
	IsCurrent() bool
	FilterBlocks(*FilterBlocksRequest) (*FilterBlocksResponse, error)
	BlockStamp() (*waddrmgr.BlockStamp, error)
	SendRawTransaction(*wire.MsgTx, bool) (*chainhash.Hash, error)
	Rescan(*chainhash.Hash, []btcutil.Address, map[wire.OutPoint]btcutil.Address) error
	NotifyReceived([]btcutil.Address) error
	NotifyBlocks() error
	Notifications() <-chan interface{}
	BackEnd() string
	MapRPCErr(err error) error
}

// AddressProber is an optional capability for a Backend that can answer, per
// address and cheaply, whether the address has any history (e.g. electrum's
// blockchain.scripthash.get_history). It lets the wallet do gap-limit recovery
// by probing derived addresses directly instead of walking blocks. Backends
// that don't implement it fall back to block-walk recovery.
//
// "Used" means any history, confirmed or in mempool — a used-then-emptied
// address must still report true, so implementations test history presence,
// not balance. earliest is the lowest CONFIRMED height in the history (0
// when the history is empty or mempool-only): the caller needs it to place a
// discovered address's sweep floor below its first activity, since the
// history predates anything the wallet has ingested.
type AddressProber interface {
	ProbeAddress(addr btcutil.Address) (used bool, earliest int32, err error)
}

// AddressWatcher is an optional capability for a Backend whose per-address
// watches (established via NotifyReceived) can be dropped again. It lets the
// wallet bound its persistent watch set by ending an address's watch once no
// time-uncontrolled event can occur on it — e.g. a change address whose every
// transaction is final. Backends that don't implement it (neutrino's cfilter
// watch list is add-only today) keep the historical watch-forever behaviour;
// see docs/address-watching-plan.md.
//
// Watch remains NotifyReceived: every existing feed watches at a hot moment
// (address creation, change creation, broadcast), so only the removal and
// sweep verbs are new.
type AddressWatcher interface {
	Unwatch(addrs []btcutil.Address) error

	// Sweep is the metered safety net for unwatched (cold) addresses: it
	// checks each entry for activity strictly above its own FromHeight (or
	// in the mempool), re-establishes the watch for those with hits, and
	// ingests the new history in block order. It returns the hit addresses
	// (for promotion out of the cold set) and the addresses whose check
	// FAILED — the caller must not advance any known-history floor past a
	// failed address, or the heights it skipped are never examined again.
	Sweep(entries []SweepEntry) (hits, failed []btcutil.Address, err error)
}

// SweepEntry is one cold address in a Sweep request. Floors are per-entry
// because cold addresses are demoted at different tips: judging every entry
// against a set-wide minimum floor sweeps each entry above the minimum from
// below its own verified floor, re-hitting its own already-ingested history
// on every sweep.
type SweepEntry struct {
	Addr btcutil.Address
	// FromHeight is exclusive: activity strictly above it counts as a hit.
	FromHeight int32
	// OutPoints are the address's known unspent outpoints. A block-scanning
	// backend (neutrino) matches spends against them exactly: the prevout
	// script reconstructed from an input's signature data covers only the
	// classes ComputePkScript understands, and a taproot keyspend witness
	// reconstructs to a wrong script with no error — an outpoint match
	// cannot be fooled that way, and the phantom-balance harm of a missed
	// spend is confined to exactly these known UTXOs. History-based
	// backends (electrum) see spends in the address's own history and may
	// ignore this.
	OutPoints []wire.OutPoint
}

// Notification types.  These are defined here and processed from from reading
// a notificationChan to avoid handling these notifications directly in
// rpcclient callbacks, which isn't very Go-like and doesn't allow
// blocking client calls.
type (
	// ClientConnected is a notification for when a client connection is
	// opened or reestablished to the chain server.
	ClientConnected struct{}

	// BlockConnected is a notification for a newly-attached block to the
	// best chain.
	BlockConnected wtxmgr.BlockMeta

	// FilteredBlockConnected is an alternate notification that contains
	// both block and relevant transaction information in one struct, which
	// allows atomic updates.
	FilteredBlockConnected struct {
		Block       *wtxmgr.BlockMeta
		RelevantTxs []*wtxmgr.TxRecord
	}

	// FilterBlocksRequest specifies a range of blocks and the set of
	// internal and external addresses of interest, indexed by corresponding
	// scoped-index of the child address. A global set of watched outpoints
	// is also included to monitor for spends.
	FilterBlocksRequest struct {
		Blocks           []wtxmgr.BlockMeta
		ExternalAddrs    map[waddrmgr.ScopedIndex]btcutil.Address
		InternalAddrs    map[waddrmgr.ScopedIndex]btcutil.Address
		WatchedOutPoints map[wire.OutPoint]btcutil.Address
	}

	// FilterBlocksResponse reports the set of all internal and external
	// addresses found in response to a FilterBlockRequest, any outpoints
	// found that correspond to those addresses, as well as the relevant
	// transactions that can modify the wallet's balance. The index of the
	// block within the FilterBlocksRequest is returned, such that the
	// caller can reinitiate a request for the subsequent block after
	// updating the addresses of interest.
	FilterBlocksResponse struct {
		BatchIndex         uint32
		BlockMeta          wtxmgr.BlockMeta
		FoundExternalAddrs map[waddrmgr.KeyScope]map[uint32]struct{}
		FoundInternalAddrs map[waddrmgr.KeyScope]map[uint32]struct{}
		FoundOutPoints     map[wire.OutPoint]btcutil.Address
		RelevantTxns       []*bisonwire.Tx
	}

	// BlockDisconnected is a notifcation that the block described by the
	// BlockStamp was reorganized out of the best chain.
	BlockDisconnected wtxmgr.BlockMeta

	// RelevantTx is a notification for a transaction which spends wallet
	// inputs or pays to a watched address.
	RelevantTx struct {
		TxRecord *wtxmgr.TxRecord
		Block    *wtxmgr.BlockMeta // nil if unmined
	}

	// RescanProgress is a notification describing the current status
	// of an in-progress rescan.
	RescanProgress struct {
		Hash   chainhash.Hash
		Height int32
		Time   time.Time
	}

	// RescanFinished is a notification that a previous rescan request
	// has finished.
	RescanFinished struct {
		Hash   *chainhash.Hash
		Height int32
		Time   time.Time
	}
)

// batchClient defines an interface that is used to interact with the RPC
// client.
//
// NOTE: the client returned from `rpcclient.NewBatch` will implement this
// interface. Unlike the client from `rpcclient.New`, calling `GetRawMempool`
// on this client will block and won't return.
//
// TODO(yy): create a new type BatchClient in `rpcclient`.
type batchClient interface {
	// GetRawMempoolAsync returns an instance of a type that can be used to
	// get the result of the RPC at some future time by invoking the
	// Receive function on the returned instance.
	GetRawMempoolAsync() rpcclient.FutureGetRawMempoolResult

	// GetRawTransactionAsync returns an instance of a type that can be
	// used to get the result of the RPC at some future time by invoking
	// the Receive function on the returned instance.
	GetRawTransactionAsync(
		txHash *chainhash.Hash) rpcclient.FutureGetRawTransactionResult

	// Send marshalls bulk requests and sends to the server creates a
	// response channel to receive the response
	Send() error
}

// getRawTxReceiver defines an interface that's used to receive response from
// `GetRawTransactionAsync`.
type getRawTxReceiver interface {
	// Receive waits for the Response promised by the future and returns a
	// transaction given its hash.
	Receive() (*btcutil.Tx, error)
}
