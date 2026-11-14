// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Tests of [`Wallet`] against a synthetic chain of empty blocks. Scanning
//! real shielded outputs needs a real chain and is covered by the Go tests
//! that run against the simnet harness.

use std::sync::Arc;

use tempfile::TempDir;
use zcash_primitives::transaction::{Authorized, TransactionData, TxVersion};

use super::*;
use crate::{
    testutil::{all_at, block_hash, encoded_blocks, regtest, tree_state},
    types::{PoolBalance, ScanPriority},
};

const SEED: [u8; 32] = [8; 32];
const OTHER_SEED: [u8; 32] = [9; 32];

/// A seed whose first ten diversifier indexes have no valid Sapling
/// diversifier. An address with a Sapling receiver cannot be taken from its
/// transparent gap, and the default address the database generates for it
/// sits beyond the gap.
const NO_SAPLING_SEED: [u8; 32] = [7; 32];

/// The height of the block before the test account's birthday.
const PRIOR: u32 = 99;
const BIRTHDAY: u32 = PRIOR + 1;

struct Fixture {
    // Held so that the directory outlives the wallet.
    dir: TempDir,
    wallet: Arc<Wallet>,
}

impl Fixture {
    fn db_path(&self) -> String {
        self.dir
            .path()
            .join("wallet.sqlite")
            .to_str()
            .unwrap()
            .to_owned()
    }
}

fn empty_wallet() -> Fixture {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("wallet.sqlite");
    let wallet = Wallet::open(path.to_str().unwrap().to_owned(), regtest(), None).unwrap();
    Fixture { dir, wallet }
}

/// A wallet with one account born at [`BIRTHDAY`] on fork 0.
fn wallet_with_account() -> (Fixture, Account) {
    let fx = empty_wallet();
    let account = fx
        .wallet
        .create_account(
            "primary".into(),
            SEED.to_vec(),
            tree_state(PRIOR, 0),
            None,
            None,
        )
        .unwrap();
    (fx, account)
}

/// Scans the blocks of `range` on `fork` in one batch.
fn scan(
    wallet: &Wallet,
    range: std::ops::Range<u32>,
    fork: u8,
) -> Result<ScanSummary, WalletError> {
    wallet.scan_blocks(
        tree_state(range.start - 1, fork),
        encoded_blocks(range, fork),
    )
}

fn assert_invalid(err: WalletError, needle: &str) {
    assert!(
        matches!(&err, WalletError::InvalidArgument { message } if message.contains(needle)),
        "want InvalidArgument containing {needle:?}, got {err:?}"
    );
}

#[test]
fn open_creates_and_reopens() {
    let fx = empty_wallet();
    assert_eq!(fx.wallet.network(), regtest());
    assert!(fx.wallet.accounts().unwrap().is_empty());
    assert_eq!(fx.wallet.birthday_height().unwrap(), None);
    assert_eq!(fx.wallet.chain_tip_height().unwrap(), None);
    assert_eq!(fx.wallet.fully_scanned_height().unwrap(), None);
    assert_eq!(fx.wallet.max_scanned_height().unwrap(), None);
    assert_eq!(fx.wallet.wallet_summary(None).unwrap(), None);
    assert!(fx.wallet.suggest_scan_ranges().unwrap().is_empty());

    // A second handle on the same file sees the same, migrated database.
    let again = Wallet::open(fx.db_path(), regtest(), None).unwrap();
    assert!(again.accounts().unwrap().is_empty());
}

#[test]
fn consensus_branch_id_changes_at_an_upgrade() {
    const NU6_2: u32 = 0x5437_f330;
    const NU6_3: u32 = 0x37a5_165b;
    const NU7: u32 = 0x7719_0ad9;

    // On mainnet NU6.3 activates at 3,428,143 and NU7 at 3,543,405. The
    // public servers reported 77190ad9 for a tip at 3,570,595.
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("wallet.sqlite");
    let main = Wallet::open(path.to_str().unwrap().to_owned(), Network::Mainnet, None).unwrap();
    assert_eq!(main.consensus_branch_id(3_428_142), NU6_2);
    assert_eq!(main.consensus_branch_id(3_428_143), NU6_3);
    assert_eq!(main.consensus_branch_id(3_543_404), NU6_3);
    assert_eq!(main.consensus_branch_id(3_543_405), NU7);
    assert_eq!(main.consensus_branch_id(3_570_595), NU7);
    // Before Overwinter there is no branch, which a node reports as zero.
    assert_eq!(main.consensus_branch_id(0), 0);

    // A regtest network follows its own schedule.
    let mut heights = all_at(1);
    heights.nu6_3 = Some(12);
    heights.nu7 = None;
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("wallet.sqlite");
    let local = Wallet::open(
        path.to_str().unwrap().to_owned(),
        Network::Regtest { heights },
        None,
    )
    .unwrap();
    assert_eq!(local.consensus_branch_id(11), NU6_2);
    assert_eq!(local.consensus_branch_id(12), NU6_3);
    assert_eq!(local.consensus_branch_id(u32::MAX), NU6_3);
}

#[test]
fn open_rejects_bad_arguments() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir
        .path()
        .join("wallet.sqlite")
        .to_str()
        .unwrap()
        .to_owned();

    assert_invalid(
        Wallet::open(String::new(), regtest(), None).err().unwrap(),
        "path is empty",
    );
    assert_invalid(
        Wallet::open(path.clone(), regtest(), Some(vec![1; 16]))
            .err()
            .unwrap(),
        "32 to 252 bytes",
    );

    // A chain without the Ironwood pool.
    let mut heights = all_at(1);
    heights.nu6_3 = None;
    heights.nu7 = None;
    assert_invalid(
        Wallet::open(path, Network::Regtest { heights }, None)
            .err()
            .unwrap(),
        "nu6_3 must activate",
    );

    let missing = dir.path().join("no-such-dir").join("wallet.sqlite");
    let err = Wallet::open(missing.to_str().unwrap().to_owned(), regtest(), None)
        .err()
        .unwrap();
    assert!(matches!(err, WalletError::Database { .. }), "{err:?}");
}

