// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zeclib

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc"
)

const (
	// rpcTimeout bounds a call that returns a single message.
	rpcTimeout = 30 * time.Second
	// streamTimeout bounds a call that streams its reply.
	streamTimeout = 5 * time.Minute
	// maxMessageSize is the largest message accepted from a server. A
	// compact block is at most a few megabytes. The gRPC default of 4 MiB
	// is too close to that.
	maxMessageSize = 32 << 20
)

// errStopStream is returned by a stream callback to end the stream early
// without an error.
var errStopStream = errors.New("stop stream")

// serverError is an error from, or about, the lightwalletd server: it was
// unreachable, a call failed, or a reply was malformed. It is distinguished
// from a wallet database error because the two call for different reactions.
type serverError struct {
	op  string
	err error
}

func (e *serverError) Error() string { return "lightwalletd " + e.op + ": " + e.err.Error() }
func (e *serverError) Unwrap() error { return e.err }

// isServerError reports whether err came from the server side.
func isServerError(err error) bool {
	var se *serverError
	return errors.As(err, &se)
}

// chainSource is the part of the lightwalletd protocol the wallet uses. Every
// error it returns that is not from a callback is a *serverError.
type chainSource interface {
	info(ctx context.Context) (*walletrpc.LightdInfo, error)
	// latestBlock returns the height and hash of the server's chain tip.
	latestBlock(ctx context.Context) (*walletrpc.BlockID, error)
	block(ctx context.Context, height uint32) (*walletrpc.CompactBlock, error)
	// blockRange calls fn for each block from start through end, in
	// ascending order. fn may return errStopStream to end the call early.
	// Any other error from fn is returned as is.
	blockRange(ctx context.Context, start, end uint32, fn func(*walletrpc.CompactBlock) error) error
	treeState(ctx context.Context, height uint32) (*walletrpc.TreeState, error)
	// subtreeRoots returns the roots of the completed subtrees of a note
	// commitment tree, from startIndex on.
	subtreeRoots(ctx context.Context, pool walletrpc.ShieldedProtocol, startIndex uint64) ([]subtreeRoot, error)
	// addressUTXOs calls fn for each unspent output of the transparent
	// addresses that was mined at startHeight or above.
	addressUTXOs(ctx context.Context, addresses []string, startHeight uint32, fn func(*walletrpc.GetAddressUtxosReply) error) error
	// transaction returns the full transaction with the given ID, or nil if
	// the server does not know it.
	transaction(ctx context.Context, txid []byte) (*walletrpc.RawTransaction, error)
	// addressTransactions calls fn for each mined transaction that involves
	// the transparent address in the blocks from start through end.
	addressTransactions(ctx context.Context, address string, start, end uint32, fn func(*walletrpc.RawTransaction) error) error
	close() error
}

// lwdClient is a chainSource backed by a gRPC connection to a lightwalletd
// server.
type lwdClient struct {
	conn   *grpc.ClientConn
	client walletrpc.CompactTxStreamerClient
}

var _ chainSource = (*lwdClient)(nil)

// dialLightwalletd prepares a connection to the server in cfg. It does not
// contact the server: gRPC connects on the first call and reconnects on its
// own after a failure.
func dialLightwalletd(cfg *Config) (*lwdClient, error) {
	if cfg.Server == "" {
		return nil, wrapf(ErrInvalidArgument, "no lightwalletd server is configured")
	}
	if _, _, err := net.SplitHostPort(cfg.Server); err != nil {
		return nil, wrapf(ErrInvalidArgument, "lightwalletd server %q: %v", cfg.Server, err)
	}

	var creds credentials.TransportCredentials
	switch {
	case cfg.Plaintext:
		creds = insecure.NewCredentials()
	case cfg.TLSConfig != nil:
		creds = credentials.NewTLS(cfg.TLSConfig.Clone())
	default:
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageSize)),
	}
	if cfg.Dialer != nil {
		opts = append(opts, grpc.WithContextDialer(cfg.Dialer))
	}

	// The passthrough scheme hands the address to the dialer as written.
	// With the default scheme gRPC would resolve the host name itself,
	// which would bypass a proxy dialer.
	conn, err := grpc.NewClient("passthrough:///"+cfg.Server, opts...)
	if err != nil {
		return nil, wrapf(ErrInvalidArgument, "lightwalletd server %q: %v", cfg.Server, err)
	}
	return &lwdClient{conn: conn, client: walletrpc.NewCompactTxStreamerClient(conn)}, nil
}

func (c *lwdClient) close() error {
	return c.conn.Close()
}

func (c *lwdClient) info(ctx context.Context) (*walletrpc.LightdInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	info, err := c.client.GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		return nil, &serverError{"GetLightdInfo", err}
	}
	return info, nil
}

func (c *lwdClient) latestBlock(ctx context.Context) (*walletrpc.BlockID, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	id, err := c.client.GetLatestBlock(ctx, &walletrpc.ChainSpec{})
	if err != nil {
		return nil, &serverError{"GetLatestBlock", err}
	}
	return id, nil
}

func (c *lwdClient) block(ctx context.Context, height uint32) (*walletrpc.CompactBlock, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	b, err := c.client.GetBlock(ctx, &walletrpc.BlockID{Height: uint64(height)})
	if err != nil {
		return nil, &serverError{fmt.Sprintf("GetBlock(%d)", height), err}
	}
	return b, nil
}

