// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

/*
Package zeclib is a Zcash light wallet backend for Go. It keeps a wallet
database in sync with the chain through a lightwalletd server, and reports
accounts, addresses, balances and sync progress.

Ironwood, the pool added by NU6.3, is the only shielded pool the wallet
supports. An account holds funds in the Ironwood pool and in transparent
outputs, and its addresses have no Sapling receiver.

The wallet logic is librustzcash, the Rust library behind the Zcash mobile
wallets, reached through generated bindings. The package is organized the way
the Zcash Android wallet SDK is:

  - A Rust crate (the rust directory) wraps the librustzcash wallet database
    in a small set of synchronous calls: create an account, store subtree
    roots, update the chain tip, suggest scan ranges, scan a batch of blocks,
    rewind. It does no networking and starts no threads.
  - Package walletrpc is the lightwalletd gRPC client, generated from the
    protocol's .proto files.
  - This package talks to the server, decides what to download, hands blocks
    to the Rust backend, and recovers from chain reorganizations. Everything
    that runs for a long time is a goroutine here, so it is controlled with a
    context.Context and awaited with a sync.WaitGroup like any other Go code.

# Use

Open opens or creates a wallet database. A new wallet needs an account, which
[Wallet.CreateAccount] derives from a seed. [Wallet.Connect] starts syncing
and returns a WaitGroup that is done when syncing has fully stopped, which
happens when the context passed to Connect is cancelled:

	w, err := zeclib.Open(&zeclib.Config{
		DBPath:  filepath.Join(dir, "wallet.sqlite"),
		Network: zeclib.Mainnet,
		Server:  "zec.rocks:443",
	}, nil)
	if err != nil {
		return err
	}
	defer w.Close()

	wg, err := w.Connect(ctx)
	if err != nil {
		return err
	}
	defer wg.Wait()

Connect does not need the server to be reachable. The wallet keeps trying to
reach it, and [Wallet.SyncStatus] reports whether it is connected, how far it
has scanned, and the last error. A wallet that was stopped can be connected
again.

# Building

The Rust backend is linked statically and is only compiled in with the "zec"
build tag and cgo. Without the tag, Open returns [ErrNotBuilt] and the rest
of the package still compiles, so code that imports it builds everywhere.
build.sh builds the static library and regenerates the bindings. See
README.md.

# Privacy

A lightwalletd server learns the block ranges a wallet downloads, the
transparent addresses it asks about, the IDs of the transactions it fetches
in full, and the birthday of an account created with [Wallet.CreateAccount].
[Wallet.CreateAccountWithTreeState] keeps the birthday from the server, and
Config.Dialer routes the connection through a proxy.
*/
package zeclib