#[test]
fn open_accepts_a_seed_that_no_migration_needs() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir
        .path()
        .join("wallet.sqlite")
        .to_str()
        .unwrap()
        .to_owned();
    Wallet::open(path, regtest(), Some(SEED.to_vec())).unwrap();
}

#[test]
fn reopening_with_a_foreign_seed_is_reported() {
    let (fx, _) = wallet_with_account();
    let err = Wallet::open(fx.db_path(), regtest(), Some(OTHER_SEED.to_vec()))
        .err()
        .unwrap();
    assert!(matches!(err, WalletError::SeedNotRelevant), "{err:?}");

    // The wallet's own seed, or no seed, is accepted.
    Wallet::open(fx.db_path(), regtest(), Some(SEED.to_vec())).unwrap();
    Wallet::open(fx.db_path(), regtest(), None).unwrap();
}

#[test]
fn create_account_returns_the_account() {
    let (fx, account) = wallet_with_account();

    assert!(Uuid::parse_str(&account.uuid).is_ok(), "{}", account.uuid);
    assert_eq!(account.name.as_deref(), Some("primary"));
    assert_eq!(account.birthday_height, BIRTHDAY);
    assert_eq!(account.hd_account_index, Some(0));
    assert_eq!(account.seed_fingerprint.as_ref().map(Vec::len), Some(32));
    assert_eq!(account.key_source, None);
    let ufvk = account.ufvk.as_deref().unwrap();
    assert!(ufvk.starts_with("uviewregtest1"), "{ufvk}");

    assert_eq!(fx.wallet.accounts().unwrap(), vec![account]);
    assert_eq!(fx.wallet.birthday_height().unwrap(), Some(BIRTHDAY));
}

#[test]
fn accounts_from_one_seed_take_consecutive_indexes() {
    let (fx, first) = wallet_with_account();
    let second = fx
        .wallet
        .create_account(
            "second".into(),
            SEED.to_vec(),
            tree_state(PRIOR + 50, 0),
            Some(400),
            Some("hardware".into()),
        )
        .unwrap();

    assert_eq!(second.hd_account_index, Some(1));
    assert_eq!(second.birthday_height, BIRTHDAY + 50);
    assert_eq!(second.key_source.as_deref(), Some("hardware"));
    assert_eq!(second.seed_fingerprint, first.seed_fingerprint);
    assert_ne!(second.uuid, first.uuid);
    assert_ne!(second.ufvk, first.ufvk);

    // Listed in account index order, and the wallet birthday is the earliest.
    assert_eq!(fx.wallet.accounts().unwrap(), vec![first, second]);
    assert_eq!(fx.wallet.birthday_height().unwrap(), Some(BIRTHDAY));
}

#[test]
fn an_account_cannot_begin_before_ironwood() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("wallet.sqlite");
    let mut heights = all_at(1);
    heights.nu6_3 = Some(50);
    heights.nu7 = Some(50);
    let w = Wallet::open(
        path.to_str().unwrap().to_owned(),
        Network::Regtest { heights },
        None,
    )
    .unwrap();
    let create = |prior: u32| {
        w.create_account(
            "early".into(),
            SEED.to_vec(),
            tree_state(prior, 0),
            None,
            None,
        )
    };

    // The birthday is the height after the tree state.
    assert_invalid(
        create(48).unwrap_err(),
        "the account birthday 49 is before NU6.3 activates at 50",
    );
    assert!(w.accounts().unwrap().is_empty());

    let account = create(49).unwrap();
    assert_eq!(account.birthday_height, 50);
}

#[test]
fn account_keys_are_deterministic() {
    let (_fx1, a) = wallet_with_account();
    let (_fx2, b) = wallet_with_account();
    assert_eq!(a.ufvk, b.ufvk);
    assert_eq!(a.seed_fingerprint, b.seed_fingerprint);
}

#[test]
fn create_account_rejects_bad_arguments() {
    let fx = empty_wallet();
    let create = |seed: Vec<u8>, state: Vec<u8>| {
        fx.wallet
            .create_account("a".into(), seed, state, None, None)
            .unwrap_err()
    };

    assert_invalid(create(vec![1; 31], tree_state(PRIOR, 0)), "32 to 252 bytes");
    assert_invalid(
        create(vec![1; 253], tree_state(PRIOR, 0)),
        "32 to 252 bytes",
    );
    // Field 1 with wire type 7, which protobuf does not define.
    assert_invalid(create(SEED.to_vec(), vec![0x0f, 0xff]), "tree state");

    // A tree state whose Sapling tree is not a serialized tree.
    let bad_tree = TreeState {
        network: "regtest".into(),
        height: PRIOR.into(),
        hash: hex::encode(block_hash(PRIOR, 0)),
        time: 0,
        sapling_tree: "zz".into(),
        orchard_tree: String::new(),
        ironwood_tree: String::new(),
    }
    .encode_to_vec();
    assert_invalid(create(SEED.to_vec(), bad_tree), "birthday tree state");

    assert!(fx.wallet.accounts().unwrap().is_empty());
}

#[test]
fn seed_relevance() {
    let fx = empty_wallet();
    assert!(!fx.wallet.is_seed_relevant(SEED.to_vec()).unwrap());

    let (fx, _) = wallet_with_account();
    assert!(fx.wallet.is_seed_relevant(SEED.to_vec()).unwrap());
    assert!(!fx.wallet.is_seed_relevant(OTHER_SEED.to_vec()).unwrap());
    assert_invalid(
        fx.wallet.is_seed_relevant(vec![0; 4]).unwrap_err(),
        "32 to 252 bytes",
    );
}

/// Reports whether a unified address has an Orchard, a Sapling and a
/// transparent receiver, in that order. The Orchard receiver is the one that
/// receives into the Ironwood pool.
fn receivers(address: &str) -> (bool, bool, bool) {
    let params = regtest().params().unwrap();
    match Address::decode(&params, address) {
        Some(Address::Unified(ua)) => (ua.has_orchard(), ua.has_sapling(), ua.has_transparent()),
        other => panic!("not a unified address: {other:?}"),
    }
}

