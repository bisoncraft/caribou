// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Plain records that cross the FFI boundary.

use zcash_client_backend::data_api::{self, scanning};

/// An account in the wallet.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct Account {
    /// The wallet's identifier for the account, as a hyphenated UUID.
    pub uuid: String,
    pub name: Option<String>,
    /// The height of the first block that can hold funds of this account.
    pub birthday_height: u32,
    /// The account's unified full viewing key, when the wallet holds one.
    pub ufvk: Option<String>,
    /// The ZIP 32 account index, for accounts derived from a seed.
    pub hd_account_index: Option<u32>,
    /// The ZIP 32 fingerprint of the seed the account was derived from.
    pub seed_fingerprint: Option<Vec<u8>>,
    /// An application-defined label for where the account's keys are held.
    pub key_source: Option<String>,
}

/// A balance within one value pool of an account, in zatoshis.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, uniffi::Record)]
pub struct PoolBalance {
    /// Value that can be spent now.
    pub spendable: u64,
    /// Change from the wallet's own transactions that is not yet confirmed
    /// deeply enough to spend.
    pub change_pending_confirmation: u64,
    /// Received value that is not yet spendable, either for lack of
    /// confirmations or because the wallet has not scanned far enough to
    /// build a witness for it.
    pub value_pending_spendability: u64,
    /// Value held in outputs that cost more in fees to spend than they hold.
    /// Not part of `total`.
    pub uneconomic: u64,
    /// Value locked by an in-progress operation.
    pub locked: u64,
    /// The sum of the spendable, pending and locked values.
    pub total: u64,
}

impl From<&data_api::Balance> for PoolBalance {
    fn from(b: &data_api::Balance) -> Self {
        PoolBalance {
            spendable: b.spendable_value().into_u64(),
            change_pending_confirmation: b.change_pending_confirmation().into_u64(),
            value_pending_spendability: b.value_pending_spendability().into_u64(),
            uneconomic: b.uneconomic_value().into_u64(),
            locked: b.locked_value().into_u64(),
            total: b.total().into_u64(),
        }
    }
}

/// The balances of one account, per value pool.
///
/// There are no Sapling or Orchard balances. An account's addresses have no
/// Sapling receiver, its birthday is at or after NU6.3, and from NU6.3 on a
/// payment to an Orchard receiver lands in the Ironwood pool.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct AccountBalance {
    pub account_uuid: String,
    pub ironwood: PoolBalance,
    pub transparent: PoolBalance,
    /// The sum of the pool totals.
    pub total: u64,
}

/// A count of items done out of items to do. The denominator is zero when
/// there is nothing to do, so a caller converting to a fraction must check it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Record)]
pub struct Ratio {
    pub numerator: u64,
    pub denominator: u64,
}

impl From<data_api::Ratio<u64>> for Ratio {
    fn from(r: data_api::Ratio<u64>) -> Self {
        Ratio {
            numerator: *r.numerator(),
            denominator: *r.denominator(),
        }
    }
}

/// Balances and sync state of the wallet, as of the chain tip it knows.
///
/// Balances can overstate what is spendable until the wallet is fully
/// scanned, because the spend of a note may not have been seen yet.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct WalletSummary {
    pub account_balances: Vec<AccountBalance>,
    /// The chain tip the wallet was last told about.
    pub chain_tip_height: u32,
    /// The height below which every block has been scanned.
    pub fully_scanned_height: u32,
    /// Progress in scanning from the recovery height (or the birthday, when
    /// the wallet is not being recovered) to the chain tip.
    pub scan_progress: Ratio,
    /// Progress in scanning from the birthday to the recovery height. Only
    /// set while a wallet restored from a seed is catching up.
    pub recovery_progress: Option<Ratio>,
    /// The Ironwood subtree index at which the next request for subtree
    /// roots should start. It is the index of the last completed subtree the
    /// wallet holds a root for, so that the request overlaps what is stored.
    pub next_subtree_index: u64,
}

/// How urgently a block range should be scanned. Higher variants come first.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, uniffi::Enum)]
pub enum ScanPriority {
    /// The range needs no scanning.
    Ignored,
    /// The range has been scanned.
    Scanned,
    /// Scanning the range advances the fully scanned height.
    Historic,
    /// The range is next to a height at which the wallet was opened.
    OpenAdjacent,
    /// The range completes a note commitment subtree that holds a wallet
    /// note, making that note spendable.
    FoundNote,
    /// The range completes the newest note commitment subtree.
    ChainTip,
    /// The range was scanned before and must be checked against the current
    /// chain. A range with this priority is always first.
    Verify,
}

