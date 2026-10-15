package assets

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"

	"github.com/bisoncraft/caribou/wallet/utxowallet/netparams"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func NetParams(chain, net string) (p *netparams.ChainParams, err error) {
	switch chain {
	case "btc":
		p = BTCParams[net]
	case "ltc":
		p = LTCParams[net]
	case "dash":
		p = DashParams[net]
	}
	if p == nil {
		return nil, fmt.Errorf("no net params for chain %s, network %s", chain, net)
	}
	registerParams := *p.BTCDParams() // populate the internal btcParams field
	registerParams.Net = math.MaxUint32
	err = chaincfg.Register(&registerParams)
	return
}

type Chain struct {
	Params map[string]*netparams.ChainParams
}

var bigOne = big.NewInt(1)

func hashPointer(b [32]byte) *chainhash.Hash {
	var h chainhash.Hash
	copy(h[:], b[:])
	return &h
}

// newHashFromStr converts the passed big-endian hex string into a
// chainhash.Hash.  It only differs from the one available in chainhash in that
// it panics on an error since it will only (and must only) be called with
// hard-coded, and therefore known good, hashes.
func newHashFromStr(hexStr string) *chainhash.Hash {
	hash, err := chainhash.NewHashFromStr(hexStr)
	if err != nil {
		// Ordinarily I don't like panics in library code since it
		// can take applications down without them having a chance to
		// recover which is extremely annoying, however an exception is
		// being made in this case because the only way this can panic
		// is if there is an error in the hard-coded hashes.  Thus it
		// will only ever potentially panic on init and therefore is
		// 100% predictable.
		panic(err)
	}
	return hash
}

// newHeaderFromStr decodes the passed hex string of a serialized block header.
// Like newHashFromStr, it panics on an error and must only be called with
// hard-coded headers.
func newHeaderFromStr(hexStr string) *wire.BlockHeader {
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		panic(err)
	}
	if len(b) != wire.MaxBlockHeaderPayload {
		panic(fmt.Sprintf("header is %d bytes, want %d", len(b), wire.MaxBlockHeaderPayload))
	}
	var hdr wire.BlockHeader
	if err := hdr.Deserialize(bytes.NewReader(b)); err != nil {
		panic(err)
	}
	return &hdr
}