#[test]
fn addresses() {
    let (fx, account) = wallet_with_account();
    let w = &fx.wallet;

    let current = w.current_address(account.uuid.clone()).unwrap();
    assert!(current.starts_with("uregtest1"), "{current}");
    // Asking again does not rotate the address.
    assert_eq!(w.current_address(account.uuid.clone()).unwrap(), current);

    // An Ironwood and a transparent receiver, and no Sapling receiver.
    assert_eq!(receivers(&current), (true, false, true));
    let transparent = crate::transparent_receiver(regtest(), current.clone())
        .unwrap()
        .unwrap();
    assert!(transparent.starts_with("tm"), "{transparent}");
    assert!(
        w.transparent_receivers(account.uuid.clone())
            .unwrap()
            .iter()
            .any(|r| r.address == transparent)
    );

    let shielded = w
        .next_available_address(account.uuid.clone(), ReceiverSet::Shielded)
        .unwrap();
    assert_ne!(shielded, current);
    assert_eq!(receivers(&shielded), (true, false, false));
    assert_eq!(
        crate::transparent_receiver(regtest(), shielded).unwrap(),
        None
    );

    let all = w
        .next_available_address(account.uuid.clone(), ReceiverSet::All)
        .unwrap();
    assert_ne!(all, current);
    assert_eq!(receivers(&all), (true, false, true));
    let next_transparent = crate::transparent_receiver(regtest(), all.clone())
        .unwrap()
        .unwrap();
    assert_ne!(next_transparent, transparent);
    assert!(
        w.transparent_receivers(account.uuid.clone())
            .unwrap()
            .iter()
            .any(|r| r.address == next_transparent)
    );
    // The newest address with both receivers is now the current one.
    assert_eq!(w.current_address(account.uuid).unwrap(), all);
}

#[test]
fn addresses_do_not_need_a_sapling_diversifier() {
    let fx = empty_wallet();
    let w = &fx.wallet;
    let account = w
        .create_account(
            "no sapling".into(),
            NO_SAPLING_SEED.to_vec(),
            tree_state(PRIOR, 0),
            None,
            None,
        )
        .unwrap();

    let current = w.current_address(account.uuid.clone()).unwrap();
    assert_eq!(receivers(&current), (true, false, true));
    let transparent = crate::transparent_receiver(regtest(), current.clone())
        .unwrap()
        .unwrap();
    assert!(
        w.transparent_receivers(account.uuid.clone())
            .unwrap()
            .iter()
            .any(|r| r.address == transparent)
    );

    // An address from the transparent gap, where no index has a Sapling
    // diversifier.
    let all = w
        .next_available_address(account.uuid.clone(), ReceiverSet::All)
        .unwrap();
    assert_eq!(receivers(&all), (true, false, true));
    assert_ne!(all, current);
    assert_eq!(w.current_address(account.uuid).unwrap(), all);
}

#[test]
fn gap_limit_is_reported() {
    let (fx, account) = wallet_with_account();
    let w = &fx.wallet;

    // An address with a transparent receiver is taken from the window of
    // unused transparent addresses. The window does not move while nothing
    // is paid to them, so it runs out.
    let mut generated = Vec::new();
    let err = loop {
        match w.next_available_address(account.uuid.clone(), ReceiverSet::All) {
            Ok(address) => generated.push(address),
            Err(err) => break err,
        }
        assert!(generated.len() <= 20, "no gap limit after 20 addresses");
    };
    assert!(
        matches!(err, WalletError::AddressGapLimit { .. }),
        "{err:?}"
    );
    assert!(!generated.is_empty());
    for address in &generated {
        assert_eq!(receivers(address), (true, false, true));
    }

    // Shielded addresses are not subject to the gap limit.
    w.next_available_address(account.uuid.clone(), ReceiverSet::Shielded)
        .unwrap();
    assert_eq!(
        w.current_address(account.uuid).unwrap(),
        *generated.last().unwrap()
    );
}

#[test]
fn account_lookups_reject_unknown_and_malformed_ids() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;
    let unknown = Uuid::from_u128(1).to_string();

    let not_found = |err: WalletError| {
        assert!(matches!(err, WalletError::AccountNotFound), "{err:?}");
    };
    not_found(w.current_address(unknown.clone()).unwrap_err());
    not_found(
        w.next_available_address(unknown.clone(), ReceiverSet::All)
            .unwrap_err(),
    );
    not_found(w.transparent_receivers(unknown.clone()).unwrap_err());
    not_found(w.utxo_query_height(unknown.clone()).unwrap_err());

    assert_invalid(
        w.current_address("nope".into()).unwrap_err(),
        "account UUID",
    );
    assert_invalid(
        w.next_available_address("nope".into(), ReceiverSet::All)
            .unwrap_err(),
        "account UUID",
    );
    assert_invalid(
        w.transparent_receivers("nope".into()).unwrap_err(),
        "account UUID",
    );
    assert_invalid(
        w.utxo_query_height("nope".into()).unwrap_err(),
        "account UUID",
    );
}