impl From<scanning::ScanPriority> for ScanPriority {
    fn from(p: scanning::ScanPriority) -> Self {
        match p {
            scanning::ScanPriority::Ignored => ScanPriority::Ignored,
            scanning::ScanPriority::Scanned => ScanPriority::Scanned,
            scanning::ScanPriority::Historic => ScanPriority::Historic,
            scanning::ScanPriority::OpenAdjacent => ScanPriority::OpenAdjacent,
            scanning::ScanPriority::FoundNote => ScanPriority::FoundNote,
            scanning::ScanPriority::ChainTip => ScanPriority::ChainTip,
            scanning::ScanPriority::Verify => ScanPriority::Verify,
        }
    }
}

/// A range of block heights to scan, from `start` up to but excluding `end`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Record)]
pub struct ScanRange {
    pub start: u32,
    pub end: u32,
    pub priority: ScanPriority,
}

impl From<&scanning::ScanRange> for ScanRange {
    fn from(r: &scanning::ScanRange) -> Self {
        ScanRange {
            start: r.block_range().start.into(),
            end: r.block_range().end.into(),
            priority: r.priority().into(),
        }
    }
}

/// The heights a scan of a batch of blocks covered.
///
/// The note counts that librustzcash reports for a scan are left out. Version
/// 0.25 counts Sapling and Orchard notes only, so both counts are always zero
/// for a wallet that receives into the Ironwood pool.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Record)]
pub struct ScanSummary {
    /// The first height scanned.
    pub start_height: u32,
    /// One past the last height scanned.
    pub end_height: u32,
}

/// The root of one completed subtree of a note commitment tree.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct SubtreeRoot {
    /// The 32-byte root hash.
    pub root_hash: Vec<u8>,
    /// The height of the block that completed the subtree.
    pub completing_block_height: u32,
}

/// An unspent transparent output paying an address of the wallet.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct Utxo {
    /// The 32-byte transaction ID in internal (little-endian) byte order, as
    /// lightwalletd reports it.
    pub txid: Vec<u8>,
    pub index: u32,
    /// The output's script.
    pub script: Vec<u8>,
    pub value_zat: u64,
    /// The height of the block that mined the output.
    pub height: u32,
}

/// The receivers to include in a newly generated unified address.
///
/// The shielded receiver is always an Orchard receiver, which from NU6.3 on
/// receives into the Ironwood pool. No address has a Sapling receiver.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum ReceiverSet {
    /// An Ironwood and a transparent receiver. Transparent receivers are
    /// subject to the wallet's gap limit.
    All,
    /// An Ironwood receiver only.
    Shielded,
}

/// Which transactions a request for an address's transactions covers.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum TransactionStatusFilter {
    Mined,
    Mempool,
    All,
}

impl From<TransactionStatusFilter> for data_api::TransactionStatusFilter {
    fn from(f: TransactionStatusFilter) -> Self {
        match f {
            TransactionStatusFilter::Mined => data_api::TransactionStatusFilter::Mined,
            TransactionStatusFilter::Mempool => data_api::TransactionStatusFilter::Mempool,
            TransactionStatusFilter::All => data_api::TransactionStatusFilter::All,
        }
    }
}

impl From<&data_api::TransactionStatusFilter> for TransactionStatusFilter {
    fn from(f: &data_api::TransactionStatusFilter) -> Self {
        match f {
            data_api::TransactionStatusFilter::Mined => TransactionStatusFilter::Mined,
            data_api::TransactionStatusFilter::Mempool => TransactionStatusFilter::Mempool,
            data_api::TransactionStatusFilter::All => TransactionStatusFilter::All,
        }
    }
}

/// Which outputs a request for an address's transactions covers.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum OutputStatusFilter {
    Unspent,
    All,
}

impl From<OutputStatusFilter> for data_api::OutputStatusFilter {
    fn from(f: OutputStatusFilter) -> Self {
        match f {
            OutputStatusFilter::Unspent => data_api::OutputStatusFilter::Unspent,
            OutputStatusFilter::All => data_api::OutputStatusFilter::All,
        }
    }
}

impl From<&data_api::OutputStatusFilter> for OutputStatusFilter {
    fn from(f: &data_api::OutputStatusFilter) -> Self {
        match f {
            data_api::OutputStatusFilter::Unspent => OutputStatusFilter::Unspent,
            data_api::OutputStatusFilter::All => OutputStatusFilter::All,
        }
    }
}

