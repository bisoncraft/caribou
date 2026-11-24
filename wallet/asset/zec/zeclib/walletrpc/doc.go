// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package walletrpc is the gRPC protocol of lightwalletd, the server that
// Zcash light wallets sync through.
//
// The .proto files in this directory are unmodified copies from
// https://github.com/zcash/lightwallet-protocol at tag v0.5.0, which is the
// version that adds the Ironwood shielded pool. They are distributed under
// the MIT license in the LICENSE file beside them. The .pb.go files are
// generated from them by gen.sh and are not edited by hand.
package walletrpc