#[test]
fn sync_cycle() {
    let (fx, account) = wallet_with_account();
    let w = &fx.wallet;
    let tip = BIRTHDAY + 39;

    w.update_chain_tip(tip).unwrap();
    assert_eq!(w.chain_tip_height().unwrap(), Some(tip));

    let ranges = w.suggest_scan_ranges().unwrap();
    assert!(!ranges.is_empty());
    assert_eq!(ranges.iter().map(|r| r.start).min(), Some(BIRTHDAY));
    assert_eq!(ranges.iter().map(|r| r.end).max(), Some(tip + 1));
    assert!(ranges.iter().all(|r| r.start < r.end));
    assert!(
        ranges.windows(2).all(|p| p[0].priority >= p[1].priority),
        "{ranges:?}"
    );

    // There is no summary until a block has been scanned.
    assert_eq!(w.wallet_summary(None).unwrap(), None);

    // Scan the whole range in two batches.
    let mid = BIRTHDAY + 20;
    let first = scan(w, BIRTHDAY..mid, 0).unwrap();
    assert_eq!(
        first,
        ScanSummary {
            start_height: BIRTHDAY,
            end_height: mid,
        }
    );
    assert_eq!(w.fully_scanned_height().unwrap(), Some(mid - 1));
    assert_eq!(w.max_scanned_height().unwrap(), Some(mid - 1));

    let second = scan(w, mid..tip + 1, 0).unwrap();
    assert_eq!((second.start_height, second.end_height), (mid, tip + 1));
    assert_eq!(w.fully_scanned_height().unwrap(), Some(tip));
    assert_eq!(w.max_scanned_height().unwrap(), Some(tip));
    assert!(w.suggest_scan_ranges().unwrap().is_empty());

    assert_eq!(w.block_hash(tip).unwrap(), Some(block_hash(tip, 0)));
    assert_eq!(w.block_hash(tip + 1).unwrap(), None);

    let summary = w.wallet_summary(Some(1)).unwrap().unwrap();
    assert_eq!(summary.fully_scanned_height, tip);
    assert_eq!(summary.chain_tip_height, tip);
    // Nothing to recover: the account was not restored.
    assert_eq!(summary.recovery_progress.map_or(0, |r| r.denominator), 0);
    assert_eq!(summary.account_balances.len(), 1);
    assert_eq!(summary.account_balances[0].account_uuid, account.uuid);
    assert_eq!(summary.account_balances[0].total, 0);
    assert_eq!(summary.account_balances[0].ironwood, PoolBalance::default());
    assert_eq!(
        summary.account_balances[0].transparent,
        PoolBalance::default()
    );

    // A new block extends the chain: one more range, one more scan.
    w.update_chain_tip(tip + 1).unwrap();
    let ranges = w.suggest_scan_ranges().unwrap();
    assert_eq!(ranges.len(), 1, "{ranges:?}");
    assert_eq!((ranges[0].start, ranges[0].end), (tip + 1, tip + 2));
    scan(w, tip + 1..tip + 2, 0).unwrap();
    assert!(w.suggest_scan_ranges().unwrap().is_empty());
}

#[test]
fn scan_blocks_rejects_bad_input() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;
    w.update_chain_tip(BIRTHDAY + 10).unwrap();

    assert_invalid(
        w.scan_blocks(tree_state(PRIOR, 0), vec![]).unwrap_err(),
        "no blocks to scan",
    );
    assert_invalid(
        w.scan_blocks(vec![0x0f, 0xff], encoded_blocks(BIRTHDAY..BIRTHDAY + 1, 0))
            .unwrap_err(),
        "tree state",
    );
    assert_invalid(
        w.scan_blocks(tree_state(PRIOR, 0), vec![vec![0x0f, 0xff]])
            .unwrap_err(),
        "compact block 0",
    );
    // The blocks do not start right after the tree state.
    assert_invalid(
        w.scan_blocks(
            tree_state(PRIOR, 0),
            encoded_blocks(BIRTHDAY + 1..BIRTHDAY + 3, 0),
        )
        .unwrap_err(),
        "first block is at height",
    );
    // A tree state whose hash is not hex.
    let bad_hash = TreeState {
        network: "regtest".into(),
        height: PRIOR.into(),
        hash: "not hex".into(),
        ..Default::default()
    }
    .encode_to_vec();
    assert_invalid(
        w.scan_blocks(bad_hash, encoded_blocks(BIRTHDAY..BIRTHDAY + 1, 0))
            .unwrap_err(),
        "tree state",
    );

    // Nothing was stored by the failed calls.
    assert_eq!(w.max_scanned_height().unwrap(), None);
}

#[test]
fn scan_blocks_reports_a_gap_inside_the_batch() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;
    w.update_chain_tip(BIRTHDAY + 10).unwrap();

    let mut blocks = encoded_blocks(BIRTHDAY..BIRTHDAY + 2, 0);
    blocks.extend(encoded_blocks(BIRTHDAY + 3..BIRTHDAY + 5, 0));
    let err = w.scan_blocks(tree_state(PRIOR, 0), blocks).unwrap_err();
    assert!(
        matches!(err, WalletError::ChainContinuity { at_height, .. } if at_height == BIRTHDAY + 3),
        "{err:?}"
    );
    assert_eq!(w.max_scanned_height().unwrap(), None);
}

#[test]
fn reorg_is_detected_and_rewound() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;
    let tip = BIRTHDAY + 29;

    w.update_chain_tip(tip).unwrap();
    scan(w, BIRTHDAY..tip + 1, 0).unwrap();
    assert!(w.suggest_scan_ranges().unwrap().is_empty());

    // The chain is replaced from `fork_height` on, and grows by one block.
    // The first replacement block still points at a fork 0 parent, but the
    // synthetic fork 1 block claims a fork 1 parent, so it does not connect.
    let fork_height = tip - 4;
    w.update_chain_tip(tip + 1).unwrap();
    let err = scan(w, tip + 1..tip + 2, 1).unwrap_err();
    assert!(
        matches!(err, WalletError::ChainContinuity { at_height, .. } if at_height == tip + 1),
        "{err:?}"
    );
    // The failed scan stored nothing.
    assert_eq!(w.max_scanned_height().unwrap(), Some(tip));

    let rewound = w.rewind_to_height(fork_height - 1).unwrap();
    assert_eq!(rewound, fork_height - 1);
    assert_eq!(w.max_scanned_height().unwrap(), Some(rewound));
    assert_eq!(w.block_hash(fork_height).unwrap(), None);
    assert_eq!(w.block_hash(rewound).unwrap(), Some(block_hash(rewound, 0)));

    // Re-tell the tip, then scan the replacement chain. Its first block names
    // the fork 0 block at the rewind height as its parent.
    w.update_chain_tip(tip + 1).unwrap();
    let ranges = w.suggest_scan_ranges().unwrap();
    assert_eq!(ranges.iter().map(|r| r.start).min(), Some(fork_height));

    let mut first = crate::testutil::empty_block(fork_height, 1);
    first.prev_hash = block_hash(rewound, 0);
    let mut blocks = vec![first.encode_to_vec()];
    blocks.extend(encoded_blocks(fork_height + 1..tip + 2, 1));
    w.scan_blocks(tree_state(rewound, 0), blocks).unwrap();

    assert_eq!(w.fully_scanned_height().unwrap(), Some(tip + 1));
    assert_eq!(w.block_hash(tip).unwrap(), Some(block_hash(tip, 1)));
    assert!(w.suggest_scan_ranges().unwrap().is_empty());
}

