// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

use std::num::NonZeroU32;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, UNIX_EPOCH};

use prost::Message;
use rand::{rand_core::UnwrapErr, rngs::SysRng};
use secrecy::SecretVec;
use transparent::{
    address::Script,
    bundle::{OutPoint, TxOut},
};
use uuid::Uuid;
use zcash_address::unified::Typecode;
use zcash_client_backend::{
    data_api::{
        self, Account as _, AccountBirthday, SeedRelevance, WalletCommitmentTrees, WalletRead,
        WalletWrite,
        chain::{CommitmentTreeRoot, scan_cached_blocks},
        wallet::{ConfirmationsPolicy, decrypt_and_store_transaction},
    },
    proto::service::TreeState,
    wallet::WalletTransparentOutput,
};
use zcash_client_sqlite::{AccountUuid, WalletDb, util::SystemClock, wallet::init::init_wallet_db};
use zcash_keys::{
    address::Address,
    encoding::AddressCodec,
    keys::{ReceiverRequirement, UnifiedAddressRequest, UnifiedFullViewingKey},
};
use zcash_primitives::{
    merkle_tree::HashSer,
    transaction::{Transaction, TxId},
};
use zcash_protocol::{
    consensus::{BlockHeight, BranchId, NetworkType, Parameters},
    value::Zatoshis,
    zip318::AnchorBucketInterval,
};
use zcash_script::script;

use crate::{
    blocks::MemoryBlockSource,
    error::{WalletError, guard, migration_error},
    network::{Network, Params},
    types::{
        Account, AccountBalance, ReceiverSet, ScanRange, ScanSummary, SubtreeRoot,
        TransactionDataRequest, TransactionStatus, Utxo, WalletSummary,
    },
};

type Db = WalletDb<rusqlite::Connection, Params, SystemClock, UnwrapErr<SysRng>>;

/// The interval of the note commitment tree checkpoints that a wallet on a
/// test network keeps as durable anchors.
///
/// The interval counts blocks at the block spacing before NU7. From NU7 on
/// blocks come three times as fast and the checkpoints are three times the
/// interval apart, which keeps them the same time apart.
///
/// Mainnet wallets keep the ZIP 318 interval of 144. A pool migration
/// anchors its transfers to that grid, so on a test network the mainnet
/// spacing would make every transfer wait hours for a usable anchor. The
/// anonymity argument for a shared grid does not apply off mainnet. The value
/// matches the Zcash Android wallet SDK.
///
/// Scanning creates the checkpoint at every grid height it passes, wherever
/// the scanned batches begin and end.
const TEST_ANCHOR_RETENTION_INTERVAL: NonZeroU32 = NonZeroU32::new(12).unwrap();

fn anchor_retention_interval(network: NetworkType) -> AnchorBucketInterval {
    match network {
        NetworkType::Main => AnchorBucketInterval::ZIP_318,
        NetworkType::Test | NetworkType::Regtest => {
            AnchorBucketInterval::custom(TEST_ANCHOR_RETENTION_INTERVAL)
        }
    }
}

/// The receivers of the unified addresses a wallet hands out.
///
/// Ironwood has no receiver type of its own. A payment to the Orchard
/// receiver of a unified address lands in the Ironwood pool from NU6.3 on.
/// No address has a Sapling receiver, so nothing can be paid into the
/// Sapling pool.
const IRONWOOD_AND_TRANSPARENT: UnifiedAddressRequest = UnifiedAddressRequest::unsafe_custom(
    ReceiverRequirement::Require,
    ReceiverRequirement::Omit,
    ReceiverRequirement::Require,
);
const IRONWOOD_ONLY: UnifiedAddressRequest = UnifiedAddressRequest::ORCHARD;