func (c *lwdClient) treeState(ctx context.Context, height uint32) (*walletrpc.TreeState, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	ts, err := c.client.GetTreeState(ctx, &walletrpc.BlockID{Height: uint64(height)})
	if err != nil {
		return nil, &serverError{fmt.Sprintf("GetTreeState(%d)", height), err}
	}
	return ts, nil
}

// drain reads a server stream to its end, passing each message to fn. It
// returns nil if fn ends the stream with errStopStream, the error of fn if
// it returns another one, and a *serverError if the stream fails.
func drain[T any](op string, stream grpc.ServerStreamingClient[T], fn func(*T) error) error {
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return &serverError{op, err}
		}
		if err := fn(msg); err != nil {
			if errors.Is(err, errStopStream) {
				return nil
			}
			return err
		}
	}
}

func (c *lwdClient) blockRange(ctx context.Context, start, end uint32, fn func(*walletrpc.CompactBlock) error) error {
	// Cancelling the context is also what ends the stream when fn stops
	// reading it early.
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	op := fmt.Sprintf("GetBlockRange(%d, %d)", start, end)
	stream, err := c.client.GetBlockRange(ctx, &walletrpc.BlockRange{
		Start: &walletrpc.BlockID{Height: uint64(start)},
		End:   &walletrpc.BlockID{Height: uint64(end)},
	})
	if err != nil {
		return &serverError{op, err}
	}
	return drain(op, stream, fn)
}

func (c *lwdClient) subtreeRoots(ctx context.Context, pool walletrpc.ShieldedProtocol, startIndex uint64) ([]subtreeRoot, error) {
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	op := fmt.Sprintf("GetSubtreeRoots(%s, %d)", pool, startIndex)
	if startIndex > uint64(^uint32(0)) {
		return nil, &serverError{op, errors.New("subtree index out of range")}
	}
	stream, err := c.client.GetSubtreeRoots(ctx, &walletrpc.GetSubtreeRootsArg{
		StartIndex:       uint32(startIndex),
		ShieldedProtocol: pool,
	})
	if err != nil {
		return nil, &serverError{op, err}
	}
	var roots []subtreeRoot
	err = drain(op, stream, func(r *walletrpc.SubtreeRoot) error {
		if r.CompletingBlockHeight > uint64(^uint32(0)) {
			return &serverError{op, errors.New("block height out of range")}
		}
		roots = append(roots, subtreeRoot{
			hash:             r.RootHash,
			completingHeight: uint32(r.CompletingBlockHeight),
		})
		return nil
	})
	return roots, err
}

func (c *lwdClient) addressUTXOs(ctx context.Context, addresses []string, startHeight uint32, fn func(*walletrpc.GetAddressUtxosReply) error) error {
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	const op = "GetAddressUtxosStream"
	stream, err := c.client.GetAddressUtxosStream(ctx, &walletrpc.GetAddressUtxosArg{
		Addresses:   addresses,
		StartHeight: uint64(startHeight),
	})
	if err != nil {
		return &serverError{op, err}
	}
	return drain(op, stream, fn)
}

// isUnsupported reports whether err is a server's way of saying that it does
// not implement a call, or does not know one of the values a request can
// carry, such as a shielded pool added after the server was built.
func isUnsupported(err error) bool {
	if !isServerError(err) {
		return false
	}
	code := status.Code(err)
	return code == codes.Unimplemented || code == codes.InvalidArgument
}

// isConnectionError reports whether err is a failure of the connection to
// the server or of the server as a whole, as opposed to a failure of the one
// request that returned it.
func isConnectionError(err error) bool {
	if !isServerError(err) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

// Servers do not agree on how to say that a transaction is unknown.
// lightwalletd passes through the node's error text with an unspecific
// status code.
var txNotFoundMessages = []string{
	"transaction not found",
	"no such mempool or main chain transaction",
	"no such mempool transaction",
}

func isTxNotFound(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	if st.Code() == codes.NotFound {
		return true
	}
	msg := strings.ToLower(st.Message())
	for _, m := range txNotFoundMessages {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

func (c *lwdClient) transaction(ctx context.Context, txid []byte) (*walletrpc.RawTransaction, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	tx, err := c.client.GetTransaction(ctx, &walletrpc.TxFilter{Hash: txid})
	if err != nil {
		if isTxNotFound(err) {
			return nil, nil
		}
		return nil, &serverError{"GetTransaction", err}
	}
	return tx, nil
}

func (c *lwdClient) addressTransactions(ctx context.Context, address string, start, end uint32, fn func(*walletrpc.RawTransaction) error) error {
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	// GetTaddressTxids is the older name of GetTaddressTransactions. It is
	// used because every server implements it, which is not yet true of the
	// newer one.
	const op = "GetTaddressTxids"
	stream, err := c.client.GetTaddressTxids(ctx, &walletrpc.TransparentAddressBlockFilter{
		Address: address,
		Range: &walletrpc.BlockRange{
			Start: &walletrpc.BlockID{Height: uint64(start)},
			End:   &walletrpc.BlockID{Height: uint64(end)},
		},
	})
	if err != nil {
		return &serverError{op, err}
	}
	return drain(op, stream, fn)
}
