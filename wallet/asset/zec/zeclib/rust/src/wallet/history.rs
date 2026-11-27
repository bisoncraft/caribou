// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! The wallet's transaction history, read from the `v_transactions` and
//! `v_tx_outputs` views of zcash_client_sqlite on the read-only connection.
//!
//! The views have one row per account and transaction. A record adds the
//! transaction's position in the order the wallet discovered transactions
//! (`seq`, the private `id_tx` column of the `transactions` table, which the
//! tests pin) and whether the wallet created it.

use rusqlite::{Connection, Row, named_params};
use zcash_client_backend::data_api::WalletRead;
use zcash_keys::address::Address;

use super::{Wallet, parse_account};
use crate::address::orchard_only;
use crate::error::{WalletError, guard};
use crate::network::Params;

/// The pool of a transaction output.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum OutputPool {
    Transparent,
    Sapling,
    Orchard,
    Ironwood,
}

impl OutputPool {
    /// Decodes the pool code of zcash_client_sqlite.
    fn from_code(code: i64) -> rusqlite::Result<Self> {
        match code {
            0 => Ok(OutputPool::Transparent),
            2 => Ok(OutputPool::Sapling),
            3 => Ok(OutputPool::Orchard),
            4 => Ok(OutputPool::Ironwood),
            _ => Err(rusqlite::Error::IntegralValueOutOfRange(0, code)),
        }
    }
}

/// An output of a transaction in the history: one that the account sent or
/// received.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct TxOutput {
    pub pool: OutputPool,
    pub output_index: u32,
    /// The value in zatoshis.
    pub value: u64,
    /// The address the output pays, when the wallet knows it. For an output
    /// of a transaction this wallet created, the address the payment named.
    /// For an output to the wallet in a transaction this wallet did not
    /// create (a payment received, or one another instance of the same seed
    /// made, as a restored wallet sees it), the receiver that was paid, as an
    /// address a sender could pay on its own: for an Orchard or Ironwood
    /// output the wallet's unified address pruned to its Orchard receiver,
    /// for a transparent output the bare transparent address. Such an address
    /// is not necessarily the string the wallet handed out, and it never has
    /// a Sapling receiver. A payment this wallet created to its own address
    /// keeps the address it named, which can have a Sapling receiver when the
    /// user paid that form. It is unknown for change and for a received
    /// output whose address the wallet did not generate.
    ///
    /// What a payment named is known only to the database that created it.
    /// For an Orchard or Ironwood output, the creating wallet records a
    /// payment to itself under the address the payment named, and a restored
    /// wallet records the same payment under the Orchard-only receiver: the
    /// same receiver, a different string. A transparent output is the
    /// t-address on both. A payment to someone else likewise: the creating
    /// wallet has the address named, a restored one the receiver its viewing
    /// key recovers.
    pub address: Option<String>,
    /// Whether the output pays one of the wallet's accounts.
    pub to_wallet: bool,
    /// Whether the output is change: set for every output to the account
    /// of a transaction the account funded, which includes a payment to one
    /// of its own addresses. Such a payment carries the address it was made
    /// to, and change carries none.
    pub is_change: bool,
    /// The memo of a shielded output, absent for a transparent one. A single
    /// 0xF6 byte is the empty memo.
    pub memo: Option<Vec<u8>>,
}

/// A transaction in an account's history.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct TxRecord {
    /// The position of the transaction in the order the wallet discovered
    /// transactions. It grows with discovery, not with height: a restore can
    /// find an old transaction late.
    pub seq: u64,
    /// The transaction ID in the byte order of the wire.
    pub txid: Vec<u8>,
    /// The height and time of the block that mined it, absent while it is
    /// unmined.
    pub mined_height: Option<u32>,
    pub block_time: Option<u64>,
    /// The last height at which it can be mined, zero if it does not expire,
    /// and absent when the wallet does not know it: for a transaction it only
    /// saw an output of in a compact block or an unspent-output reply.
    pub expiry_height: Option<u32>,
    /// Whether it is unmined and its expiry height is at or below the
    /// wallet's greatest scanned height. It is reliable only once the wallet
    /// is scanned to the tip: before, it can lag, and with blocks scanned out
    /// of order the greatest scanned height can be near the tip while the
    /// block that mined the transaction is not scanned yet, so a mined
    /// transaction can show as expired. False when the expiry is unknown.
    pub expired_unmined: bool,
    /// Whether this wallet created the transaction.
    pub created_by_wallet: bool,
    /// The change of the account's balance, in zatoshis.
    pub balance_delta: i64,
    /// The value of the account's inputs the transaction spent, and of its
    /// outputs that pay the account, in zatoshis.
    pub total_spent: u64,
    pub total_received: u64,
    /// Whether the wallet holds the full transaction. Without it, a
    /// transaction found by scanning shows only what compact blocks tell:
    /// the account's spent notes and its own outputs. A transaction another
    /// instance of the same seed created then looks like a payment to
    /// itself, and its outside output, recipient and fee appear once the
    /// full transaction is stored.
    pub raw_present: bool,
    /// The fee of the transaction in zatoshis, whoever paid it: on a
    /// receive it is the sender's. Absent while it is unknown. It is unknown when
    /// the transaction spends transparent inputs whose values the wallet does
    /// not know, such as a payment to the wallet from someone else's
    /// transparent outputs, and can stay unknown.
    pub fee: Option<u64>,
    /// Whether the transaction only moved the account's transparent funds
    /// into a shielded pool.
    pub is_shielding: bool,
    /// The outputs the account sent or received, in output order.
    pub outputs: Vec<TxOutput>,
}

/// Transactions of an account's history, with the chain tip the wallet knew
/// when it read them, against which a caller counts confirmations.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct TxListing {
    pub chain_tip: Option<u32>,
    pub records: Vec<TxRecord>,
}

/// One transaction of an account's history, if the account has it, with the
/// chain tip the wallet knew when it read it.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct TxLookup {
    pub chain_tip: Option<u32>,
    pub record: Option<TxRecord>,
}

/// The columns of a record, with `t` the transactions table and `v` the
/// v_transactions view.
const RECORD_COLUMNS: &str = "t.id_tx, v.txid, v.mined_height, v.block_time, v.expiry_height, \
     IFNULL(v.expired_unmined, 0), t.created IS NOT NULL, v.account_balance_delta, v.total_spent, \
     v.total_received, v.fee_paid, v.is_shielding, t.raw IS NOT NULL";

