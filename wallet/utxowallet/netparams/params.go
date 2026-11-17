// Copyright (c) 2013-2015 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package netparams

import (
	"math/big"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// ChainParams are an extended version of btcd/chaincfg.Params that omit some
// unnecessary fields and add some fields necessary for multi-asset needs.
type ChainParams struct {
	Chain                    string
	Name                     string
	Net                      wire.BitcoinNet
	GenesisBlock             *wire.MsgBlock
	GenesisHash              *chainhash.Hash
	TargetTimespan           time.Duration
	TargetTimePerBlock       time.Duration
	RetargetAdjustmentFactor int64
	ReduceMinDifficulty      bool
	MinDiffReductionTime     time.Duration
	Checkpoints              []Checkpoint
	PowLimit                 *big.Int
	PowLimitBits             uint32
	PoWNoRetargeting         bool
	DNSSeeds                 []chaincfg.DNSSeed
	DefaultPort              string

	// Human-readable part for Bech32 encoded segwit addresses, as defined
	// in BIP 173.
	Bech32HRPSegwit string

	// Address encoding magics
	PubKeyHashAddrID        byte // First byte of a P2PKH address
	ScriptHashAddrID        byte // First byte of a P2SH address
	PrivateKeyID            byte // First byte of a WIF private key
	WitnessPubKeyHashAddrID byte // First byte of a P2WPKH address
	WitnessScriptHashAddrID byte // First byte of a P2WSH address

	// BIP32 hierarchical deterministic extended key magics
	HDPrivateKeyID [4]byte
	HDPublicKeyID  [4]byte

	// BIP44 coin type used in the hierarchical deterministic path for
	// address generation.
	HDCoinType uint32

	// KeyPurposes are the BIP-43 purposes of the key scopes the wallet creates
	// and derives addresses under, one per address type the chain uses. The
	// zeroth entry is the primary scope, used for receiving, change, and
	// gap-limit recovery. BTC and LTC use {84} (native segwit, P2WPKH); Dash,
	// which has no SegWit, uses {44} (legacy P2PKH). The seed is app-specific and
	// never shared with externally-created wallets, so only these scopes are ever
	// created — there are no additional default scopes to discover.
	KeyPurposes []uint32

	BIP0034Height int32
	BIP0065Height int32
	BIP0066Height int32

	CoinbaseMaturity uint16

	// CheckPoW is a function that will check the proof-of-work validity for a
	// block header. If CheckPoW is nil, the standard Bitcoin protocol is used.
	CheckPoW func(*wire.BlockHeader) error

	// HeaderHashFunc, when non-nil, computes the chain's canonical block-header
	// hash — i.e. the block identity used for prev-block linkage, indexing, and
	// genesis/checkpoint comparison. Most chains identify blocks by
	// double-SHA256, which is exactly wire.BlockHeader.BlockHash(), so they leave
	// this nil. Chains whose block identity differs set it: Dash identifies
	// blocks by the X11 hash of the serialized 80-byte header (the same value as
	// its PoW hash), so wire.BlockHeader.BlockHash() (sha256d) is wrong for it.
	// Any code that links or indexes headers by hash must go through HeaderHash,
	// not BlockHash() directly. Transaction ids are unaffected (still sha256d).
	HeaderHashFunc func(*wire.BlockHeader) chainhash.Hash

	// NextBits returns the difficulty bits consensus requires of the block at
	// height, reading the difficulty fields of its ancestors through at, and
	// false when an input the rule reads is unavailable. Nil disables
	// contextual difficulty checks. It is nil on every network that permits
	// minimum-difficulty blocks (ReduceMinDifficulty) or does not retarget
	// (PoWNoRetargeting), where difficulty does not bound the cost of a forged
	// chain whatever the client checks.
	NextBits func(height int32, at DifficultyAt) (uint32, bool)

	// DifficultyLookback is the number of blocks below a checkpoint whose
	// difficulty fields NextBits reads when validating the blocks just above
	// it. Each checkpoint record carries that many Difficulty entries.
	DifficultyLookback int

	// MaxSatoshi varies between assets.
	MaxSatoshi int64

	// ElectrumServers is the list of built-in electrumx servers for this
	// (chain, network) combination. Consumers pick the first entry; the list
	// exists to allow a fallback hierarchy in the future.
	ElectrumServers []*ServerAddr

	// NeutrinoSeeds is the list of default peer addresses for the neutrino
	// (BIP-157/158) chain service. Nil/empty on mainnet (DNS seeds suffice).
	NeutrinoSeeds []string

	btcdParamsOnce sync.Once
	btcdParams     *chaincfg.Params
}

// Checkpoint is a block whose identity is fixed in the chain parameters.
// Header sync rejects any chain that does not pass through it. The full header
// and the filter header let a header store start at the checkpoint instead of
// at genesis, trusting both as it would the genesis block.
type Checkpoint struct {
	Height int32
	// Hash is the block hash under the chain's HeaderHash. It duplicates
	// HeaderHash(Header) so the value can be compared against block
	// explorers directly.
	Hash *chainhash.Hash
	// Header is the block header.
	Header *wire.BlockHeader
	// FilterHeader is the BIP-158 basic filter header of the block, which
	// commits to the filters of the block and all of its ancestors.
	FilterHeader *chainhash.Hash
	// Difficulty holds the difficulty fields of the DifficultyLookback blocks
	// below the checkpoint, oldest first, ending at Height-1. A header store
	// starting at the checkpoint has no other source for them, and NextBits
	// reads them when validating the blocks just above the checkpoint. They
	// are trusted on the same footing as Hash.
	Difficulty []BlockDifficulty
}

// BlockDifficulty is the part of a block header that difficulty rules read.
type BlockDifficulty struct {
	Bits      uint32
	Timestamp uint32 // Unix seconds, as encoded in the header
}

// HeaderDifficulty returns the difficulty fields of hdr.
func HeaderDifficulty(hdr *wire.BlockHeader) BlockDifficulty {
	return BlockDifficulty{Bits: hdr.Bits, Timestamp: uint32(hdr.Timestamp.Unix())}
}

// DifficultyAt returns the difficulty fields of the block at height on the
// chain being validated, and false when they are unavailable.
type DifficultyAt func(height int32) (BlockDifficulty, bool)

// HeaderHash returns the canonical block-header hash (block identity) for hdr on
// this chain. It uses HeaderHashFunc when set and otherwise falls back to the
// standard double-SHA256 (wire.BlockHeader.BlockHash). It is nil-safe so callers
// that may not have chain params (e.g. some tests) still get the default.
func (c *ChainParams) HeaderHash(hdr *wire.BlockHeader) chainhash.Hash {
	if c != nil && c.HeaderHashFunc != nil {
		return c.HeaderHashFunc(hdr)
	}
	return hdr.BlockHash()
}

// BTCDParams are the btcd.chaincfg.Params that correspond the ChainParams.
func (c *ChainParams) BTCDParams() *chaincfg.Params {
	c.btcdParamsOnce.Do(c.initBTCDParams)
	return c.btcdParams
}

func (c *ChainParams) initBTCDParams() {
	c.btcdParams = &chaincfg.Params{
		Name:                     c.Name,
		Net:                      c.Net,
		GenesisBlock:             c.GenesisBlock,
		GenesisHash:              c.GenesisHash,
		TargetTimespan:           c.TargetTimespan,
		TargetTimePerBlock:       c.TargetTimePerBlock,
		RetargetAdjustmentFactor: c.RetargetAdjustmentFactor,
		ReduceMinDifficulty:      c.ReduceMinDifficulty,
		MinDiffReductionTime:     c.MinDiffReductionTime,
		Checkpoints:              c.btcdCheckpoints(),
		PowLimit:                 c.PowLimit,
		PowLimitBits:             c.PowLimitBits,
		PoWNoRetargeting:         c.PoWNoRetargeting,
		DNSSeeds:                 c.DNSSeeds,
		DefaultPort:              c.DefaultPort,
		Bech32HRPSegwit:          c.Bech32HRPSegwit,
		PubKeyHashAddrID:         c.PubKeyHashAddrID,
		ScriptHashAddrID:         c.ScriptHashAddrID,
		PrivateKeyID:             c.PrivateKeyID,
		WitnessPubKeyHashAddrID:  c.WitnessPubKeyHashAddrID,
		WitnessScriptHashAddrID:  c.WitnessScriptHashAddrID,
		HDPrivateKeyID:           c.HDPrivateKeyID,
		HDPublicKeyID:            c.HDPublicKeyID,
		HDCoinType:               c.HDCoinType,
		BIP0034Height:            c.BIP0034Height,
		BIP0065Height:            c.BIP0065Height,
		BIP0066Height:            c.BIP0066Height,
		CoinbaseMaturity:         c.CoinbaseMaturity,
	}
}

func (c *ChainParams) btcdCheckpoints() []chaincfg.Checkpoint {
	if len(c.Checkpoints) == 0 {
		return nil
	}
	cps := make([]chaincfg.Checkpoint, len(c.Checkpoints))
	for i, cp := range c.Checkpoints {
		cps[i] = chaincfg.Checkpoint{Height: cp.Height, Hash: cp.Hash}
	}
	return cps
}
