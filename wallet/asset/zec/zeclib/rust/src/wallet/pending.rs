// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! The transactions the wallet created that still wait to be mined.

use zcash_client_backend::data_api::WalletRead;

use super::Wallet;
use crate::error::{WalletError, guard};

/// A transaction the wallet created that is neither mined nor expired.
#[derive(Debug, PartialEq, Eq, uniffi::Record)]
pub struct UnminedTransaction {
    /// The transaction ID in the byte order of the wire.
    pub txid: Vec<u8>,
    /// The serialized transaction.
    pub raw: Vec<u8>,
    /// The last height at which the transaction can be mined. Zero means it
    /// does not expire.
    pub expiry_height: u32,
}

#[uniffi::export]
impl Wallet {
    /// Lists the transactions this wallet created that are not mined and not
    /// expired, oldest first, for rebroadcast.
    ///
    /// A transaction counts as expired once its expiry height is below the
    /// next block, that is at or below the chain tip most recently given to
    /// [`update_chain_tip`](Wallet::update_chain_tip). Before the first chain
    /// tip every unmined transaction is listed. The list reflects what the
    /// wallet knows: a transaction that was mined in a block the wallet has
    /// not scanned is still listed.
    pub fn rebroadcast_inventory(&self) -> Result<Vec<UnminedTransaction>, WalletError> {
        guard(|| {
            let db = self.db();
            let tip = db.chain_height()?.map(u32::from);
            let reader = self.reader();
            let mut stmt = reader
                .prepare_cached(
                    "SELECT txid, raw, expiry_height FROM transactions
                     WHERE created IS NOT NULL
                       AND mined_height IS NULL
                       AND raw IS NOT NULL
                       AND (:tip IS NULL OR expiry_height IS NULL OR expiry_height = 0
                            OR expiry_height > :tip)
                     ORDER BY id_tx",
                )
                .map_err(WalletError::database)?;
            let rows = stmt
                .query_map(rusqlite::named_params! { ":tip": tip }, |row| {
                    Ok(UnminedTransaction {
                        txid: row.get(0)?,
                        raw: row.get(1)?,
                        expiry_height: row.get::<_, Option<u32>>(2)?.unwrap_or(0),
                    })
                })
                .map_err(WalletError::database)?;
            rows.collect::<Result<Vec<_>, _>>()
                .map_err(WalletError::database)
        })
    }
}