fn record_query(filter: &str, tail: &str) -> String {
    format!(
        "SELECT {RECORD_COLUMNS}
         FROM v_transactions v
         JOIN transactions t ON t.txid = v.txid
         WHERE v.account_uuid = :account AND ({filter})
         ORDER BY t.id_tx {tail}"
    )
}

fn check_txid(txid: &[u8]) -> Result<(), WalletError> {
    if txid.len() != 32 {
        return Err(WalletError::invalid("a transaction ID is 32 bytes"));
    }
    Ok(())
}

fn u64_column(row: &Row<'_>, i: usize) -> rusqlite::Result<u64> {
    let v: i64 = row.get(i)?;
    u64::try_from(v).map_err(|_| rusqlite::Error::IntegralValueOutOfRange(i, v))
}

fn optional_u64(row: &Row<'_>, i: usize) -> rusqlite::Result<Option<u64>> {
    row.get::<_, Option<i64>>(i)?
        .map(|v| u64::try_from(v).map_err(|_| rusqlite::Error::IntegralValueOutOfRange(i, v)))
        .transpose()
}

fn record_from_row(row: &Row<'_>) -> rusqlite::Result<TxRecord> {
    Ok(TxRecord {
        seq: u64_column(row, 0)?,
        txid: row.get(1)?,
        mined_height: row.get(2)?,
        block_time: optional_u64(row, 3)?,
        expiry_height: row.get(4)?,
        expired_unmined: row.get(5)?,
        created_by_wallet: row.get(6)?,
        balance_delta: row.get(7)?,
        total_spent: u64_column(row, 8)?,
        total_received: u64_column(row, 9)?,
        fee: optional_u64(row, 10)?,
        is_shielding: row.get(11)?,
        raw_present: row.get(12)?,
        outputs: vec![],
    })
}

/// The address of an output to the wallet that this wallet did not create, as
/// the history reports it: the receiver that was paid, as an address a sender
/// can pay on its own. The view gives the address the output's address row
/// holds. ensure_address does not force an update, so upsert_address keeps a
/// row's address and writes the full address of every receiver the viewing
/// key can produce (ALLOW_ALL) only when it creates the row: a diversifier
/// first met by scanning, as in a restored database, gets a row with the
/// full address, and the row of the account's default address holds the full
/// address from the account's creation. Either way it has a Sapling receiver
/// and is not the string the wallet hands out. An Orchard or Ironwood output
/// is reported as that
/// address pruned to its Orchard receiver; a Sapling output has no address
/// this wallet hands out; a transparent output already comes as the bare
/// transparent address. A missing address, as for change, stays missing:
/// the library gives an internal-scope output no address row
/// (zcash_client_sqlite `ensure_address`), so recovered change has none.
fn received_address(
    params: &Params,
    pool: OutputPool,
    address: Option<String>,
) -> Result<Option<String>, WalletError> {
    match pool {
        OutputPool::Transparent => Ok(address),
        OutputPool::Sapling => Ok(None),
        OutputPool::Orchard | OutputPool::Ironwood => {
            match address.as_deref().and_then(|a| Address::decode(params, a)) {
                Some(Address::Unified(ua)) => orchard_only(params, &ua),
                _ => Ok(None),
            }
        }
    }
}

/// Reads the records a query selects, with their outputs. The rows are
/// materialized before the caller releases the lock.
fn read_records(
    net: &Params,
    conn: &Connection,
    account: &[u8],
    filter: &str,
    tail: &str,
    params: &[(&str, &dyn rusqlite::ToSql)],
) -> Result<Vec<TxRecord>, WalletError> {
    let mut all: Vec<(&str, &dyn rusqlite::ToSql)> = vec![(":account", &account)];
    all.extend_from_slice(params);
    let mut stmt = conn
        .prepare(&record_query(filter, tail))
        .map_err(WalletError::database)?;
    let mut records = stmt
        .query_map(all.as_slice(), record_from_row)
        .map_err(WalletError::database)?
        .collect::<Result<Vec<_>, _>>()
        .map_err(WalletError::database)?;

    let mut outputs = conn
        .prepare_cached(
            "SELECT o.output_pool, o.output_index, o.value, o.to_address,
                    o.to_account_uuid IS NOT NULL, o.is_change, o.memo,
                    EXISTS (SELECT 1 FROM sent_notes s
                            WHERE s.transaction_id = o.transaction_id
                              AND s.output_pool = o.output_pool
                              AND s.output_index = o.output_index)
             FROM v_tx_outputs o
             WHERE o.transaction_id = :id
               AND (o.from_account_uuid = :account OR o.to_account_uuid = :account)
             ORDER BY o.output_pool, o.output_index",
        )
        .map_err(WalletError::database)?;
    for record in &mut records {
        let id = record.seq as i64;
        let created = record.created_by_wallet;
        record.outputs = outputs
            .query_map(named_params! { ":id": id, ":account": account }, |row| {
                let output = TxOutput {
                    pool: OutputPool::from_code(row.get(0)?)?,
                    output_index: row.get(1)?,
                    value: u64_column(row, 2)?,
                    address: row.get(3)?,
                    to_wallet: row.get(4)?,
                    is_change: row.get(5)?,
                    memo: row.get(6)?,
                };
                let sent: bool = row.get(7)?;
                Ok((output, sent))
            })
            .map_err(WalletError::database)?
            .collect::<Result<Vec<_>, _>>()
            .map_err(WalletError::database)?
            .into_iter()
            .map(|(mut output, sent)| {
                // What a payment named is known only to the database that
                // created it. A restored wallet, or a second device on the
                // same seed, recovers the output with the viewing key and
                // the library records it under the full address of its own
                // address row, Sapling receiver included, in a sent row that
                // looks authoritative. So an output to the wallet keeps the
                // sent row's address only when this wallet created the
                // transaction.
                if output.to_wallet && !(sent && created) {
                    output.address = received_address(net, output.pool, output.address)?;
                }
                Ok(output)
            })
            .collect::<Result<Vec<_>, WalletError>>()?;
    }
    Ok(records)
}