/// A Zcash light wallet database for the Ironwood shielded pool.
///
/// The wallet holds funds in the Ironwood pool and in transparent outputs.
/// It does not support the earlier shielded pools: an account cannot begin
/// before NU6.3, its addresses have no Sapling receiver, and from NU6.3 on
/// the Orchard pool accepts no payments. The viewing keys of an account
/// still include a Sapling key, because librustzcash derives every key of an
/// account, but no address exposes it.
///
/// A `Wallet` holds viewing keys, notes, transparent outputs, note commitment
/// trees and scan progress. It holds no spending keys and no seed: account
/// creation derives the viewing keys from the seed and discards it.
///
/// The wallet learns about the chain only through the calls below. A caller
/// keeps it in sync by repeating this cycle, which is the synchronization
/// flow documented by `zcash_client_backend`:
///
/// 1. [`put_subtree_roots`](Wallet::put_subtree_roots) with the Ironwood
///    roots the server reports, starting at the index given by
///    [`wallet_summary`](Wallet::wallet_summary).
/// 2. [`update_chain_tip`](Wallet::update_chain_tip) with the server's tip.
/// 3. [`suggest_scan_ranges`](Wallet::suggest_scan_ranges), then for each
///    range, in order, [`scan_blocks`](Wallet::scan_blocks) with the blocks
///    of the range and the tree state of the block before it.
/// 4. On [`WalletError::ChainContinuity`],
///    [`rewind_to_height`](Wallet::rewind_to_height) and start over.
/// 5. [`transaction_data_requests`](Wallet::transaction_data_requests), and
///    answer each one.
///
/// All methods are safe to call from any thread. Calls are serialized: the
/// wallet holds one database connection behind a mutex, so a read waits for a
/// scan in progress to finish.
#[derive(uniffi::Object)]
pub struct Wallet {
    network: Network,
    params: Params,
    db: Mutex<Db>,
}

impl Wallet {
    /// Locks the database. A panic while the lock was held leaves the mutex
    /// poisoned. The panic unwound through an SQLite transaction guard, which
    /// rolled the transaction back, so the connection is still consistent and
    /// the poison flag is ignored.
    fn db(&self) -> MutexGuard<'_, Db> {
        self.db.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn account(&self, db: &Db, uuid: AccountUuid) -> Result<Account, WalletError> {
        let account = db.get_account(uuid)?.ok_or(WalletError::AccountNotFound)?;
        let derivation = account.source().key_derivation();
        Ok(Account {
            uuid: uuid.expose_uuid().to_string(),
            name: account.name().map(str::to_owned),
            birthday_height: account.birthday_height().into(),
            ufvk: account
                .ufvk()
                .map(|k: &UnifiedFullViewingKey| k.encode(&self.params))
                .transpose()
                .map_err(|e| WalletError::Database {
                    message: format!("encoding the viewing key: {e}"),
                })?,
            hd_account_index: derivation.map(|d| u32::from(d.account_index())),
            seed_fingerprint: derivation.map(|d| d.seed_fingerprint().to_bytes().to_vec()),
            key_source: account.source().key_source().map(str::to_owned),
        })
    }
}

fn parse_account(uuid: &str) -> Result<AccountUuid, WalletError> {
    Uuid::parse_str(uuid)
        .map(AccountUuid::from_uuid)
        .map_err(|e| WalletError::invalid(format!("account UUID: {e}")))
}

fn parse_tree_state(bytes: &[u8]) -> Result<TreeState, WalletError> {
    TreeState::decode(bytes).map_err(|e| WalletError::invalid(format!("tree state: {e}")))
}

fn parse_transparent(
    params: &Params,
    address: &str,
) -> Result<transparent::address::TransparentAddress, WalletError> {
    match Address::decode(params, address) {
        Some(Address::Transparent(addr)) => Ok(addr),
        _ => Err(WalletError::invalid(
            "not a transparent address for this network",
        )),
    }
}

fn parse_txid(bytes: &[u8]) -> Result<TxId, WalletError> {
    <[u8; 32]>::try_from(bytes)
        .map(TxId::from_bytes)
        .map_err(|_| WalletError::invalid("a transaction ID is 32 bytes"))
}