#[test]
fn verify_range_is_suggested_first_after_reopen() {
    let (fx, _) = wallet_with_account();
    let tip = BIRTHDAY + 29;
    fx.wallet.update_chain_tip(tip).unwrap();
    scan(&fx.wallet, BIRTHDAY..tip + 1, 0).unwrap();

    // The chain moves on while the wallet is closed. The wallet then asks for
    // its newest scanned blocks to be checked before anything else.
    let w = Wallet::open(fx.db_path(), regtest(), None).unwrap();
    w.update_chain_tip(tip + 200).unwrap();
    let ranges = w.suggest_scan_ranges().unwrap();
    assert!(!ranges.is_empty());
    assert!(
        ranges.windows(2).all(|p| p[0].priority >= p[1].priority),
        "{ranges:?}"
    );
    assert_eq!(ranges.iter().map(|r| r.end).max(), Some(tip + 201));
    assert!(ranges.iter().all(|r| r.priority > ScanPriority::Scanned));
}

#[test]
fn rewind_below_the_scanned_range_is_invalid() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;
    let tip = BIRTHDAY + 9;
    w.update_chain_tip(tip).unwrap();
    scan(w, BIRTHDAY..tip + 1, 0).unwrap();

    // There is no checkpoint below the birthday to rewind to.
    let err = w.rewind_to_height(PRIOR - 50).unwrap_err();
    assert!(
        matches!(
            err,
            WalletError::RewindInvalid {
                requested_height,
                ..
            } if requested_height == PRIOR - 50
        ),
        "{err:?}"
    );
    assert_eq!(w.max_scanned_height().unwrap(), Some(tip));

    // A rewind to the current height or above changes nothing.
    assert_eq!(w.rewind_to_height(tip + 100).unwrap(), tip);
    assert_eq!(w.max_scanned_height().unwrap(), Some(tip));
}

#[test]
fn checkpoints_are_kept_on_the_anchor_grid_inside_a_batch() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;
    let tip = BIRTHDAY + 59;
    w.update_chain_tip(tip).unwrap();
    // The first batch runs past two heights of the anchor grid. NU7 is active,
    // so the grid step is three times the retention interval of 12 blocks.
    scan(w, BIRTHDAY..150, 0).unwrap();
    scan(w, 150..tip + 1, 0).unwrap();

    // Above the newest checkpoint, which is the state the second batch
    // started from, a rewind is exact.
    assert_eq!(w.rewind_to_height(155).unwrap(), 155);
    // Below it, a rewind lands on a checkpoint. The first batch left one on
    // each grid height although it did not end there.
    assert_eq!(w.rewind_to_height(148).unwrap(), 144);
    // 132 is a multiple of the interval and not of the grid step.
    assert_eq!(w.rewind_to_height(143).unwrap(), 108);
    assert_eq!(w.max_scanned_height().unwrap(), Some(108));
}

#[test]
fn wallet_summary_arguments() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;
    assert_invalid(w.wallet_summary(Some(0)).unwrap_err(), "min_confirmations");
    // No chain tip yet, and then no scanned block yet.
    assert_eq!(w.wallet_summary(Some(1)).unwrap(), None);
    w.update_chain_tip(BIRTHDAY + 5).unwrap();
    assert_eq!(w.wallet_summary(Some(1)).unwrap(), None);
    scan(w, BIRTHDAY..BIRTHDAY + 3, 0).unwrap();
    let partial = w.wallet_summary(Some(1)).unwrap().unwrap();
    assert_eq!(partial.chain_tip_height, BIRTHDAY + 5);
    assert_eq!(partial.fully_scanned_height, BIRTHDAY + 2);
    assert!(w.wallet_summary(Some(100)).unwrap().is_some());
}

#[test]
fn recovery_progress_is_reported_for_a_restored_account() {
    let fx = empty_wallet();
    let w = &fx.wallet;
    w.create_account(
        "restored".into(),
        SEED.to_vec(),
        tree_state(PRIOR, 0),
        Some(BIRTHDAY + 50),
        None,
    )
    .unwrap();
    w.update_chain_tip(BIRTHDAY + 60).unwrap();
    scan(w, BIRTHDAY..BIRTHDAY + 10, 0).unwrap();
    let summary = w.wallet_summary(None).unwrap().unwrap();
    assert!(summary.recovery_progress.is_some(), "{summary:?}");
}

#[test]
fn subtree_roots() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;
    let root = |height: u32| {
        // A small integer is a valid node of the Ironwood tree. The wallet
        // requires distinct roots within a tree.
        let mut root_hash = vec![0; 32];
        root_hash[0] = height as u8;
        SubtreeRoot {
            root_hash,
            completing_block_height: height,
        }
    };

    // An empty list is a no-op.
    w.put_subtree_roots(0, vec![]).unwrap();

    w.put_subtree_roots(0, vec![root(10), root(20), root(21), root(22)])
        .unwrap();

    w.update_chain_tip(BIRTHDAY + 5).unwrap();
    scan(w, BIRTHDAY..BIRTHDAY + 6, 0).unwrap();
    let summary = w.wallet_summary(None).unwrap().unwrap();
    // The next request overlaps the last root the wallet holds.
    assert_eq!(summary.next_subtree_index, 2);

    // Roots can be stored again, and extended from the reported index.
    w.put_subtree_roots(2, vec![root(21), root(22), root(23)])
        .unwrap();
    let summary = w.wallet_summary(None).unwrap().unwrap();
    assert_eq!(summary.next_subtree_index, 3);

    let short = SubtreeRoot {
        root_hash: vec![0; 31],
        completing_block_height: 10,
    };
    assert_invalid(
        w.put_subtree_roots(5, vec![root(50), short]).unwrap_err(),
        "subtree root 1",
    );
    // A root that is not a canonical field element.
    let non_canonical = SubtreeRoot {
        root_hash: vec![0xff; 32],
        completing_block_height: 10,
    };
    assert_invalid(
        w.put_subtree_roots(3, vec![non_canonical]).unwrap_err(),
        "subtree root 0",
    );
}

