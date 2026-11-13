// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

use std::any::Any;
use std::panic::{self, AssertUnwindSafe};

use zcash_client_backend::{data_api::chain::error::Error as ChainError, scanning::ScanError};
use zcash_client_sqlite::{error::SqliteClientError, wallet::init::WalletMigrationError};

/// The error type of every fallible call in this crate.
///
/// Variants that the caller is expected to branch on carry their data in
/// fields. The remaining variants carry a message only.
#[derive(Debug, thiserror::Error, uniffi::Error)]
pub enum WalletError {
    /// An argument was malformed or out of range. Retrying with the same
    /// arguments fails the same way.
    #[error("invalid argument: {message}")]
    InvalidArgument { message: String },

    /// The wallet database reported an error.
    #[error("wallet database: {message}")]
    Database { message: String },

    /// A pending database migration needs the wallet seed. Opening the wallet
    /// again with the seed resolves it.
    #[error("the wallet seed is required to migrate the wallet database")]
    SeedRequired,

    /// The seed given for a database migration does not belong to any account
    /// in the wallet.
    #[error("the seed is not relevant to any account in the wallet")]
    SeedNotRelevant,

    /// The account identifier does not name an account in the wallet.
    #[error("the account is not known to the wallet")]
    AccountNotFound,

    /// A new address with a transparent receiver cannot be generated: the
    /// wallet already holds as many unused transparent addresses as its gap
    /// limit allows. Funds sent beyond the gap would not be found when the
    /// wallet is restored from its seed. The limit lifts once one of the
    /// unused addresses receives funds.
    #[error("transparent address gap limit reached: {message}")]
    AddressGapLimit { message: String },

    /// The scanned blocks do not connect to the chain the wallet has already
    /// scanned. This is how a chain reorganization is detected. The caller
    /// recovers by rewinding the wallet to a height below `at_height` and
    /// scanning again.
    #[error("chain continuity error at height {at_height}: {message}")]
    ChainContinuity { at_height: u32, message: String },

    /// The wallet cannot be rewound to the requested height. When
    /// `safe_height` is set, a rewind to that height succeeds.
    #[error("the wallet cannot be rewound to height {requested_height}")]
    RewindInvalid {
        requested_height: u32,
        safe_height: Option<u32>,
    },

    /// A block could not be scanned for a reason other than chain continuity.
    #[error("scan error at height {at_height}: {message}")]
    Scan { at_height: u32, message: String },

    /// A bug: a panic was caught at the FFI boundary.
    #[error("internal error: {message}")]
    Internal { message: String },
}

impl WalletError {
    pub(crate) fn invalid(message: impl Into<String>) -> Self {
        WalletError::InvalidArgument {
            message: message.into(),
        }
    }

    pub(crate) fn database(e: impl std::fmt::Display) -> Self {
        WalletError::Database {
            message: e.to_string(),
        }
    }
}

impl From<SqliteClientError> for WalletError {
    fn from(e: SqliteClientError) -> Self {
        match e {
            SqliteClientError::RequestedRewindInvalid {
                safe_rewind_height,
                requested_height,
            } => WalletError::RewindInvalid {
                requested_height: requested_height.into(),
                safe_height: safe_rewind_height.map(u32::from),
            },
            SqliteClientError::AccountUnknown => WalletError::AccountNotFound,
            e @ SqliteClientError::ReachedGapLimit(..) => WalletError::AddressGapLimit {
                message: e.to_string(),
            },
            e => WalletError::database(e),
        }
    }
}

impl From<ScanError> for WalletError {
    fn from(e: ScanError) -> Self {
        let at_height = e.at_height().into();
        let message = e.to_string();
        if e.is_continuity_error() {
            WalletError::ChainContinuity { at_height, message }
        } else {
            WalletError::Scan { at_height, message }
        }
    }
}

impl<BlockSourceErr: std::fmt::Debug> From<ChainError<SqliteClientError, BlockSourceErr>>
    for WalletError
{
    fn from(e: ChainError<SqliteClientError, BlockSourceErr>) -> Self {
        match e {
            ChainError::Wallet(e) => e.into(),
            ChainError::Scan(e) => e.into(),
            ChainError::BlockSource(e) => WalletError::Internal {
                message: format!("block source: {e:?}"),
            },
            // The enum is non-exhaustive.
            e => WalletError::Internal {
                message: format!("unrecognized chain error: {e:?}"),
            },
        }
    }
}

/// Maps a failed database migration. The seed conditions are reported as
/// their own variants because the caller acts on them.
pub(crate) fn migration_error<E>(e: E) -> WalletError
where
    E: std::error::Error + 'static,
{
    match e
        .source()
        .and_then(|s| s.downcast_ref::<WalletMigrationError>())
    {
        Some(WalletMigrationError::SeedRequired) => WalletError::SeedRequired,
        Some(WalletMigrationError::SeedNotRelevant) => WalletError::SeedNotRelevant,
        _ => WalletError::Database {
            message: format!("migration failed: {e}"),
        },
    }
}