fn secret_seed(seed: Vec<u8>) -> Result<SecretVec<u8>, WalletError> {
    // Wrap before validating so that the bytes are zeroized on every path.
    let seed = SecretVec::new(seed);
    let len = secrecy::ExposeSecret::expose_secret(&seed).len();
    if !(32..=252).contains(&len) {
        return Err(WalletError::invalid(format!(
            "a seed is 32 to 252 bytes, got {len}"
        )));
    }
    Ok(seed)
}

fn parse_roots<H: HashSer>(
    roots: &[SubtreeRoot],
) -> Result<Vec<CommitmentTreeRoot<H>>, WalletError> {
    roots
        .iter()
        .enumerate()
        .map(|(i, root)| {
            let hash = H::read(&root.root_hash[..])
                .map_err(|e| WalletError::invalid(format!("subtree root {i}: {e}")))?;
            Ok(CommitmentTreeRoot::from_parts(
                BlockHeight::from_u32(root.completing_block_height),
                hash,
            ))
        })
        .collect()
}

#[uniffi::export]
impl Wallet {
    /// Opens the wallet database at `db_path`, creating it if it does not
    /// exist, and brings its schema up to date.
    ///
    /// `seed` is only needed when a pending migration requires it, in which
    /// case opening without it fails with [`WalletError::SeedRequired`]. A
    /// new database never needs it.
    #[uniffi::constructor]
    pub fn open(
        db_path: String,
        network: Network,
        seed: Option<Vec<u8>>,
    ) -> Result<Arc<Self>, WalletError> {
        guard(|| {
            let seed = seed.map(secret_seed).transpose()?;
            let params = network.params()?;
            if db_path.is_empty() {
                return Err(WalletError::invalid("the database path is empty"));
            }

            let mut db = WalletDb::for_path(&db_path, params, SystemClock, UnwrapErr(SysRng))
                .map_err(WalletError::database)?
                .with_anchor_retention_interval(anchor_retention_interval(params.network_type()));
            init_wallet_db(&mut db, seed).map_err(migration_error)?;

            Ok(Arc::new(Wallet {
                network,
                params,
                db: Mutex::new(db),
            }))
        })
    }

    /// The network the wallet was opened for.
    pub fn network(&self) -> Network {
        self.network
    }

    /// Creates an account from a ZIP 32 seed and returns it.
    ///
    /// The account takes the next unused ZIP 32 account index for the seed.
    /// Only the account's viewing keys are stored.
    ///
    /// `birthday_tree_state` is the protobuf-encoded lightwalletd `TreeState`
    /// of the block before the account's birthday. The wallet scans from the
    /// birthday on, so funds received earlier are not found. The birthday
    /// cannot be below the activation height of NU6.3, which is the first
    /// block that can hold Ironwood funds.
    ///
    /// `recover_until` is set when restoring an account that may already
    /// hold funds, to the chain tip height at the time of the restore. It
    /// separates recovery progress from ordinary sync progress in the wallet
    /// summary. It is left unset for a new account.
    pub fn create_account(
        &self,
        name: String,
        seed: Vec<u8>,
        birthday_tree_state: Vec<u8>,
        recover_until: Option<u32>,
        key_source: Option<String>,
    ) -> Result<Account, WalletError> {
        guard(|| {
            let seed = secret_seed(seed)?;
            let tree_state = parse_tree_state(&birthday_tree_state)?;
            let birthday =
                AccountBirthday::from_treestate(tree_state, recover_until.map(BlockHeight::from))
                    .map_err(|e| WalletError::invalid(format!("birthday tree state: {e}")))?;
            let activation = self.params.ironwood_activation();
            if birthday.height() < activation {
                return Err(WalletError::invalid(format!(
                    "the account birthday {} is before NU6.3 activates at {activation}",
                    birthday.height(),
                )));
            }

            let mut db = self.db();
            let (uuid, _usk) = db.create_account(&name, &seed, &birthday, key_source.as_deref())?;
            self.account(&db, uuid)
        })
    }