#[uniffi::export]
impl Wallet {
    /// Lists the account's transactions with a `seq` above `after_seq`, at
    /// most `limit` of them, in `seq` order. A listing from zero, continued
    /// from the last `seq` returned, reads the whole history. A listing from
    /// a watermark reads the transactions discovered since, whatever their
    /// height.
    ///
    /// A transaction can be linked to the account after its row was stored,
    /// and then appears with its old, lower `seq`, below a watermark: for one,
    /// a transaction stored with a spend of a transparent output that the
    /// wallet learns of later. A full listing finds it.
    pub fn transactions(
        &self,
        account_uuid: String,
        after_seq: u64,
        limit: u32,
    ) -> Result<TxListing, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let db = self.db();
            if db.get_account(uuid)?.is_none() {
                return Err(WalletError::AccountNotFound);
            }
            let chain_tip = db.chain_height()?.map(u32::from);
            let reader = self.reader();
            let after = i64::try_from(after_seq).unwrap_or(i64::MAX);
            let limit = i64::from(limit);
            let records = read_records(
                &self.params,
                &reader,
                uuid.expose_uuid().as_bytes(),
                "t.id_tx > :after",
                "LIMIT :limit",
                &[(":after", &after), (":limit", &limit)],
            )?;
            Ok(TxListing { chain_tip, records })
        })
    }

    /// Returns one transaction of the account's history, or nothing if the
    /// account has no transaction with that ID. `txid` is in the byte order
    /// of the wire.
    pub fn transaction(
        &self,
        account_uuid: String,
        txid: Vec<u8>,
    ) -> Result<TxLookup, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            check_txid(&txid)?;
            let db = self.db();
            if db.get_account(uuid)?.is_none() {
                return Err(WalletError::AccountNotFound);
            }
            let chain_tip = db.chain_height()?.map(u32::from);
            let reader = self.reader();
            let record = read_records(
                &self.params,
                &reader,
                uuid.expose_uuid().as_bytes(),
                "v.txid = :txid",
                "",
                &[(":txid", &txid)],
            )?
            .pop();
            Ok(TxLookup { chain_tip, record })
        })
    }

    /// Lists the account's transactions whose record can still change: the
    /// unmined ones, those with at most `depth` confirmations, those mined
    /// in a block the wallet has not scanned yet (whose block time is still
    /// unknown), those whose raw transaction the wallet lacks, and those
    /// funded by the account whose fee is unknown. A receive paid from
    /// someone else's transparent outputs keeps an unknown fee for good and
    /// is not listed for that alone.
    ///
    /// A transaction with exactly `depth` confirmations is still listed, so
    /// a caller that counts confirmations against the returned chain tip
    /// sees it reach `depth` before it leaves the listing.
    pub fn open_transactions(
        &self,
        account_uuid: String,
        depth: u32,
    ) -> Result<TxListing, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let db = self.db();
            if db.get_account(uuid)?.is_none() {
                return Err(WalletError::AccountNotFound);
            }
            let chain_tip = db.chain_height()?.map(u32::from);
            // At most depth confirmations: tip - mined + 1 <= depth.
            let floor: i64 =
                chain_tip.map_or(i64::MAX, |tip| i64::from(tip) + 1 - i64::from(depth));
            let reader = self.reader();
            let records = read_records(
                &self.params,
                &reader,
                uuid.expose_uuid().as_bytes(),
                "v.mined_height IS NULL OR v.mined_height >= :floor OR v.block_time IS NULL
                 OR t.raw IS NULL OR (v.total_spent > 0 AND v.fee_paid IS NULL)",
                "",
                &[(":floor", &floor)],
            )?;
            Ok(TxListing { chain_tip, records })
        })
    }

    /// Returns the transaction IDs among `txids` whose full transaction the
    /// wallet does not hold: unknown to it, or known only from an output
    /// seen in a compact block or an unspent-output reply. IDs are in the
    /// byte order of the wire.
    pub fn missing_transactions(&self, txids: Vec<Vec<u8>>) -> Result<Vec<Vec<u8>>, WalletError> {
        guard(|| {
            let _db = self.db();
            let reader = self.reader();
            let mut stmt = reader
                .prepare_cached("SELECT raw IS NOT NULL FROM transactions WHERE txid = :txid")
                .map_err(WalletError::database)?;
            for txid in &txids {
                check_txid(txid)?;
            }
            let mut missing = Vec::new();
            for txid in txids {
                let held: Option<bool> = stmt
                    .query_row(named_params! { ":txid": txid }, |row| row.get(0))
                    .map(Some)
                    .or_else(|e| match e {
                        rusqlite::Error::QueryReturnedNoRows => Ok(None),
                        e => Err(e),
                    })
                    .map_err(WalletError::database)?;
                if held != Some(true) {
                    missing.push(txid);
                }
            }
            Ok(missing)
        })
    }

    /// Returns the UUID of the account an address belongs to, or nothing if
    /// it belongs to none. A transparent address matches only if the wallet
    /// generated it. A unified address matches the account that can derive
    /// one of its shielded receivers, generated or not, such as an address
    /// reduced to the Orchard receiver of one of the wallet's addresses.
    pub fn owns_address(&self, address: String) -> Result<Option<String>, WalletError> {
        guard(|| {
            let decoded = Address::decode(&self.params, &address)
                .ok_or_else(|| WalletError::invalid("not a valid address for this network"))?;
            Ok(self
                .db()
                .find_account_for_address(&self.params, &decoded)
                .map_err(|e| WalletError::Database {
                    message: format!("finding the account of an address: {e:?}"),
                })?
                .map(|uuid| uuid.expose_uuid().to_string()))
        })
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use tempfile::TempDir;
    use transparent::address::TransparentAddress;

    use super::*;
    use crate::{
        Account,
        testutil::{encoded_blocks, regtest, tree_state},
        types::{TransactionStatus, Utxo},
    };

    const SEED: [u8; 32] = [8; 32];
    const TIP: u32 = 120;
    const ALPHA_TRANSPARENT: &str = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm";

    /// A wallet with one account scanned to [`TIP`] over empty blocks, and
    /// the script and address of its first transparent receive address.
    struct Fixture {
        dir: TempDir,
        w: Arc<Wallet>,
        account: Account,
        address: String,
        script: Vec<u8>,
    }

    fn fixture() -> Fixture {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("wallet.sqlite");
        let w = Wallet::open(path.to_str().unwrap().to_owned(), regtest(), None).unwrap();
        let account = w
            .create_account("test".into(), SEED.to_vec(), tree_state(99, 0), None, None)
            .unwrap();
        w.update_chain_tip(TIP).unwrap();
        w.scan_blocks(tree_state(99, 0), encoded_blocks(100..TIP + 1, 0))
            .unwrap();
        let address = w
            .transparent_receivers(account.uuid.clone())
            .unwrap()
            .into_iter()
            .find(|a| !a.change && a.index == 0)
            .unwrap()
            .address;
        let params = regtest().params().unwrap();
        let Some(Address::Transparent(TransparentAddress::PublicKeyHash(hash))) =
            Address::decode(&params, &address)
        else {
            panic!("not a P2PKH address");
        };
        let mut script = vec![0x76, 0xa9, 0x14];
        script.extend_from_slice(&hash);
        script.extend_from_slice(&[0x88, 0xac]);
        Fixture {
            dir,
            w,
            account,
            address,
            script,
        }
    }

    impl Fixture {
        fn uuid(&self) -> String {
            self.account.uuid.clone()
        }

        /// Records an unspent output of `value` paid to the receive address
        /// by the transaction `txid` at index 0, mined at `height`.
        fn put(&self, txid: &[u8], value: u64, height: u32) {
            self.w
                .put_utxo(Utxo {
                    txid: txid.to_vec(),
                    index: 0,
                    script: self.script.clone(),
                    value_zat: value,
                    height,
                })
                .unwrap();
        }

        /// Records an output of the transaction `[n; 32]`, which the wallet
        /// does not hold.
        fn receive(&self, n: u8, value: u64, height: u32) -> Vec<u8> {
            self.put(&[n; 32], value, height);
            vec![n; 32]
        }

        /// Pays 90,000 of a 100,000 output to an outside transparent
        /// address, with no change.
        fn send_all(&self) -> Vec<u8> {
            self.w
                .send(
                    self.uuid(),
                    SEED.to_vec(),
                    ALPHA_TRANSPARENT.into(),
                    90_000,
                    None,
                    10_000,
                    None,
                )
                .unwrap()
                .transactions[0]
                .txid
                .clone()
        }

        fn all(&self) -> Vec<TxRecord> {
            self.w.transactions(self.uuid(), 0, 100).unwrap().records
        }

        fn lookup(&self, txid: &[u8]) -> Option<TxRecord> {
            self.w
                .transaction(self.uuid(), txid.to_vec())
                .unwrap()
                .record
        }

        fn open(&self, depth: u32) -> Vec<Vec<u8>> {
            self.w
                .open_transactions(self.uuid(), depth)
                .unwrap()
                .records
                .into_iter()
                .map(|r| r.txid)
                .collect()
        }

        fn spendable(&self) -> u64 {
            self.w.spendable(self.uuid(), None).unwrap().transparent
        }

        fn db_conn(&self) -> rusqlite::Connection {
            rusqlite::Connection::open(self.dir.path().join("wallet.sqlite")).unwrap()
        }
    }

    /// A serialized v5 transaction paying `value` to `script`. A coinbase
    /// transaction has the one null input of a coinbase. Any other spends
    /// an output the wallet does not know, which leaves its fee unknown.
    fn paying_transaction(coinbase: bool, script: &[u8], value: u64) -> (Vec<u8>, Vec<u8>) {
        use transparent::{
            address::Script,
            bundle::{Authorized as TransparentAuthorized, Bundle, OutPoint, TxIn, TxOut},
        };
        use zcash_primitives::transaction::{Authorized, TransactionData, TxVersion};
        use zcash_protocol::{
            consensus::{BlockHeight, BranchId},
            value::Zatoshis,
        };
        use zcash_script::script;

        let prevout = if coinbase {
            OutPoint::new([0; 32], u32::MAX)
        } else {
            OutPoint::new([0x55; 32], 0)
        };
        let bundle = Bundle::<TransparentAuthorized> {
            // A coinbase input's script starts with the block height.
            vin: vec![TxIn::from_parts(
                prevout,
                Script(script::Code(vec![0x01, 110])),
                u32::MAX,
            )],
            vout: vec![TxOut::new(
                Zatoshis::from_u64(value).unwrap(),
                Script(script::Code(script.to_vec())),
            )],
            authorization: TransparentAuthorized,
        };
        assert_eq!(bundle.is_coinbase(), coinbase);
        let tx = TransactionData::<Authorized>::from_parts(
            TxVersion::V5,
            BranchId::Nu5,
            0,
            BlockHeight::from_u32(if coinbase { 0 } else { 200 }),
            Some(bundle),
            None,
            None,
            None,
        )
        .freeze()
        .unwrap();
        let mut bytes = vec![];
        tx.write(&mut bytes).unwrap();
        (bytes, tx.txid().as_ref().to_vec())
    }

    #[test]
    fn records_of_a_receive_and_a_send() {
        let f = fixture();
        let received = f.receive(1, 100_000, 110);
        let sent_id = f.send_all();

        let all = f.all();
        assert_eq!(all.len(), 2);
        let receive = &all[0];
        assert_eq!(receive.txid, received);
        assert_eq!(
            (receive.mined_height, receive.created_by_wallet, receive.fee),
            (Some(110), false, None)
        );
        // The fixture's block at 110 has the time 1,700,000,110.
        assert_eq!(receive.block_time, Some(1_700_000_110));
        // Known from an unspent-output reply only.
        assert_eq!((receive.expiry_height, receive.raw_present), (None, false));
        assert_eq!(
            (
                receive.balance_delta,
                receive.total_spent,
                receive.total_received
            ),
            (100_000, 0, 100_000)
        );
        assert_eq!(
            receive.outputs,
            vec![TxOutput {
                pool: OutputPool::Transparent,
                output_index: 0,
                value: 100_000,
                address: Some(f.address.clone()),
                to_wallet: true,
                is_change: false,
                memo: None,
            }]
        );

        let send = &all[1];
        assert_eq!(send.txid, sent_id);
        assert!(send.seq > receive.seq);
        assert_eq!(
            (send.mined_height, send.created_by_wallet, send.fee),
            (None, true, Some(10_000))
        );
        assert_eq!(send.expiry_height, Some(TIP + 1 + 120));
        assert!(send.raw_present && !send.expired_unmined);
        assert_eq!(
            (send.balance_delta, send.total_spent, send.total_received),
            (-100_000, 100_000, 0)
        );
        assert_eq!(send.outputs.len(), 1);
        let out = &send.outputs[0];
        assert_eq!(
            (
                out.pool,
                out.value,
                out.address.as_deref(),
                out.to_wallet,
                out.is_change
            ),
            (
                OutputPool::Transparent,
                90_000,
                Some(ALPHA_TRANSPARENT),
                false,
                false
            )
        );

        // Every read carries the chain tip it was made against.
        let lookup = f.w.transaction(f.uuid(), sent_id).unwrap();
        assert_eq!(
            (lookup.chain_tip, lookup.record.as_ref()),
            (Some(TIP), Some(send))
        );
        assert_eq!(
            f.w.transactions(f.uuid(), 0, 1).unwrap().chain_tip,
            Some(TIP)
        );
        assert_eq!(
            f.w.open_transactions(f.uuid(), 1).unwrap().chain_tip,
            Some(TIP)
        );
        assert_eq!(f.lookup(&[7; 32]), None);
        assert!(matches!(
            f.w.transaction(f.uuid(), vec![7; 31]),
            Err(WalletError::InvalidArgument { .. })
        ));
        assert!(matches!(
            f.w.transactions(uuid::Uuid::new_v4().to_string(), 0, 10),
            Err(WalletError::AccountNotFound)
        ));
    }

    /// seq is the transactions table's id_tx: the order of discovery, not of
    /// height. The listing pages by it.
    #[test]
    fn seq_is_the_order_of_discovery() {
        let f = fixture();
        f.receive(1, 10_000, 115);
        f.receive(2, 20_000, 116);
        // Found last, mined first.
        f.receive(3, 30_000, 105);

        let ids: Vec<(i64, Vec<u8>)> = f
            .db_conn()
            .prepare("SELECT id_tx, txid FROM transactions ORDER BY id_tx")
            .unwrap()
            .query_map([], |row| Ok((row.get(0)?, row.get(1)?)))
            .unwrap()
            .collect::<Result<_, _>>()
            .unwrap();
        let all = f.all();
        assert_eq!(
            all.iter()
                .map(|r| (r.seq as i64, r.txid.clone()))
                .collect::<Vec<_>>(),
            ids
        );
        assert_eq!(
            all.iter().map(|r| r.txid[0]).collect::<Vec<_>>(),
            vec![1, 2, 3]
        );

        // Paging and the watermark.
        let page = |after| f.w.transactions(f.uuid(), after, 2).unwrap().records;
        let first = page(0);
        assert_eq!(first.len(), 2);
        let rest = page(first[1].seq);
        assert_eq!(rest.iter().map(|r| r.txid[0]).collect::<Vec<_>>(), vec![3]);
        assert!(page(rest[0].seq).is_empty());
        f.receive(4, 40_000, 100);
        let since = page(rest[0].seq);
        assert_eq!(since.iter().map(|r| r.txid[0]).collect::<Vec<_>>(), vec![4]);
    }

    #[test]
    fn open_transactions() {
        let f = fixture();
        f.receive(1, 100_000, 110);
        let sent_id = f.send_all();
        // The receive lacks its raw transaction; the send is unmined.
        assert_eq!(f.open(10), vec![vec![1; 32], sent_id.clone()]);

        // The send is mined at 115, so at tip 120 it has 6 confirmations.
        // It is open while that is at most `depth`.
        f.w.set_transaction_status(sent_id.clone(), TransactionStatus::Mined { height: 115 })
            .unwrap();
        assert_eq!(f.open(6), vec![vec![1; 32], sent_id]);
        assert_eq!(f.open(5), vec![vec![1; 32]]);
    }

    /// A transaction mined in a block that has not been scanned has no block
    /// time yet, and is open until it has one.
    #[test]
    fn a_transaction_in_an_unscanned_block_is_open() {
        let f = fixture();
        f.w.update_chain_tip(140).unwrap();
        let (regular, regular_id) = paying_transaction(false, &f.script, 100_000);
        f.w.decrypt_and_store_transaction(regular, None, Some(125))
            .unwrap();
        f.put(&regular_id, 100_000, 125);
        let record = f.lookup(&regular_id).unwrap();
        assert_eq!((record.mined_height, record.block_time), (Some(125), None));
        assert!(record.raw_present);
        // 16 confirmations, raw present, a receive: open only for the
        // missing block time.
        assert_eq!(f.open(5), vec![regular_id.clone()]);
        f.w.scan_blocks(tree_state(TIP, 0), encoded_blocks(TIP + 1..141, 0))
            .unwrap();
        assert_eq!(
            f.lookup(&regular_id).unwrap().block_time,
            Some(1_700_000_125)
        );
        assert!(f.open(5).is_empty());
    }

    /// A rewind unmines a transaction whose expiry is unknown: the view then
    /// has no expired flag for it, and every listing still reads.
    #[test]
    fn listings_read_after_a_rewind() {
        let f = fixture();
        let received = f.receive(1, 100_000, 115);
        f.w.rewind_to_height(112).unwrap();
        let record = f.lookup(&received).unwrap();
        assert_eq!(
            (
                record.mined_height,
                record.expiry_height,
                record.expired_unmined
            ),
            (None, None, false)
        );
        assert_eq!(f.all().len(), 1);
        assert_eq!(f.open(10), vec![received]);
    }

    #[test]
    fn missing_transactions() {
        let f = fixture();
        let received = f.receive(1, 100_000, 110);
        let sent_id = f.send_all();
        let unknown = vec![9; 32];
        assert_eq!(
            f.w.missing_transactions(vec![received.clone(), sent_id, unknown.clone()])
                .unwrap(),
            vec![received, unknown]
        );
        assert!(matches!(
            f.w.missing_transactions(vec![vec![1; 31]]),
            Err(WalletError::InvalidArgument { .. })
        ));
    }

    #[test]
    fn owns_address() {
        let f = fixture();
        assert_eq!(f.w.owns_address(f.address.clone()).unwrap(), Some(f.uuid()));
        let unified = f.w.current_address(f.uuid()).unwrap();
        assert_eq!(f.w.owns_address(unified).unwrap(), Some(f.uuid()));
        assert_eq!(f.w.owns_address(ALPHA_TRANSPARENT.into()).unwrap(), None);
        assert!(matches!(
            f.w.owns_address("nonsense".into()),
            Err(WalletError::InvalidArgument { .. })
        ));
    }

    /// A shielded output the wallet received is reported at the receiver
    /// that was paid: the account's full unified address, Sapling receiver
    /// included, comes back pruned to its Orchard receiver, the same string
    /// parse_address gives, and the wallet owns it. A Sapling output has no
    /// address, and a transparent one keeps its own.
    #[test]
    fn a_received_shielded_output_is_reported_at_its_orchard_receiver() {
        use zcash_keys::keys::{UnifiedAddressRequest, UnifiedFullViewingKey};
        let f = fixture();
        let params = regtest().params().unwrap();
        let ufvk =
            UnifiedFullViewingKey::decode(&params, f.account.ufvk.as_ref().unwrap()).unwrap();
        let (full, _) = ufvk
            .default_address(UnifiedAddressRequest::AllAvailableKeys)
            .unwrap();
        assert!(
            full.sapling().is_some(),
            "the account has no Sapling receiver to prune"
        );
        let full = full.encode(&params);
        let reported = received_address(&params, OutputPool::Ironwood, Some(full.clone()))
            .unwrap()
            .unwrap();
        let parsed = crate::address::parse_address(regtest(), full.clone()).unwrap();
        assert_eq!(Some(reported.clone()), parsed.receivers[0].address);
        assert_eq!(f.w.owns_address(reported.clone()).unwrap(), Some(f.uuid()));
        let Some(Address::Unified(pruned)) = Address::decode(&params, &reported) else {
            panic!("not a unified address");
        };
        assert!(
            pruned.orchard().is_some()
                && pruned.sapling().is_none()
                && pruned.transparent().is_none()
        );
        assert_eq!(
            received_address(&params, OutputPool::Orchard, Some(full.clone())).unwrap(),
            Some(reported)
        );
        assert_eq!(
            received_address(&params, OutputPool::Sapling, Some(full)).unwrap(),
            None
        );
        assert_eq!(
            received_address(&params, OutputPool::Transparent, Some(f.address.clone())).unwrap(),
            Some(f.address.clone())
        );
        assert_eq!(
            received_address(&params, OutputPool::Ironwood, None).unwrap(),
            None
        );
    }

    /// An output to the wallet keeps the address its sent row names only in a
    /// transaction this wallet created. A restored wallet, or another device
    /// on the same seed, recovers the payment with the viewing key, and the
    /// library records its sent row under the full address of the wallet's
    /// own address row, Sapling receiver included; the history reports the
    /// Orchard-only receiver instead. Recovered change has no address row
    /// (zcash_client_sqlite `ensure_address` gives an internal-scope output
    /// none) and stays without an address.
    ///
    /// The fixtures cannot produce a decryptable shielded note, so the rows
    /// the library would store for such a payment are inserted by hand: a
    /// received Ironwood note at the account's default address row, which
    /// holds the full address, a change note without an address row, and
    /// the sent rows of both.
    #[test]
    fn an_output_to_the_wallet_keeps_its_named_address_only_if_created_here() {
        use zcash_keys::keys::{UnifiedAddressRequest, UnifiedFullViewingKey};
        let f = fixture();
        let params = regtest().params().unwrap();
        let ufvk =
            UnifiedFullViewingKey::decode(&params, f.account.ufvk.as_ref().unwrap()).unwrap();
        let (full, _) = ufvk
            .default_address(UnifiedAddressRequest::AllAvailableKeys)
            .unwrap();
        assert!(full.sapling().is_some());
        let full = full.encode(&params);
        let named = f.w.current_address(f.uuid()).unwrap();
        let orchard = received_address(&params, OutputPool::Ironwood, Some(full.clone()))
            .unwrap()
            .unwrap();

        let db = f.db_conn();
        let account_id: i64 = db
            .query_row("SELECT id FROM accounts", [], |row| row.get(0))
            .unwrap();
        // The address row of the default address. ensure_address passes
        // force_update_address = false, so upsert_address keeps the address a
        // row holds and writes the ALLOW_ALL address only when it creates
        // the row. The row of the default address already holds the full
        // address, Sapling included, which is not the string the wallet
        // hands out.
        let (address_id, stored): (i64, String) = db
            .query_row(
                "SELECT id, address FROM addresses WHERE account_id = ?1 AND key_scope = 0
                 ORDER BY diversifier_index_be LIMIT 1",
                [account_id],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .unwrap();
        assert_eq!(stored, full);
        assert_ne!(stored, named);
        let insert = |txid: &[u8], created: Option<&str>, sent_to: &str| {
            db.execute(
                "INSERT INTO transactions (txid, created, mined_height, min_observed_height, fee)
                 VALUES (?1, ?2, 110, 110, 10000)",
                rusqlite::params![txid, created],
            )
            .unwrap();
            let tx = db.last_insert_rowid();
            for (index, value, change, scope, address) in [
                (0, 6_000_000i64, 0, 0, Some(address_id)),
                (1, 1_000i64, 1, 1, None),
            ] {
                db.execute(
                    "INSERT INTO ironwood_received_notes (transaction_id, action_index, account_id,
                       diversifier, value, rho, rseed, nf, is_change, recipient_key_scope,
                       address_id, note_version)
                     VALUES (?1, ?2, ?3, zeroblob(11), ?4, randomblob(32), randomblob(32),
                       randomblob(32), ?5, ?6, ?7, 3)",
                    rusqlite::params![tx, index, account_id, value, change, scope, address],
                )
                .unwrap();
            }
            // The sent rows of the payment and of the change, which the
            // library stores both when it creates a transaction and when it
            // recovers an output; change names no address.
            db.execute(
                "INSERT INTO sent_notes (transaction_id, output_pool, output_index,
                   from_account_id, to_address, to_account_id, value)
                 VALUES (?1, 4, 0, ?2, ?3, ?2, 6000000), (?1, 4, 1, ?2, NULL, ?2, 1000)",
                rusqlite::params![tx, account_id, sent_to],
            )
            .unwrap();
        };
        let created_here = [0xc1u8; 32];
        let restored = [0xc2u8; 32];
        insert(&created_here, Some("2026-11-14T00:00:00Z"), &named);
        insert(&restored, None, &full);
        // The rule follows the creation, not the shape of the address: a
        // transaction not created here whose sent row names the address
        // without Sapling is still pruned, and one created here that named
        // the full address keeps it (a user can pay their own address in that
        // form when another wallet displays it).
        let restored_named = [0xc3u8; 32];
        let created_full = [0xc4u8; 32];
        insert(&restored_named, None, &named);
        insert(&created_full, Some("2026-11-14T00:00:00Z"), &full);

        let outputs = |txid: &[u8]| {
            let r = f.lookup(txid).unwrap();
            let pay = r
                .outputs
                .iter()
                .find(|o| o.output_index == 0)
                .unwrap()
                .clone();
            let change = r
                .outputs
                .iter()
                .find(|o| o.output_index == 1)
                .unwrap()
                .clone();
            (r.created_by_wallet, pay, change)
        };
        let (created, pay, change) = outputs(&created_here);
        assert!(created);
        assert!(pay.to_wallet);
        assert_eq!(pay.address.as_deref(), Some(named.as_str()));
        assert_eq!(change.address, None);
        let (created, pay, change) = outputs(&restored);
        assert!(!created);
        assert!(pay.to_wallet);
        assert_eq!(pay.address.as_deref(), Some(orchard.as_str()));
        assert_eq!(change.address, None);
        let (created, pay, change) = outputs(&restored_named);
        assert!(!created);
        assert_eq!(pay.address.as_deref(), Some(orchard.as_str()));
        assert_eq!(change.address, None);
        let (created, pay, change) = outputs(&created_full);
        assert!(created);
        assert_eq!(pay.address.as_deref(), Some(full.as_str()));
        assert_eq!(change.address, None);
    }

    /// A coinbase output is never selectable while the wallet holds its
    /// transaction, in either order of storing the transaction and the
    /// output. Recorded alone, it is taken for a regular output: the gap the
    /// sync loop closes by fetching funding transactions first.
    #[test]
    fn a_coinbase_output_in_either_order() {
        // The transaction first.
        let f = fixture();
        let (coinbase, coinbase_id) = paying_transaction(true, &f.script, 50_000);
        f.w.decrypt_and_store_transaction(coinbase.clone(), None, Some(110))
            .unwrap();
        f.put(&coinbase_id, 50_000, 110);
        assert_eq!(f.spendable(), 0);
        assert!(matches!(
            f.w.estimate_transfer(f.uuid(), ALPHA_TRANSPARENT.into(), 10_000, None, None),
            Err(WalletError::InsufficientFunds { available: 0, .. })
        ));
        // A regular payment stored the same way is spendable.
        let (regular, regular_id) = paying_transaction(false, &f.script, 30_000);
        f.w.decrypt_and_store_transaction(regular, None, Some(110))
            .unwrap();
        f.put(&regular_id, 30_000, 110);
        assert_eq!(f.spendable(), 30_000);

        // The output first.
        let f = fixture();
        f.put(&coinbase_id, 50_000, 110);
        // The gap: without its transaction, the output counts as regular.
        assert_eq!(f.spendable(), 50_000);
        f.w.decrypt_and_store_transaction(coinbase, None, Some(110))
            .unwrap();
        assert_eq!(f.spendable(), 0);
    }

    /// A rewind below a coinbase output's height clears the coinbase mark of
    /// its transaction and keeps the raw bytes. The output stays
    /// unselectable after the rewind, and when an unspent-output reply
    /// records it again before its block is scanned again.
    #[test]
    fn a_coinbase_output_across_a_rewind() {
        let f = fixture();
        let (coinbase, coinbase_id) = paying_transaction(true, &f.script, 50_000);
        f.w.decrypt_and_store_transaction(coinbase, None, Some(110))
            .unwrap();
        f.put(&coinbase_id, 50_000, 110);
        assert_eq!(f.spendable(), 0);

        // 108 is the checkpoint of the anchor grid below 110.
        assert_eq!(f.w.rewind_to_height(108).unwrap(), 108);
        assert_eq!(f.spendable(), 0);
        // The wallet still holds the transaction, so the sync loop asks for
        // nothing and records the output again.
        assert!(
            f.w.missing_transactions(vec![coinbase_id.clone()])
                .unwrap()
                .is_empty()
        );
        f.put(&coinbase_id, 50_000, 110);
        assert_eq!(f.spendable(), 0);
        assert_eq!(tx_index(&f, &coinbase_id), Some(0));
    }

    fn tx_index(f: &Fixture, txid: &[u8]) -> Option<i64> {
        f.db_conn()
            .query_row(
                "SELECT tx_index FROM transactions WHERE txid = ?1",
                [txid],
                |row| row.get(0),
            )
            .unwrap()
    }

    /// An output that the held transaction does not have, by index, script
    /// or value, is refused and not recorded.
    #[test]
    fn an_output_is_checked_against_its_transaction() {
        let f = fixture();
        let (regular, regular_id) = paying_transaction(false, &f.script, 30_000);
        f.w.decrypt_and_store_transaction(regular, None, Some(110))
            .unwrap();
        // Storing the transaction recorded its output. It is taken out
        // again, so that only put_utxo can record it.
        f.db_conn()
            .execute("DELETE FROM transparent_received_outputs", [])
            .unwrap();
        assert_eq!(f.spendable(), 0);
        let utxo = Utxo {
            txid: regular_id.clone(),
            index: 0,
            script: f.script.clone(),
            value_zat: 30_000,
            height: 110,
        };
        let mut other_script = f.script.clone();
        other_script[3] ^= 1;
        for bad in [
            Utxo {
                value_zat: 31_000,
                ..utxo.clone()
            },
            Utxo {
                index: 1,
                ..utxo.clone()
            },
            Utxo {
                script: other_script,
                ..utxo.clone()
            },
        ] {
            assert!(
                matches!(f.w.put_utxo(bad), Err(WalletError::InvalidArgument { .. })),
                "a mismatched output was accepted"
            );
            assert_eq!(f.spendable(), 0);
        }
        f.w.put_utxo(utxo).unwrap();
        assert_eq!(f.spendable(), 30_000);
    }

    /// A held transaction that cannot be parsed cannot be classified, and
    /// its output is not recorded.
    #[test]
    fn an_output_of_an_unreadable_transaction_is_not_recorded() {
        let f = fixture();
        let (coinbase, coinbase_id) = paying_transaction(true, &f.script, 50_000);
        f.w.decrypt_and_store_transaction(coinbase, None, Some(110))
            .unwrap();
        let db = f.db_conn();
        db.execute("DELETE FROM transparent_received_outputs", [])
            .unwrap();
        db.execute(
            "UPDATE transactions SET raw = x'00', tx_index = NULL WHERE txid = ?1",
            [&coinbase_id],
        )
        .unwrap();
        assert!(matches!(
            f.w.put_utxo(Utxo {
                txid: coinbase_id,
                index: 0,
                script: f.script.clone(),
                value_zat: 50_000,
                height: 110,
            }),
            Err(WalletError::Database { .. })
        ));
        assert_eq!(
            f.w.wallet_summary(Some(1))
                .unwrap()
                .unwrap()
                .account_balances[0]
                .transparent
                .total,
            0
        );
    }

    /// A rewind restores the coinbase mark of every transaction it cleared,
    /// and leaves regular transactions regular.
    #[test]
    fn a_rewind_reclassifies_every_coinbase_transaction() {
        let f = fixture();
        let mut coinbase_ids = vec![];
        let mut regular_ids = vec![];
        for i in 0..5u64 {
            let (coinbase, coinbase_id) = paying_transaction(true, &f.script, 50_000 + i);
            f.w.decrypt_and_store_transaction(coinbase, None, Some(110))
                .unwrap();
            coinbase_ids.push(coinbase_id);
            let (regular, regular_id) = paying_transaction(false, &f.script, 30_000 + i);
            f.w.decrypt_and_store_transaction(regular, None, Some(110))
                .unwrap();
            regular_ids.push(regular_id);
        }
        let regular_total = (0..5).map(|i| 30_000 + i).sum::<u64>();
        assert_eq!(f.spendable(), regular_total);
        assert_eq!(f.w.rewind_to_height(108).unwrap(), 108);
        for id in &coinbase_ids {
            assert_eq!(tx_index(&f, id), Some(0));
        }
        for id in &regular_ids {
            assert_eq!(tx_index(&f, id), None);
        }
        assert_eq!(f.spendable(), regular_total);
    }

    /// A rewind that finds a held transaction it cannot read fails, and
    /// rewinds nothing: every coinbase mark stays, a later coinbase
    /// transaction's included, and no coinbase output becomes selectable.
    /// Once the transaction reads again, the rewind succeeds.
    #[test]
    fn a_rewind_fails_on_an_unreadable_transaction() {
        let f = fixture();
        let mut coinbase_ids = vec![];
        let mut raws = vec![];
        for i in 0..2u64 {
            let (coinbase, coinbase_id) = paying_transaction(true, &f.script, 50_000 + i);
            f.w.decrypt_and_store_transaction(coinbase.clone(), None, Some(110))
                .unwrap();
            coinbase_ids.push(coinbase_id);
            raws.push(coinbase);
        }
        let (regular, _) = paying_transaction(false, &f.script, 30_000);
        f.w.decrypt_and_store_transaction(regular, None, Some(110))
            .unwrap();
        assert_eq!(f.spendable(), 30_000);
        // The first candidate in the order they are read.
        let first = coinbase_ids
            .iter()
            .min_by_key(|id| {
                f.db_conn()
                    .query_row(
                        "SELECT id_tx FROM transactions WHERE txid = ?1",
                        [id],
                        |row| row.get::<_, i64>(0),
                    )
                    .unwrap()
            })
            .unwrap()
            .clone();
        f.db_conn()
            .execute(
                "UPDATE transactions SET raw = x'00' WHERE txid = ?1",
                [&first],
            )
            .unwrap();
        assert!(matches!(
            f.w.rewind_to_height(108),
            Err(WalletError::Database { .. })
        ));
        for id in &coinbase_ids {
            assert_eq!(tx_index(&f, id), Some(0));
            let mined: Option<u32> = f
                .db_conn()
                .query_row(
                    "SELECT mined_height FROM transactions WHERE txid = ?1",
                    [id],
                    |row| row.get(0),
                )
                .unwrap();
            assert_eq!(mined, Some(110));
        }
        assert_eq!(f.spendable(), 30_000);

        let i = coinbase_ids.iter().position(|id| *id == first).unwrap();
        f.db_conn()
            .execute(
                "UPDATE transactions SET raw = ?1 WHERE txid = ?2",
                rusqlite::params![&raws[i], &first],
            )
            .unwrap();
        assert_eq!(f.w.rewind_to_height(108).unwrap(), 108);
        for id in &coinbase_ids {
            assert_eq!(tx_index(&f, id), Some(0));
        }
        assert_eq!(f.spendable(), 30_000);
    }

    /// A transaction fetched by its ID is stored only if it is that one.
    #[test]
    fn a_requested_transaction_must_be_the_one_requested() {
        let f = fixture();
        let (a, a_id) = paying_transaction(false, &f.script, 30_000);
        let (b, _) = paying_transaction(false, &f.script, 40_000);
        assert!(matches!(
            f.w.decrypt_and_store_transaction(b, Some(a_id.clone()), Some(110)),
            Err(WalletError::InvalidArgument { .. })
        ));
        assert!(matches!(
            f.w.decrypt_and_store_transaction(a.clone(), Some(vec![1, 2]), Some(110)),
            Err(WalletError::InvalidArgument { .. })
        ));
        assert_eq!(f.spendable(), 0);
        assert_eq!(
            f.w.missing_transactions(vec![a_id.clone()]).unwrap(),
            vec![a_id.clone()]
        );
        assert_eq!(
            f.w.decrypt_and_store_transaction(a, Some(a_id.clone()), Some(110))
                .unwrap(),
            a_id
        );
        assert!(f.w.missing_transactions(vec![a_id]).unwrap().is_empty());
        assert_eq!(f.spendable(), 30_000);
    }

    /// The record of a receive whose funding transaction was stored, and
    /// which rows stay open for a fee that is not known.
    #[test]
    fn open_rows_and_unknown_fees() {
        let f = fixture();
        let (regular, regular_id) = paying_transaction(false, &f.script, 100_000);
        f.w.decrypt_and_store_transaction(regular, None, Some(110))
            .unwrap();
        f.put(&regular_id, 100_000, 110);
        let receive = f.lookup(&regular_id).unwrap();
        // Stored with its raw bytes and expiry. The fee stays unknown, as
        // the input is someone else's.
        assert_eq!((receive.expiry_height, receive.fee), (Some(200), None));
        assert_eq!(receive.total_spent, 0);
        // 11 confirmations, with its raw bytes: not open at depth 10,
        // although its fee is unknown.
        assert!(f.open(10).is_empty());
        assert_eq!(f.open(11), vec![regular_id.clone()]);

        // A send of the account's own funds whose fee is unknown stays open.
        let sent_id = f.send_all();
        f.w.set_transaction_status(sent_id.clone(), TransactionStatus::Mined { height: 111 })
            .unwrap();
        assert!(f.open(5).is_empty());
        f.db_conn()
            .execute(
                "UPDATE transactions SET fee = NULL WHERE txid = ?1",
                [&sent_id],
            )
            .unwrap();
        assert_eq!(f.open(5), vec![sent_id]);
    }
}