/// Runs `f`, converting a panic into [`WalletError::Internal`].
///
/// Every exported call goes through this. uniffi reports an uncaught Rust
/// panic to Go as a Go panic, which a wallet must not be exposed to.
pub(crate) fn guard<T>(f: impl FnOnce() -> Result<T, WalletError>) -> Result<T, WalletError> {
    match panic::catch_unwind(AssertUnwindSafe(f)) {
        Ok(res) => res,
        Err(payload) => Err(WalletError::Internal {
            message: panic_message(payload),
        }),
    }
}

fn panic_message(payload: Box<dyn Any + Send>) -> String {
    if let Some(s) = payload.downcast_ref::<&str>() {
        format!("panic: {s}")
    } else if let Some(s) = payload.downcast_ref::<String>() {
        format!("panic: {s}")
    } else {
        "panic with a non-string payload".to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use zcash_protocol::consensus::BlockHeight;

    #[test]
    fn guard_passes_results_through() {
        assert_eq!(guard(|| Ok(7)).unwrap(), 7);
        assert!(matches!(
            guard::<()>(|| Err(WalletError::SeedRequired)),
            Err(WalletError::SeedRequired)
        ));
    }

    #[test]
    fn guard_catches_panics() {
        let err = guard::<()>(|| panic!("static message")).unwrap_err();
        assert!(
            matches!(&err, WalletError::Internal { message } if message == "panic: static message"),
            "{err:?}"
        );

        let n = 3;
        let err = guard::<()>(|| panic!("formatted {n}")).unwrap_err();
        assert!(
            matches!(&err, WalletError::Internal { message } if message == "panic: formatted 3"),
            "{err:?}"
        );

        let err = guard::<()>(|| std::panic::panic_any(42_u8)).unwrap_err();
        assert!(
            matches!(&err, WalletError::Internal { message } if message.contains("non-string")),
            "{err:?}"
        );
    }

    #[test]
    fn rewind_error_keeps_heights() {
        let err: WalletError = SqliteClientError::RequestedRewindInvalid {
            safe_rewind_height: Some(BlockHeight::from_u32(90)),
            requested_height: BlockHeight::from_u32(50),
        }
        .into();
        assert!(matches!(
            err,
            WalletError::RewindInvalid {
                requested_height: 50,
                safe_height: Some(90)
            }
        ));

        let err: WalletError = SqliteClientError::RequestedRewindInvalid {
            safe_rewind_height: None,
            requested_height: BlockHeight::from_u32(50),
        }
        .into();
        assert!(matches!(
            err,
            WalletError::RewindInvalid {
                requested_height: 50,
                safe_height: None
            }
        ));
    }

    #[test]
    fn account_and_gap_limit_errors_are_typed() {
        let err: WalletError = SqliteClientError::AccountUnknown.into();
        assert!(matches!(err, WalletError::AccountNotFound), "{err:?}");

        let err: WalletError = SqliteClientError::ReachedGapLimit(
            transparent::keys::TransparentKeyScope::EXTERNAL,
            10,
        )
        .into();
        assert!(
            matches!(err, WalletError::AddressGapLimit { .. }),
            "{err:?}"
        );
    }

    #[test]
    fn other_database_errors_are_messages() {
        let err: WalletError = SqliteClientError::InvalidNote.into();
        assert!(matches!(err, WalletError::Database { .. }), "{err:?}");
    }

    #[test]
    fn scan_errors_split_on_continuity() {
        let err: WalletError = ScanError::PrevHashMismatch {
            at_height: BlockHeight::from_u32(12),
        }
        .into();
        assert!(
            matches!(err, WalletError::ChainContinuity { at_height: 12, .. }),
            "{err:?}"
        );

        let err: WalletError = ScanError::TreeSizeUnknown {
            protocol: zcash_protocol::ShieldedPool::Sapling,
            at_height: BlockHeight::from_u32(13),
        }
        .into();
        assert!(
            matches!(err, WalletError::Scan { at_height: 13, .. }),
            "{err:?}"
        );
    }

    #[test]
    fn chain_errors_unwrap_to_their_cause() {
        type E = ChainError<SqliteClientError, std::convert::Infallible>;

        let err: WalletError = E::Wallet(SqliteClientError::InvalidNote).into();
        assert!(matches!(err, WalletError::Database { .. }), "{err:?}");

        let err: WalletError = E::Scan(ScanError::PrevHashMismatch {
            at_height: BlockHeight::from_u32(5),
        })
        .into();
        assert!(
            matches!(err, WalletError::ChainContinuity { at_height: 5, .. }),
            "{err:?}"
        );

        let err: WalletError =
            ChainError::<SqliteClientError, &str>::BlockSource("unreadable").into();
        assert!(
            matches!(&err, WalletError::Internal { message } if message.contains("unreadable")),
            "{err:?}"
        );
    }

    #[test]
    fn display_is_readable() {
        assert_eq!(
            WalletError::invalid("bad").to_string(),
            "invalid argument: bad"
        );
        assert_eq!(
            WalletError::database("locked").to_string(),
            "wallet database: locked"
        );
    }
}