    /// Lists the accounts in the wallet.
    pub fn accounts(&self) -> Result<Vec<Account>, WalletError> {
        guard(|| {
            let db = self.db();
            let mut accounts = db
                .get_account_ids()?
                .into_iter()
                .map(|uuid| self.account(&db, uuid))
                .collect::<Result<Vec<_>, _>>()?;
            // The database returns accounts in no particular order.
            accounts
                .sort_by(|a, b| (a.hd_account_index, &a.uuid).cmp(&(b.hd_account_index, &b.uuid)));
            Ok(accounts)
        })
    }

    /// Reports whether `seed` is the seed of at least one account derived in
    /// this wallet. It is false for a wallet with no derived accounts.
    pub fn is_seed_relevant(&self, seed: Vec<u8>) -> Result<bool, WalletError> {
        guard(|| {
            let seed = secret_seed(seed)?;
            Ok(matches!(
                self.db().seed_relevance_to_derived_accounts(&seed)?,
                SeedRelevance::Relevant { .. }
            ))
        })
    }

    /// Returns the account's current unified address, which has an Ironwood
    /// and a transparent receiver.
    ///
    /// It is the newest such address that
    /// [`next_available_address`](Wallet::next_available_address) generated.
    /// Before the first one is generated it is the default address of the
    /// account without its Sapling receiver.
    pub fn current_address(&self, account_uuid: String) -> Result<String, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let db = self.db();
            if let Some(ua) =
                db.get_last_generated_address_matching(uuid, IRONWOOD_AND_TRANSPARENT)?
            {
                return Ok(ua.encode(&self.params));
            }