/// The script that pays `address`.
fn script_for(address: &str) -> Vec<u8> {
    let params = regtest().params().unwrap();
    match Address::decode(&params, address) {
        Some(Address::Transparent(addr)) => script::Evaluable::to_bytes(&addr.script()),
        other => panic!("not a transparent address: {other:?}"),
    }
}

#[test]
fn transparent_outputs() {
    let (fx, account) = wallet_with_account();
    let w = &fx.wallet;
    let tip = BIRTHDAY + 9;
    w.update_chain_tip(tip).unwrap();
    scan(w, BIRTHDAY..tip + 1, 0).unwrap();

    let address = w.transparent_receivers(account.uuid.clone()).unwrap()[0]
        .address
        .clone();
    let query_height = w.utxo_query_height(account.uuid.clone()).unwrap();
    assert!(query_height <= tip, "{query_height}");

    let utxo = Utxo {
        txid: vec![3; 32],
        index: 1,
        script: script_for(&address),
        value_zat: 250_000,
        height: BIRTHDAY + 2,
    };
    w.put_utxo(utxo.clone()).unwrap();
    // Storing the same output again replaces it.
    w.put_utxo(utxo.clone()).unwrap();

    let summary = w.wallet_summary(Some(1)).unwrap().unwrap();
    let balance = &summary.account_balances[0];
    assert_eq!(balance.transparent.total, 250_000, "{balance:?}");
    assert_eq!(balance.total, 250_000);
    assert_eq!(balance.ironwood.total, 0);

    // The wallet now wants to know more about the funding transaction.
    let requests = w.transaction_data_requests().unwrap();
    assert!(
        requests.iter().any(|r| matches!(
            r,
            TransactionDataRequest::GetStatus { txid }
            | TransactionDataRequest::Enhancement { txid } if *txid == utxo.txid
        ) || matches!(
            r,
            TransactionDataRequest::TransactionsInvolvingAddress { .. }
        )),
        "{requests:?}"
    );
    for request in &requests {
        if let TransactionDataRequest::TransactionsInvolvingAddress {
            address,
            block_range_start,
            block_range_end,
            ..
        } = request
        {
            assert!(address.starts_with("tm"), "{address}");
            if let Some(end) = block_range_end {
                assert!(block_range_start <= end, "{request:?}");
            }
        }
    }

    assert_invalid(
        w.put_utxo(Utxo {
            txid: vec![3; 31],
            ..utxo.clone()
        })
        .unwrap_err(),
        "transaction ID is 32 bytes",
    );
    assert_invalid(
        w.put_utxo(Utxo {
            value_zat: u64::MAX,
            ..utxo.clone()
        })
        .unwrap_err(),
        "value is out of range",
    );
    assert_invalid(
        w.put_utxo(Utxo {
            script: vec![0x6a],
            ..utxo.clone()
        })
        .unwrap_err(),
        "does not pay a transparent address",
    );

    // An output paying an address that is not the wallet's.
    let foreign = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm";
    let err = w
        .put_utxo(Utxo {
            txid: vec![4; 32],
            script: script_for(foreign),
            ..utxo
        })
        .unwrap_err();
    assert!(matches!(err, WalletError::Database { .. }), "{err:?}");
}

#[test]
fn transaction_status() {
    let (fx, account) = wallet_with_account();
    let w = &fx.wallet;
    let tip = BIRTHDAY + 9;
    w.update_chain_tip(tip).unwrap();
    scan(w, BIRTHDAY..tip + 1, 0).unwrap();

    assert_invalid(
        w.set_transaction_status(vec![1; 5], TransactionStatus::NotInMainChain)
            .unwrap_err(),
        "transaction ID is 32 bytes",
    );

    // Statuses for a transaction the wallet has never seen are accepted and
    // ignored.
    let unknown = vec![9; 32];
    w.set_transaction_status(unknown.clone(), TransactionStatus::TxidNotRecognized)
        .unwrap();
    w.set_transaction_status(unknown.clone(), TransactionStatus::NotInMainChain)
        .unwrap();
    w.set_transaction_status(unknown, TransactionStatus::Mined { height: tip })
        .unwrap();

    // Reporting the height of a known transaction answers the wallet's
    // status request for it.
    let address = w.transparent_receivers(account.uuid).unwrap()[0]
        .address
        .clone();
    let txid = vec![5; 32];
    w.put_utxo(Utxo {
        txid: txid.clone(),
        index: 0,
        script: script_for(&address),
        value_zat: 1_000,
        height: BIRTHDAY + 1,
    })
    .unwrap();
    let wants_status = |w: &Wallet| {
        w.transaction_data_requests()
            .unwrap()
            .iter()
            .any(|r| matches!(r, TransactionDataRequest::GetStatus { txid: t } if *t == txid))
    };
    w.set_transaction_status(
        txid.clone(),
        TransactionStatus::Mined {
            height: BIRTHDAY + 1,
        },
    )
    .unwrap();
    assert!(!wants_status(w));
}

