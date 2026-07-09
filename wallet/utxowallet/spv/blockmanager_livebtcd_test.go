//go:build livebtcd

// Tests in this file spawn a real btcd instance via rpctest. They are gated
// behind the `livebtcd` build tag so that `go test ./...` stays runnable
// without btcd in PATH. To run them: `go test -tags=livebtcd ./...` (with a
// btcd binary discoverable on PATH).

package spv

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/bisoncraft/caribou/wallet/utxowallet/bisonwire"
	"github.com/bisoncraft/caribou/wallet/utxowallet/peer"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestHandleHeaders checks that we handle headers correctly, and that we
// disconnect peers that serve us bad headers (headers that don't connect to
// each other properly).
func TestHandleHeaders(t *testing.T) {
	t.Parallel()

	// First, we set up a block manager and a fake peer that will act as the
	// test's remote peer.
	bm, _, _, err := setupBlockManager(t)
	require.NoError(t, err)

	fakePeer, err := peer.NewOutboundPeer(&peer.Config{Chain: bisonwire.ChainBTC}, "fake:123")
	require.NoError(t, err)

	assertPeerDisconnected := func(shouldBeDisconnected bool) {
		// This is quite hacky but works: We expect the peer to be
		// disconnected, which sets the unexported "disconnected" field
		// to 1.
		refValue := reflect.ValueOf(fakePeer).Elem()
		foo := refValue.FieldByName("disconnect").Int()

		if shouldBeDisconnected {
			require.EqualValues(t, 1, foo)
		} else {
			require.EqualValues(t, 0, foo)
		}
	}

	// We'll want to use actual, real blocks, so we take a miner harness
	// that we can use to generate some.
	harness, err := rpctest.New(
		&chaincfg.SimNetParams, nil, []string{"--txindex"}, "",
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, harness.TearDown())
	})

	err = harness.SetUp(false, 0)
	require.NoError(t, err)

	// Generate 200 valid blocks that we then feed to the block manager.
	blockHashes, err := harness.Client.Generate(200)
	require.NoError(t, err)

	hmsg := &headersMsg{
		headers: &wire.MsgHeaders{
			Headers: make([]*wire.BlockHeader, len(blockHashes)),
		},
		peer: &ServerPeer{
			Peer: fakePeer,
		},
	}

	for i := range blockHashes {
		header, err := harness.Client.GetBlockHeader(blockHashes[i])
		require.NoError(t, err)

		hmsg.headers.Headers[i] = header
	}

	// Let's feed in the correct headers. This should work fine and the peer
	// should not be disconnected.
	bm.handleHeadersMsg(hmsg)
	assertPeerDisconnected(false)

	// Now scramble the headers and feed them in again. This should cause
	// the peer to be disconnected.
	rand.Shuffle(len(hmsg.headers.Headers), func(i, j int) {
		hmsg.headers.Headers[i], hmsg.headers.Headers[j] =
			hmsg.headers.Headers[j], hmsg.headers.Headers[i]
	})
	bm.handleHeadersMsg(hmsg)
	assertPeerDisconnected(true)
}