/// Transaction data that the wallet needs from the chain and cannot get from
/// compact blocks. The caller fetches it from lightwalletd and reports back
/// through `Wallet::set_transaction_status` or
/// `Wallet::decrypt_and_store_transaction`.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum TransactionDataRequest {
    /// Whether, and where, the transaction has been mined. Answered with
    /// `Wallet::set_transaction_status`.
    GetStatus { txid: Vec<u8> },
    /// The full transaction, for its memos and transparent parts. Answered
    /// with `Wallet::decrypt_and_store_transaction`, or with
    /// `Wallet::set_transaction_status` when the transaction cannot be found.
    Enhancement { txid: Vec<u8> },
    /// Every transaction that involves a transparent address of the wallet
    /// within a block range. Each one found is passed to
    /// `Wallet::decrypt_and_store_transaction`, and the request is then
    /// closed with `Wallet::notify_address_checked`.
    TransactionsInvolvingAddress {
        address: String,
        block_range_start: u32,
        /// One past the last height of the range, or unset for no upper
        /// bound.
        block_range_end: Option<u32>,
        /// The earliest time, in seconds since the Unix epoch, at which the
        /// request should be made. Unset means now.
        request_at: Option<u64>,
        tx_status_filter: TransactionStatusFilter,
        output_status_filter: OutputStatusFilter,
    },
}

/// What the chain knows about a transaction.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum TransactionStatus {
    /// The server has no record of the transaction.
    TxidNotRecognized,
    /// The transaction is known but is not in a block of the main chain. It
    /// may be in the mempool.
    NotInMainChain,
    /// The transaction was mined in the main chain at this height.
    Mined { height: u32 },
}

#[cfg(test)]
mod tests {
    use super::*;
    use zcash_protocol::consensus::BlockHeight;

    #[test]
    fn scan_priority_order_matches_librustzcash() {
        use scanning::ScanPriority as P;
        let ordered = [
            P::Ignored,
            P::Scanned,
            P::Historic,
            P::OpenAdjacent,
            P::FoundNote,
            P::ChainTip,
            P::Verify,
        ];
        for pair in ordered.windows(2) {
            assert!(pair[0] < pair[1]);
            assert!(ScanPriority::from(pair[0]) < ScanPriority::from(pair[1]));
        }
    }

    #[test]
    fn scan_range_conversion() {
        let r = scanning::ScanRange::from_parts(
            BlockHeight::from_u32(5)..BlockHeight::from_u32(9),
            scanning::ScanPriority::Verify,
        );
        assert_eq!(
            ScanRange::from(&r),
            ScanRange {
                start: 5,
                end: 9,
                priority: ScanPriority::Verify
            }
        );
    }

    #[test]
    fn ratio_conversion() {
        assert_eq!(
            Ratio::from(data_api::Ratio::new(3, 4)),
            Ratio {
                numerator: 3,
                denominator: 4
            }
        );
    }

    #[test]
    fn zero_balance_converts_to_default() {
        assert_eq!(
            PoolBalance::from(&data_api::Balance::ZERO),
            PoolBalance::default()
        );
    }

    #[test]
    fn filters_round_trip() {
        use data_api::{OutputStatusFilter as O, TransactionStatusFilter as T};
        for f in [
            TransactionStatusFilter::Mined,
            TransactionStatusFilter::Mempool,
            TransactionStatusFilter::All,
        ] {
            assert_eq!(TransactionStatusFilter::from(&T::from(f)), f);
        }
        for f in [OutputStatusFilter::Unspent, OutputStatusFilter::All] {
            assert_eq!(OutputStatusFilter::from(&O::from(f)), f);
        }
    }

    #[test]
    fn filters_convert() {
        use data_api::{OutputStatusFilter as O, TransactionStatusFilter as T};
        assert_eq!(
            TransactionStatusFilter::from(&T::Mined),
            TransactionStatusFilter::Mined
        );
        assert_eq!(
            TransactionStatusFilter::from(&T::Mempool),
            TransactionStatusFilter::Mempool
        );
        assert_eq!(
            TransactionStatusFilter::from(&T::All),
            TransactionStatusFilter::All
        );
        assert_eq!(
            OutputStatusFilter::from(&O::Unspent),
            OutputStatusFilter::Unspent
        );
        assert_eq!(OutputStatusFilter::from(&O::All), OutputStatusFilter::All);
    }
}