/// A serialized transaction with no inputs and no outputs.
fn empty_transaction(expiry_height: u32) -> (Vec<u8>, Vec<u8>) {
    let tx = TransactionData::<Authorized>::from_parts(
        TxVersion::V5,
        BranchId::Nu5,
        0,
        BlockHeight::from_u32(expiry_height),
        None,
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

/// A serialized transaction that spends the given outputs and pays
/// `value_zat` to each of the given scripts. The inputs carry no signatures:
/// the wallet stores transactions without verifying them. It does compute
/// the fee, and rejects a transaction that pays out more than its inputs
/// hold, which one without inputs does. An input the wallet does not know
/// leaves the fee unknown.
fn transparent_transaction(
    spends: &[(Vec<u8>, u32)],
    pays: &[Vec<u8>],
    value_zat: u64,
) -> (Vec<u8>, Vec<u8>) {
    use transparent::bundle::{Authorized as TransparentAuthorized, Bundle, TxIn};

    let bundle = Bundle::<TransparentAuthorized> {
        vin: spends
            .iter()
            .map(|(txid, index)| {
                let txid: [u8; 32] = txid.as_slice().try_into().unwrap();
                TxIn::from_parts(
                    OutPoint::new(txid, *index),
                    Script(script::Code(vec![])),
                    u32::MAX,
                )
            })
            .collect(),
        vout: pays
            .iter()
            .map(|s| {
                TxOut::new(
                    Zatoshis::from_u64(value_zat).unwrap(),
                    Script(script::Code(s.clone())),
                )
            })
            .collect(),
        authorization: TransparentAuthorized,
    };
    let tx = TransactionData::<Authorized>::from_parts(
        TxVersion::V5,
        BranchId::Nu5,
        0,
        BlockHeight::from_u32(0),
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

/// The indexes of the receivers on one chain.
fn indexes(receivers: &[WatchedAddress], change: bool) -> Vec<u32> {
    receivers
        .iter()
        .filter(|r| r.change == change)
        .map(|r| r.index)
        .collect()
}

/// The indexes of the unused receivers on one chain.
fn unused(receivers: &[WatchedAddress], change: bool) -> Vec<u32> {
    receivers
        .iter()
        .filter(|r| r.change == change && r.unused)
        .map(|r| r.index)
        .collect()
}

#[test]
fn transparent_window_follows_stored_history() {
    let (fx, account) = wallet_with_account();
    let w = &fx.wallet;

    // A new account watches one gap of unused addresses on each chain,
    // listed by chain and index.
    let receivers = w.transparent_receivers(account.uuid.clone()).unwrap();
    assert_eq!(indexes(&receivers, false), (0..10).collect::<Vec<_>>());
    assert_eq!(indexes(&receivers, true), (0..5).collect::<Vec<_>>());
    assert_eq!(unused(&receivers, false), (0..10).collect::<Vec<_>>());
    assert_eq!(unused(&receivers, true), (0..5).collect::<Vec<_>>());
    assert_eq!(
        receivers.iter().map(|r| r.change).collect::<Vec<_>>(),
        [vec![false; 10], vec![true; 5]].concat()
    );

    // The history of the last watched address for receiving: a transaction
    // that paid it and one that spent the output. Nothing of it is unspent,
    // and storing it still counts the address as used.
    let last = receivers[9].address.clone();
    let elsewhere = (vec![0xaa; 32], 0);
    let (paying, paying_txid) = transparent_transaction(
        std::slice::from_ref(&elsewhere),
        &[script_for(&last)],
        70_000,
    );
    assert_eq!(
        w.decrypt_and_store_transaction(paying, Some(BIRTHDAY + 5))
            .unwrap(),
        paying_txid
    );
    let (spending, _) = transparent_transaction(&[(paying_txid, 0)], &[vec![0x51]], 60_000);
    w.decrypt_and_store_transaction(spending, Some(BIRTHDAY + 6))
        .unwrap();

    let receivers = w.transparent_receivers(account.uuid.clone()).unwrap();
    assert_eq!(indexes(&receivers, false), (0..20).collect::<Vec<_>>());
    assert_eq!(indexes(&receivers, true), (0..5).collect::<Vec<_>>());
    // The addresses up to the used one are no longer unused.
    assert_eq!(unused(&receivers, false), (10..20).collect::<Vec<_>>());
    assert_eq!(unused(&receivers, true), (0..5).collect::<Vec<_>>());

    // The same on the change chain.
    let change = receivers.iter().find(|r| r.change && r.index == 2).unwrap();
    let (paying, _) = transparent_transaction(&[elsewhere], &[script_for(&change.address)], 5_000);
    w.decrypt_and_store_transaction(paying, Some(BIRTHDAY + 7))
        .unwrap();
    let receivers = w.transparent_receivers(account.uuid).unwrap();
    assert_eq!(indexes(&receivers, true), (0..8).collect::<Vec<_>>());
    assert_eq!(unused(&receivers, true), (3..8).collect::<Vec<_>>());
    assert_eq!(unused(&receivers, false), (10..20).collect::<Vec<_>>());
}

/// The default address of an account can lie beyond the first gap of
/// unused addresses. The account then watches more unused addresses on the
/// receive chain than the gap limit, and every one of them is reported as
/// unused.
#[test]
fn unused_addresses_beyond_the_gap() {
    let fx = empty_wallet();
    let w = &fx.wallet;
    let account = w
        .create_account(
            "no sapling".into(),
            NO_SAPLING_SEED.to_vec(),
            tree_state(PRIOR, 0),
            None,
            None,
        )
        .unwrap();

    let receivers = w.transparent_receivers(account.uuid.clone()).unwrap();
    assert_eq!(indexes(&receivers, false), (0..11).collect::<Vec<_>>());
    assert_eq!(unused(&receivers, false), (0..11).collect::<Vec<_>>());
    assert_eq!(unused(&receivers, true), (0..5).collect::<Vec<_>>());

    // A use of the first address leaves the ones after it unused, and the
    // gap now reaches as far as the default address.
    let elsewhere = (vec![0xaa; 32], 0);
    let (paying, _) =
        transparent_transaction(&[elsewhere], &[script_for(&receivers[0].address)], 70_000);
    w.decrypt_and_store_transaction(paying, Some(BIRTHDAY + 5))
        .unwrap();
    let receivers = w.transparent_receivers(account.uuid).unwrap();
    assert_eq!(indexes(&receivers, false), (0..11).collect::<Vec<_>>());
    assert_eq!(unused(&receivers, false), (1..11).collect::<Vec<_>>());
}

#[test]
fn decrypt_and_store_transaction() {
    let (fx, _) = wallet_with_account();
    let w = &fx.wallet;

    assert_invalid(
        w.decrypt_and_store_transaction(vec![1, 2, 3], None)
            .unwrap_err(),
        "transaction:",
    );

    // Without a chain tip, and then with one, for both a mined and an unmined
    // transaction. The transaction has nothing for the wallet, so storing it
    // only has to succeed and report the right ID.
    let (bytes, txid) = empty_transaction(BIRTHDAY + 40);
    assert_eq!(
        w.decrypt_and_store_transaction(bytes.clone(), None)
            .unwrap(),
        txid
    );
    w.update_chain_tip(BIRTHDAY + 9).unwrap();
    assert_eq!(
        w.decrypt_and_store_transaction(bytes.clone(), None)
            .unwrap(),
        txid
    );
    assert_eq!(
        w.decrypt_and_store_transaction(bytes, Some(BIRTHDAY + 3))
            .unwrap(),
        txid
    );
}

#[test]
fn a_poisoned_lock_does_not_disable_the_wallet() {
    let (fx, account) = wallet_with_account();
    let wallet = Arc::clone(&fx.wallet);
    let res = std::thread::spawn(move || {
        let _held = wallet.db.lock().unwrap();
        panic!("poison the lock");
    })
    .join();
    assert!(res.is_err());
    assert!(fx.wallet.db.is_poisoned());

    assert_eq!(fx.wallet.accounts().unwrap(), vec![account]);
    fx.wallet.update_chain_tip(BIRTHDAY + 3).unwrap();
}

#[test]
fn concurrent_calls_are_serialized() {
    let (fx, account) = wallet_with_account();
    let tip = BIRTHDAY + 199;
    fx.wallet.update_chain_tip(tip).unwrap();

    let scanner = {
        let wallet = Arc::clone(&fx.wallet);
        std::thread::spawn(move || {
            for start in (BIRTHDAY..tip + 1).step_by(20) {
                scan(&wallet, start..start + 20, 0).unwrap();
            }
        })
    };
    let readers: Vec<_> = (0..4)
        .map(|_| {
            let wallet = Arc::clone(&fx.wallet);
            let uuid = account.uuid.clone();
            std::thread::spawn(move || {
                for _ in 0..50 {
                    wallet.wallet_summary(None).unwrap();
                    wallet.current_address(uuid.clone()).unwrap();
                    wallet.suggest_scan_ranges().unwrap();
                }
            })
        })
        .collect();

    scanner.join().unwrap();
    for reader in readers {
        reader.join().unwrap();
    }
    assert_eq!(fx.wallet.fully_scanned_height().unwrap(), Some(tip));
}

#[test]
fn address_checks() {
    let (fx, account) = wallet_with_account();
    let w = &fx.wallet;
    let tip = BIRTHDAY + 9;
    w.update_chain_tip(tip).unwrap();
    scan(w, BIRTHDAY..tip + 1, 0).unwrap();

    // A new wallet asks for the transactions of each of its ephemeral
    // addresses, with no upper bound and no scheduled time.
    let requests = w.transaction_data_requests().unwrap();
    assert!(!requests.is_empty());
    let mut ephemeral = vec![];
    for request in &requests {
        let TransactionDataRequest::TransactionsInvolvingAddress {
            address,
            block_range_end,
            request_at,
            ..
        } = request
        else {
            panic!("unexpected request {request:?}");
        };
        assert_eq!((*block_range_end, *request_at), (None, None), "{request:?}");
        ephemeral.push(address.clone());
    }

    // Closing an open-ended request accepts the height that was checked.
    w.notify_address_checked(requests[0].clone(), tip).unwrap();

    // Scheduling the next check stamps the address's following request.
    let at = w
        .schedule_next_check(ephemeral[0].clone(), 600)
        .unwrap()
        .unwrap();
    let rescheduled = w
        .transaction_data_requests()
        .unwrap()
        .into_iter()
        .find_map(|r| match r {
            TransactionDataRequest::TransactionsInvolvingAddress {
                address,
                request_at,
                ..
            } if address == ephemeral[0] => Some(request_at),
            _ => None,
        })
        .unwrap();
    assert_eq!(rescheduled, Some(at));

    // For a bounded request, the reported height must be the last block of
    // the range.
    let address = w.transparent_receivers(account.uuid).unwrap()[0]
        .address
        .clone();
    let bounded = TransactionDataRequest::TransactionsInvolvingAddress {
        address,
        block_range_start: BIRTHDAY,
        block_range_end: Some(tip + 1),
        request_at: None,
        tx_status_filter: crate::TransactionStatusFilter::Mined,
        output_status_filter: crate::OutputStatusFilter::All,
    };
    let err = w
        .notify_address_checked(bounded.clone(), tip - 1)
        .unwrap_err();
    assert!(matches!(err, WalletError::Database { .. }), "{err:?}");
    w.notify_address_checked(bounded, tip).unwrap();

    assert_invalid(
        w.notify_address_checked(TransactionDataRequest::GetStatus { txid: vec![6; 32] }, tip)
            .unwrap_err(),
        "not a request for the transactions of an address",
    );
    assert_invalid(
        w.notify_address_checked(
            TransactionDataRequest::TransactionsInvolvingAddress {
                address: "nope".into(),
                block_range_start: 1,
                block_range_end: None,
                request_at: Some(1_700_000_000),
                tx_status_filter: crate::TransactionStatusFilter::All,
                output_status_filter: crate::OutputStatusFilter::Unspent,
            },
            tip,
        )
        .unwrap_err(),
        "not a transparent address",
    );
}

#[test]
fn next_check_scheduling() {
    let (fx, account) = wallet_with_account();
    let w = &fx.wallet;
    let address = w.transparent_receivers(account.uuid).unwrap()[0]
        .address
        .clone();

    let now = std::time::SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let at = w.schedule_next_check(address, 600).unwrap().unwrap();
    assert!(at >= now, "scheduled at {at}, now {now}");

    // An address the wallet does not hold.
    assert_eq!(
        w.schedule_next_check("tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm".into(), 600)
            .unwrap(),
        None
    );
    assert_invalid(
        w.schedule_next_check("nope".into(), 600).unwrap_err(),
        "not a transparent address",
    );
}
