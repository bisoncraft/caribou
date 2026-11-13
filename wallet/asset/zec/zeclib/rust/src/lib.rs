// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Go FFI backend for a Zcash light wallet.
//!
//! This crate is a thin, synchronous wrapper around the librustzcash wallet
//! crates (`zcash_client_backend` and `zcash_client_sqlite`). It owns the
//! wallet database and nothing else: it makes no network connections and
//! starts no threads of its own. The caller fetches compact blocks, tree
//! states and subtree roots from a lightwalletd server and hands them to
//! [`Wallet`] as protobuf-encoded bytes, in the same division of labor as the
//! Zcash Android wallet SDK, where the Kotlin block processor drives a Rust
//! backend.
//!
//! The interface is exported with uniffi proc-macros. The Go bindings are
//! generated from the compiled library by uniffi-bindgen-go.
//!
//! Every exported call that can fail returns [`WalletError`]. Panics raised
//! inside a call are caught and reported as [`WalletError::Internal`], so a
//! bug in this crate or in librustzcash reaches Go as an error and not as a
//! panic.

// The namespace becomes the name of the generated Go package.
uniffi::setup_scaffolding!("ffi");

mod address;
mod blocks;
mod error;
mod network;
mod types;
mod wallet;

pub use address::transparent_receiver;
pub use error::WalletError;
pub use network::{ActivationHeights, Network};
pub use types::{
    Account, AccountBalance, OutputStatusFilter, PoolBalance, Ratio, ReceiverSet, ScanPriority,
    ScanRange, ScanSummary, SubtreeRoot, TransactionDataRequest, TransactionStatus,
    TransactionStatusFilter, Utxo, WalletSummary, WatchedAddress,
};
pub use wallet::Wallet;

#[cfg(test)]
mod testutil;