            // The database generates the default address of an account with
            // every receiver the account has a key for. Without its Sapling
            // receiver it pays the same Orchard and transparent receivers,
            // which the wallet already watches.
            let default = db
                .get_last_generated_address_matching(uuid, UnifiedAddressRequest::AllAvailableKeys)?
                .ok_or(WalletError::AccountNotFound)?;
            let ua = default
                .prune_retaining(&[Typecode::ORCHARD, Typecode::P2PKH])
                .map_err(|e| WalletError::Database {
                    message: format!("the default address: {e}"),
                })?
                .ok_or_else(|| WalletError::Database {
                    message: "the default address has no Ironwood or transparent receiver".into(),
                })?;
            Ok(ua.encode(&self.params))
        })
    }

    /// Generates, stores and returns a new unified address for the account.
    ///
    /// With [`ReceiverSet::All`] the address has an Ironwood and a
    /// transparent receiver and is taken from the wallet's transparent gap.
    /// It fails with [`WalletError::AddressGapLimit`] when every address in
    /// the gap has been handed out. With [`ReceiverSet::Shielded`] the
    /// address has an Ironwood receiver only.
    pub fn next_available_address(
        &self,
        account_uuid: String,
        receivers: ReceiverSet,
    ) -> Result<String, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let request = match receivers {
                ReceiverSet::All => IRONWOOD_AND_TRANSPARENT,
                ReceiverSet::Shielded => IRONWOOD_ONLY,
            };
            self.db()
                .get_next_available_address(uuid, request)?
                .map(|(ua, _)| ua.encode(&self.params))
                .ok_or(WalletError::AccountNotFound)
        })
    }

    /// Lists the transparent addresses of the account that can receive
    /// funds, including its change addresses. These are the addresses to ask
    /// a server about when looking for transparent outputs.
    pub fn transparent_receivers(&self, account_uuid: String) -> Result<Vec<String>, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let db = self.db();
            if db.get_account(uuid)?.is_none() {
                return Err(WalletError::AccountNotFound);
            }
            let mut addresses: Vec<String> = db
                .get_transparent_receivers(uuid, true, true)?
                .into_keys()
                .map(|addr| addr.encode(&self.params))
                .collect();
            addresses.sort();
            Ok(addresses)
        })
    }

    /// The earliest birthday height among the wallet's accounts, or nothing
    /// for a wallet without accounts.
    pub fn birthday_height(&self) -> Result<Option<u32>, WalletError> {
        guard(|| Ok(self.db().get_wallet_birthday()?.map(u32::from)))
    }

    /// The chain tip most recently given to
    /// [`update_chain_tip`](Wallet::update_chain_tip), or nothing if it has
    /// never been called.
    pub fn chain_tip_height(&self) -> Result<Option<u32>, WalletError> {
        guard(|| Ok(self.db().chain_height()?.map(u32::from)))
    }

    /// The height up to which every block since the birthday has been
    /// scanned, or nothing if no block has been scanned.
    pub fn fully_scanned_height(&self) -> Result<Option<u32>, WalletError> {
        guard(|| {
            Ok(self
                .db()
                .block_fully_scanned()?
                .map(|meta| u32::from(meta.block_height())))
        })
    }

    /// The greatest height that has been scanned, or nothing if no block has
    /// been scanned. Blocks below it may be unscanned.
    pub fn max_scanned_height(&self) -> Result<Option<u32>, WalletError> {
        guard(|| {
            Ok(self
                .db()
                .block_max_scanned()?
                .map(|meta| u32::from(meta.block_height())))
        })
    }

    /// The hash of the scanned block at `height`, in the byte order of the
    /// compact block's `hash` field, or nothing if the wallet has not scanned
    /// that block.
    pub fn block_hash(&self, height: u32) -> Result<Option<Vec<u8>>, WalletError> {
        guard(|| {
            Ok(self
                .db()
                .get_block_hash(BlockHeight::from_u32(height))?
                .map(|hash| hash.0.to_vec()))
        })
    }

    /// Stores roots of completed subtrees of the Ironwood note commitment
    /// tree.
    ///
    /// The roots let the wallet scan the chain out of order: with them it can
    /// build a witness for a note without having scanned every block before
    /// it, so recent funds become spendable before the full history is
    /// scanned. `roots` holds consecutive roots, the first one being the root
    /// of the subtree at `start_index`. An empty list is allowed.
    ///
    /// The wallet takes no roots of the Sapling and Orchard trees. It holds
    /// no notes in them, so it never builds a witness in them. Leaving them
    /// out also keeps the height range that the wallet scans first as short
    /// as the Ironwood tree allows: that range begins at the lowest
    /// last-completed subtree among the trees the wallet has roots for.
    pub fn put_subtree_roots(
        &self,
        start_index: u64,
        roots: Vec<SubtreeRoot>,
    ) -> Result<(), WalletError> {
        guard(|| {
            let roots = parse_roots::<orchard::tree::MerkleHashOrchard>(&roots)?;
            if roots.is_empty() {
                return Ok(());
            }
            self.db()
                .put_ironwood_subtree_roots(start_index, &roots)
                .map_err(|e| WalletError::Database {
                    message: format!("storing subtree roots: {e:?}"),
                })
        })
    }

    /// Tells the wallet the height of the chain tip, which updates the ranges
    /// that [`suggest_scan_ranges`](Wallet::suggest_scan_ranges) returns.
    ///
    /// A height below the wallet's greatest scanned height is not treated as
    /// a reorganization here. That case is found when the replacement blocks
    /// are scanned, or by comparing [`block_hash`](Wallet::block_hash) with
    /// the server's hash.
    pub fn update_chain_tip(&self, height: u32) -> Result<(), WalletError> {
        guard(|| {
            self.db().update_chain_tip(BlockHeight::from_u32(height))?;
            Ok(())
        })
    }

    /// Returns the block ranges the wallet wants scanned, most urgent first.
    /// The list is empty when the wallet is scanned up to the chain tip.
    pub fn suggest_scan_ranges(&self) -> Result<Vec<ScanRange>, WalletError> {
        guard(|| {
            Ok(self
                .db()
                .suggest_scan_ranges()?
                .iter()
                .map(ScanRange::from)
                .collect())
        })
    }

    /// Scans a batch of consecutive compact blocks for transactions of the
    /// wallet's accounts and stores what it finds.
    ///
    /// `from_state` is the protobuf-encoded lightwalletd `TreeState` of the
    /// block before the first block of the batch. `blocks` are
    /// protobuf-encoded `CompactBlock`s in ascending height order.
    ///
    /// The batch is stored in one database transaction: on error nothing from
    /// it is kept. [`WalletError::ChainContinuity`] means the batch does not
    /// connect to blocks the wallet has already scanned.
    pub fn scan_blocks(
        &self,
        from_state: Vec<u8>,
        blocks: Vec<Vec<u8>>,
    ) -> Result<ScanSummary, WalletError> {
        guard(|| {
            let chain_state = parse_tree_state(&from_state)?
                .to_chain_state()
                .map_err(|e| WalletError::invalid(format!("tree state: {e}")))?;
            let source = MemoryBlockSource::decode(&blocks)?;

            let from_height = chain_state.block_height() + 1;
            match source.first_height() {
                None => return Err(WalletError::invalid("no blocks to scan")),
                Some(first) if first != u64::from(from_height) => {
                    return Err(WalletError::invalid(format!(
                        "the first block is at height {first} but the tree state is for height \
                         {}",
                        chain_state.block_height(),
                    )));
                }
                Some(_) => {}
            }

            let mut db = self.db();
            let summary = scan_cached_blocks(
                &self.params,
                &source,
                &mut *db,
                from_height,
                &chain_state,
                source.len(),
            )?;

            let range = summary.scanned_range();
            Ok(ScanSummary {
                start_height: range.start.into(),
                end_height: range.end.into(),
                // librustzcash counts Ironwood notes as Orchard notes. The
                // Sapling counts are zero for a wallet without Sapling
                // addresses and are added so that nothing found goes
                // unreported.
                spent_notes: (summary.spent_orchard_note_count()
                    + summary.spent_sapling_note_count()) as u64,
                received_notes: (summary.received_orchard_note_count()
                    + summary.received_sapling_note_count()) as u64,
            })
        })
    }

    /// Discards everything the wallet learned from blocks above `height` and
    /// returns the height it rewound to, which is `height` or lower.
    ///
    /// The wallet can rewind to a height for which it kept a note commitment
    /// tree checkpoint, and to any height above its newest checkpoint. It
    /// rewinds to the greatest such height that is not above `height`. If
    /// there is none, [`WalletError::RewindInvalid`] names the lowest
    /// checkpoint, when one exists.
    ///
    /// Checkpoints are made for the tree state a scanned batch starts from,
    /// for every block that adds a note commitment, and from NU6.3 on for
    /// every height of the anchor grid.
    pub fn rewind_to_height(&self, height: u32) -> Result<u32, WalletError> {
        guard(|| {
            Ok(self
                .db()
                .truncate_to_height(BlockHeight::from_u32(height))?
                .into())
        })
    }

    /// Returns balances and sync progress, or nothing while the wallet lacks
    /// the data to compute them: before it has an account, a chain tip and at
    /// least one scanned block.
    ///
    /// `min_confirmations` is the number of confirmations a received output
    /// needs to count as spendable. Unset selects the ZIP 315 policy: 3
    /// confirmations for outputs of the wallet's own transactions and 10 for
    /// everything else.
    pub fn wallet_summary(
        &self,
        min_confirmations: Option<u32>,
    ) -> Result<Option<WalletSummary>, WalletError> {
        guard(|| {
            let policy = match min_confirmations {
                None => ConfirmationsPolicy::default(),
                Some(n) => ConfirmationsPolicy::new_symmetrical(
                    NonZeroU32::new(n).ok_or_else(|| {
                        WalletError::invalid("min_confirmations must be at least 1")
                    })?,
                    true,
                ),
            };
            let Some(summary) = self.db().get_wallet_summary(policy)? else {
                return Ok(None);
            };

            let mut account_balances: Vec<AccountBalance> = summary
                .account_balances()
                .iter()
                .map(
                    |(uuid, balance): (&AccountUuid, &data_api::AccountBalance)| AccountBalance {
                        account_uuid: uuid.expose_uuid().to_string(),
                        ironwood: balance.ironwood_balance().into(),
                        transparent: (&balance.unshielded_balance()).into(),
                        total: balance.total().into_u64(),
                    },
                )
                .collect();
            account_balances.sort_by(|a, b| a.account_uuid.cmp(&b.account_uuid));

            let progress = summary.progress();
            Ok(Some(WalletSummary {
                account_balances,
                chain_tip_height: summary.chain_tip_height().into(),
                fully_scanned_height: summary.fully_scanned_height().into(),
                scan_progress: progress.scan().into(),
                recovery_progress: progress.recovery().map(Into::into),
                next_subtree_index: summary.next_ironwood_subtree_index(),
            }))
        })
    }

    /// The height from which a server should be asked for the account's
    /// unspent transparent outputs.
    pub fn utxo_query_height(&self, account_uuid: String) -> Result<u32, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let db = self.db();
            if db.get_account(uuid)?.is_none() {
                return Err(WalletError::AccountNotFound);
            }
            Ok(db.utxo_query_height(uuid)?.into())
        })
    }

    /// Records an unspent transparent output that a server reported for one
    /// of the wallet's transparent addresses. Storing an output again
    /// replaces it.
    pub fn put_utxo(&self, utxo: Utxo) -> Result<(), WalletError> {
        guard(|| {
            let txid = <[u8; 32]>::try_from(&utxo.txid[..])
                .map_err(|_| WalletError::invalid("a transaction ID is 32 bytes"))?;
            let value = Zatoshis::from_u64(utxo.value_zat)
                .map_err(|_| WalletError::invalid("the output value is out of range"))?;
            let output = WalletTransparentOutput::<AccountUuid>::from_parts(
                OutPoint::new(txid, utxo.index),
                TxOut::new(value, Script(script::Code(utxo.script))),
                Some(BlockHeight::from_u32(utxo.height)),
                // The wallet matches the output to an account by its address.
                None,
                None,
                None,
            )
            .ok_or_else(|| WalletError::invalid("the script does not pay a transparent address"))?;
            self.db().put_received_transparent_utxo(&output)?;
            Ok(())
        })
    }

    /// Lists the transaction data the wallet needs from the chain. See
    /// [`TransactionDataRequest`].
    pub fn transaction_data_requests(&self) -> Result<Vec<TransactionDataRequest>, WalletError> {
        guard(|| {
            Ok(self
                .db()
                .transaction_data_requests()?
                .into_iter()
                .map(|request| match request {
                    data_api::TransactionDataRequest::GetStatus(txid) => {
                        TransactionDataRequest::GetStatus {
                            txid: txid.as_ref().to_vec(),
                        }
                    }
                    data_api::TransactionDataRequest::Enhancement(txid) => {
                        TransactionDataRequest::Enhancement {
                            txid: txid.as_ref().to_vec(),
                        }
                    }
                    data_api::TransactionDataRequest::TransactionsInvolvingAddress(req) => {
                        TransactionDataRequest::TransactionsInvolvingAddress {
                            address: Address::Transparent(req.address()).encode(&self.params),
                            block_range_start: req.block_range_start().into(),
                            block_range_end: req.block_range_end().map(u32::from),
                            request_at: req
                                .request_at()
                                .map(|t| t.duration_since(UNIX_EPOCH).map_or(0, |d| d.as_secs())),
                            tx_status_filter: req.tx_status_filter().into(),
                            output_status_filter: req.output_status_filter().into(),
                        }
                    }
                })
                .collect())
        })
    }

    /// Closes a [`TransactionDataRequest::TransactionsInvolvingAddress`]
    /// request once every transaction it covers has been fetched and stored.
    ///
    /// `as_of_height` is the height of the last block the query covered. For
    /// a request with a `block_range_end` it must be that height minus one.
    /// Without this call the wallet repeats the request on every cycle.
    pub fn notify_address_checked(
        &self,
        request: TransactionDataRequest,
        as_of_height: u32,
    ) -> Result<(), WalletError> {
        guard(|| {
            let TransactionDataRequest::TransactionsInvolvingAddress {
                address,
                block_range_start,
                block_range_end,
                request_at,
                tx_status_filter,
                output_status_filter,
            } = request
            else {
                return Err(WalletError::invalid(
                    "not a request for the transactions of an address",
                ));
            };
            let address = parse_transparent(&self.params, &address)?;
            let data_api::TransactionDataRequest::TransactionsInvolvingAddress(request) =
                data_api::TransactionDataRequest::transactions_involving_address(
                    address,
                    BlockHeight::from_u32(block_range_start),
                    block_range_end.map(BlockHeight::from_u32),
                    request_at.map(|secs| UNIX_EPOCH + Duration::from_secs(secs)),
                    tx_status_filter.into(),
                    output_status_filter.into(),
                )
            else {
                unreachable!("the constructor builds this variant");
            };
            self.db()
                .notify_address_checked(request, BlockHeight::from_u32(as_of_height))?;
            Ok(())
        })
    }

    /// Schedules the next check of a transparent address of the wallet for
    /// unspent outputs, at a random time `offset_seconds` from now on
    /// average, and returns that time in seconds since the Unix epoch. The
    /// randomization keeps a server from linking the wallet's addresses by
    /// the timing of its queries. Returns nothing for an address the wallet
    /// does not schedule checks for.
    pub fn schedule_next_check(
        &self,
        address: String,
        offset_seconds: u32,
    ) -> Result<Option<u64>, WalletError> {
        guard(|| {
            let address = parse_transparent(&self.params, &address)?;
            Ok(self
                .db()
                .schedule_next_check(&address, offset_seconds)?
                .map(|t| t.duration_since(UNIX_EPOCH).map_or(0, |d| d.as_secs())))
        })
    }

    /// Records what a server reported about a transaction, in answer to a
    /// [`TransactionDataRequest`].
    pub fn set_transaction_status(
        &self,
        txid: Vec<u8>,
        status: TransactionStatus,
    ) -> Result<(), WalletError> {
        guard(|| {
            let txid = parse_txid(&txid)?;
            let status = match status {
                TransactionStatus::TxidNotRecognized => {
                    data_api::TransactionStatus::TxidNotRecognized
                }
                TransactionStatus::NotInMainChain => data_api::TransactionStatus::NotInMainChain,
                TransactionStatus::Mined { height } => {
                    data_api::TransactionStatus::Mined(BlockHeight::from_u32(height))
                }
            };
            self.db().set_transaction_status(txid, status)?;
            Ok(())
        })
    }

    /// Decrypts a full transaction with the wallet's viewing keys, stores the
    /// details that compact blocks leave out (memos, transparent inputs and
    /// outputs, outgoing payments), and returns its transaction ID.
    ///
    /// `mined_height` is the height of the block holding the transaction, or
    /// unset for a transaction that is not mined.
    pub fn decrypt_and_store_transaction(
        &self,
        tx: Vec<u8>,
        mined_height: Option<u32>,
    ) -> Result<Vec<u8>, WalletError> {
        guard(|| {
            let mined_height = mined_height.map(BlockHeight::from_u32);
            let mut db = self.db();

            // Transactions before v5 do not encode their consensus branch, so
            // it is taken from the height the transaction is, or would be,
            // mined at.
            let branch_height = match mined_height {
                Some(h) => h,
                None => db
                    .chain_height()?
                    .map_or(BlockHeight::from_u32(0), |tip| tip + 1),
            };
            let branch_id = BranchId::for_height(&self.params, branch_height);
            let tx = Transaction::read(&tx[..], branch_id)
                .map_err(|e| WalletError::invalid(format!("transaction: {e}")))?;

            decrypt_and_store_transaction(&self.params, &mut *db, &tx, mined_height)?;
            Ok(tx.txid().as_ref().to_vec())
        })
    }
}

#[cfg(test)]
mod tests;
