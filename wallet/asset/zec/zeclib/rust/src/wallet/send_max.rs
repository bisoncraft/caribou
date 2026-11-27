// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Sending everything an account can spend in one transaction.
//!
//! librustzcash's `propose_send_max_transfer` spends shielded notes only,
//! and an ordinary proposal needs an amount, which a fee that depends on the
//! inputs makes unknowable in advance. So the proposal is built here from the
//! inputs themselves: the eligible inputs, as the selector's own queries list
//! them, capped to what one transaction takes; the ZIP 317 fee of exactly
//! those inputs and the one payment; no change; and an amount of the inputs
//! less the fee.

use std::collections::BTreeMap;
use std::convert::Infallible;

use nonempty::NonEmpty;
use zcash_client_backend::{
    data_api::{
        NoteRetention, ReceivedNotes, WalletRead,
        wallet::{SpendingKeys, create_proposed_transactions},
    },
    fees::{StandardFeeRule, TransactionBalance},
    proposal::{Proposal, ShieldedInputs},
    wallet::{Note, OvkPolicy, ReceivedNote},
};
use zcash_client_sqlite::{AccountUuid, ReceivedNoteId, util::SystemClock};
use zcash_primitives::transaction::fees::{
    FeeRule,
    transparent::{InputSize, InputView},
};
use zcash_protocol::{PoolType, consensus::BlockHeight, memo::MemoBytes, value::Zatoshis};
use zip321::{Payment, TransactionRequest};

use super::{
    Db, Wallet, confirmations_policy, parse_account, secret_seed,
    send::{
        MAX_IRONWOOD_ACTIONS, NoSaplingProver, SendResult, check_proposal, eligible_inputs,
        ironwood_bundle_version, parse_memo, send_error,
    },
};
use crate::{
    error::{WalletError, guard},
    recipient::{PaidPool, Recipient, resolve_recipient},
};

/// The most transparent inputs one transaction spends: the cap that input
/// selection applies by default, 10% of the 2,000,000 bytes of a block at 150
/// bytes per P2PKH input (`shielding_max_inputs` with
/// `DEFAULT_SHIELDING_BLOCK_SPACE_PERCENT` in zcash_client_backend). It is the
/// selector's choice, not a rule of the chain.
pub(crate) const TRANSPARENT_INPUT_CAP: usize = 1_333;

/// Spendable funds that one send max transaction could not take, because of
/// the limits on its inputs. Another transaction can send them now.
#[derive(Debug, Default, PartialEq, Eq, uniffi::Record)]
pub struct LeftBehind {
    /// The number and value, in zatoshis, of the spendable transparent
    /// outputs beyond the 1,333 one transaction spends. The smallest ones are
    /// left.
    pub transparent_count: u64,
    pub transparent_value: u64,
    /// The number and value of the spendable Ironwood notes beyond what the
    /// wallet's limit of Ironwood actions allows. The smallest ones are left.
    pub note_count: u64,
    pub note_value: u64,
}

/// The result of [`Wallet::estimate_send_max`].
#[derive(Debug, PartialEq, Eq, uniffi::Record)]
pub struct SendMaxEstimate {
    /// The amount the recipient would get and the fee, in zatoshis.
    pub amount: u64,
    pub fee: u64,
    /// The address the payment would name and the pool it would land in.
    pub paid_address: String,
    pub paid_pool: PaidPool,
    pub left_behind: LeftBehind,
}

/// The result of [`Wallet::send_max`].
#[derive(Debug, uniffi::Record)]
pub struct SendMaxResult {
    /// The created transactions, the fee and the paid address, as for a
    /// send.
    pub result: SendResult,
    /// The amount the recipient gets, in zatoshis.
    pub amount: u64,
    pub left_behind: LeftBehind,
}

/// Keeps the Ironwood notes of a [`ReceivedNotes`] and drops the others.
struct IronwoodOnly;

impl NoteRetention<ReceivedNoteId> for IronwoodOnly {
    fn should_retain_sapling(&self, _: &ReceivedNote<ReceivedNoteId, sapling::Note>) -> bool {
        false
    }
    fn should_retain_orchard(&self, _: &ReceivedNote<ReceivedNoteId, orchard::Note>) -> bool {
        false
    }
    fn should_retain_ironwood(&self, _: &ReceivedNote<ReceivedNoteId, orchard::Note>) -> bool {
        true
    }
}

/// Sorts `items` by value, largest first, keeping the given order among equal
/// values, and splits off all but the first `keep`. Returns the kept items
/// and the count and value of the rest.
pub(crate) fn keep_largest<T>(
    mut items: Vec<T>,
    value: impl Fn(&T) -> Zatoshis,
    keep: usize,
) -> Result<(Vec<T>, u64, Zatoshis), WalletError> {
    items.sort_by_key(|item| std::cmp::Reverse(value(item)));
    let rest = items.split_off(keep.min(items.len()));
    let rest_value = sum(rest.iter().map(&value))?;
    Ok((items, rest.len() as u64, rest_value))
}

fn sum(mut values: impl Iterator<Item = Zatoshis>) -> Result<Zatoshis, WalletError> {
    values.try_fold(Zatoshis::ZERO, |acc, v| {
        (acc + v).ok_or_else(|| WalletError::Send {
            message: "the input values overflow".into(),
        })
    })
}

/// A transparent output as a proposal carries it: without the account it
/// belongs to, as `redact_account_data` in zcash_client_backend makes it.
fn proposal_input(
    o: &zcash_client_backend::wallet::WalletTransparentOutput<AccountUuid>,
) -> zcash_client_backend::wallet::WalletTransparentOutput<()> {
    let input = zcash_client_backend::wallet::WalletTransparentOutput::from_parts(
        o.outpoint().clone(),
        o.txout().clone(),
        o.mined_height(),
        o.recipient_account().map(|_| ()),
        o.recipient_key_scope(),
        None,
    )
    .expect("the output was decoded with a recipient address");
    match o.serialized_size() {
        InputSize::Known(size) => input.with_known_input_size(size),
        InputSize::Unknown(_) => input,
    }
}

/// A send max proposal and what it leaves behind.
struct Built {
    proposal: Proposal<StandardFeeRule, ReceivedNoteId>,
    amount: Zatoshis,
    left_behind: LeftBehind,
}

impl Wallet {
    /// Builds the proposal that sends everything one transaction can take
    /// from the account to `recipient`.
    fn build_send_max(
        &self,
        db: &Db,
        uuid: AccountUuid,
        recipient: &Recipient,
        memo: Option<MemoBytes>,
        min_confirmations: Option<u32>,
    ) -> Result<Built, WalletError> {
        let policy = confirmations_policy(min_confirmations)?;
        let eligible =
            eligible_inputs(db, uuid, policy)?.ok_or_else(|| WalletError::NotSynced {
                message: "the wallet has no chain tip or no scanned anchor".into(),
            })?;
        let target = BlockHeight::from(eligible.target);
        let version = ironwood_bundle_version(&self.params, target)?;
        let pays_ironwood = recipient.pool == PaidPool::Ironwood;
        let ironwood_actions = |spends: usize| -> Result<usize, WalletError> {
            if spends == 0 && !pays_ironwood {
                // No Ironwood bundle at all.
                return Ok(0);
            }
            orchard::builder::BundleType::DEFAULT
                .num_actions(version.default_flags(), spends, usize::from(pays_ironwood))
                .map_err(|e| WalletError::Send {
                    message: format!("counting the Ironwood actions: {e}"),
                })
        };

        // The transparent outputs, largest first, up to the input cap.
        let (transparent, transparent_count, transparent_value) =
            keep_largest(eligible.transparent, |o| o.value(), TRANSPARENT_INPUT_CAP)?;

        // The notes, largest first, as many as the action limit allows.
        let notes: Vec<ReceivedNote<ReceivedNoteId, Note>> =
            ReceivedNotes::new(vec![], vec![], eligible.notes.take_ironwood())
                .into_vec(&IronwoodOnly);
        let mut spends = notes.len().min(MAX_IRONWOOD_ACTIONS);
        while spends > 0 && ironwood_actions(spends)? > MAX_IRONWOOD_ACTIONS {
            spends -= 1;
        }
        let (notes, note_count, note_value) = keep_largest(notes, |n| n.note().value(), spends)?;
        let actions = ironwood_actions(notes.len())?;

        let fee = StandardFeeRule::Zip317
            .fee_required(
                &self.params,
                target,
                transparent.iter().map(|o| o.serialized_size()),
                recipient.transparent_output_size,
                0,
                0,
                0,
                actions,
            )
            .map_err(|e| WalletError::Send {
                message: format!("the fee: {e}"),
            })?;
        let total = (sum(transparent.iter().map(|o| o.value()))?
            + sum(notes.iter().map(|n| n.note().value()))?)
        .ok_or_else(|| WalletError::Send {
            message: "the input values overflow".into(),
        })?;
        // The payment has to be worth sending: at least one zatoshi, and for
        // a transparent receiver the smallest output nodes relay.
        let least = Zatoshis::from_u64(recipient.min_amount().max(1))
            .expect("the minimum is a small amount");
        let required = (fee + least).ok_or_else(|| WalletError::Send {
            message: "the fee overflows".into(),
        })?;
        if total < required {
            return Err(WalletError::InsufficientFunds {
                available: total.into_u64(),
                required: required.into_u64(),
            });
        }
        let amount = (total - fee).expect("the total covers the fee");

        let payment = Payment::new(
            recipient.address.clone(),
            Some(amount),
            memo,
            None,
            None,
            vec![],
        )
        .map_err(|e| WalletError::invalid(format!("payment: {e}")))?;
        let request = TransactionRequest::new(vec![payment])
            .map_err(|e| WalletError::invalid(format!("payment request: {e}")))?;
        let pool = match recipient.pool {
            PaidPool::Ironwood => PoolType::IRONWOOD,
            PaidPool::Transparent => PoolType::TRANSPARENT,
        };
        let balance = TransactionBalance::new(vec![], fee).map_err(|e| WalletError::Send {
            message: format!("the balance: {e:?}"),
        })?;
        let proposal = Proposal::single_step(
            request,
            BTreeMap::from([(0, pool)]),
            transparent.iter().map(proposal_input).collect(),
            NonEmpty::from_vec(notes).map(ShieldedInputs::from_parts),
            // An Ironwood bundle needs an anchor also when it only has the
            // payment's output.
            eligible.anchor,
            balance,
            StandardFeeRule::Zip317,
            eligible.target,
            policy,
            false,
            // Checked above by ironwood_bundle_version.
            true,
        )
        .map_err(|e| WalletError::Send {
            message: format!("the proposal: {e}"),
        })?;
        Ok(Built {
            proposal,
            amount,
            left_behind: LeftBehind {
                transparent_count,
                transparent_value: transparent_value.into_u64(),
                note_count,
                note_value: note_value.into_u64(),
            },
        })
    }
}

#[uniffi::export]
impl Wallet {
    /// Returns what [`send_max`](Wallet::send_max) would send to `address`:
    /// the amount, the fee, the paid address and pool, and the spendable
    /// funds one transaction could not take. It builds and validates the
    /// proposal without creating it: nothing is stored or locked. It checks
    /// neither a seed nor a fee limit nor an expected amount, which it does
    /// not take.
    pub fn estimate_send_max(
        &self,
        account_uuid: String,
        address: String,
        memo: Option<Vec<u8>>,
        min_confirmations: Option<u32>,
    ) -> Result<SendMaxEstimate, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let recipient = resolve_recipient(&self.params, &address)?;
            let memo = parse_memo(memo, &recipient)?;
            let db = self.db();
            if db.get_account(uuid)?.is_none() {
                return Err(WalletError::AccountNotFound);
            }
            let built = self.build_send_max(&db, uuid, &recipient, memo, min_confirmations)?;
            let fee = check_proposal(&self.params, &built.proposal, &recipient, None)?;
            Ok(SendMaxEstimate {
                amount: built.amount.into_u64(),
                fee,
                paid_address: recipient.address.encode(),
                paid_pool: recipient.pool,
                left_behind: built.left_behind,
            })
        })
    }

    /// Sends everything the account can spend in one transaction to
    /// `address`, and stores the transaction as unmined. Nothing is
    /// broadcast.
    ///
    /// The inputs are every input a [`send`](Wallet::send) could select: the
    /// account's Ironwood notes and its transparent outputs above 5,000
    /// zatoshis that are not coinbase outputs, under the confirmations
    /// policy of `min_confirmations`. One transaction spends at most 1,333
    /// transparent outputs and as many notes as [`MAX_IRONWOOD_ACTIONS`]
    /// allows, the largest of each. The rest is reported in the result as
    /// left behind. Those funds stay spendable by a later transaction,
    /// although on their own they may not cover its fee. The transaction has
    /// no change: the recipient gets the inputs less the fee.
    ///
    /// `expected_amount`, when set, is the amount the caller approved, such
    /// as the amount of an earlier [`estimate_send_max`](Wallet::estimate_send_max).
    /// The inputs can change in between, and if the amount differs the call
    /// fails with [`WalletError::AmountChanged`] and creates nothing.
    ///
    /// An input that becomes spendable after the estimate raises the fee and
    /// the amount together, and the fee limit is checked first, so
    /// [`WalletError::FeeTooHigh`] can mean the same thing. For either error
    /// the remedy is a new estimate.
    ///
    /// It fails with [`WalletError::InsufficientFunds`] when the inputs do
    /// not cover the fee and a payment of at least one zatoshi, or for a
    /// transparent recipient the smallest output nodes relay. The other
    /// arguments, the checks and the errors are those of `send`.
    #[allow(clippy::too_many_arguments)]
    pub fn send_max(
        &self,
        account_uuid: String,
        seed: Vec<u8>,
        address: String,
        memo: Option<Vec<u8>>,
        max_fee: u64,
        expected_amount: Option<u64>,
        min_confirmations: Option<u32>,
    ) -> Result<SendMaxResult, WalletError> {
        guard(|| {
            let seed = secret_seed(seed)?;
            let uuid = parse_account(&account_uuid)?;
            let recipient = resolve_recipient(&self.params, &address)?;
            let memo = parse_memo(memo, &recipient)?;
            let mut db = self.db();
            let usk = self.spending_key(&db, uuid, &seed)?;
            drop(seed);
            let built = self.build_send_max(&db, uuid, &recipient, memo, min_confirmations)?;
            let fee = check_proposal(&self.params, &built.proposal, &recipient, Some(max_fee))?;
            if let Some(expected) = expected_amount
                && expected != built.amount.into_u64()
            {
                return Err(WalletError::AmountChanged {
                    expected,
                    actual: built.amount.into_u64(),
                });
            }
            let txids = create_proposed_transactions::<_, _, Infallible, _, Infallible, _>(
                &mut *db,
                &self.params,
                &SystemClock,
                &mut rand::rand_core::UnwrapErr(rand::rngs::SysRng),
                &NoSaplingProver,
                &NoSaplingProver,
                &SpendingKeys::from_unified_spending_key(usk),
                OvkPolicy::Sender,
                &built.proposal,
                None,
            )
            .map_err(send_error)?;
            Ok(SendMaxResult {
                result: self.created(&db, txids.into(), fee, &recipient),
                amount: built.amount.into_u64(),
                left_behind: built.left_behind,
            })
        })
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use tempfile::TempDir;
    use transparent::address::TransparentAddress;
    use zcash_keys::address::{Address, UnifiedAddress};
    use zcash_primitives::transaction::Transaction;
    use zcash_protocol::consensus::BranchId;

    use super::*;
    use crate::{
        Account,
        testutil::{encoded_blocks, regtest, tree_state},
        types::Utxo,
    };

    const SEED: [u8; 32] = [8; 32];
    const OTHER_SEED: [u8; 32] = [9; 32];
    const TIP: u32 = 120;
    // Account 0 of the alpha funder of the simnet harness and its transparent
    // receiver: recipients outside the test wallet.
    const ALPHA_UA: &str = "uregtest13pvma5dl78lka0cl83u4jmm78emspd6rm9e56xgm3ttk4pxcwcr4guhdc5u8mg7z3fyr9zjec593khe5m4c3a825yx22lcjwm4anp2xvu4ej5frpc5dm25294c8w3x0pnnxjde38tz4fpgaglrrhx0k7z5259slahnxng23mhn8lhhff8dkeznyhynrfwlu3e8mzedknqathcdlv7hd";
    const ALPHA_TRANSPARENT: &str = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm";

    /// A wallet with one account, scanned to [`TIP`] over empty blocks, that
    /// holds transparent outputs of the given values at its first receive
    /// address, mined at height 110.
    struct Funded {
        _dir: TempDir,
        w: Arc<Wallet>,
        account: Account,
        script: Vec<u8>,
    }

    fn funded(values: &[u64]) -> Funded {
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
        for (i, value) in values.iter().enumerate() {
            let mut txid = [0u8; 32];
            txid[..8].copy_from_slice(&(i as u64 + 1).to_le_bytes());
            w.put_utxo(Utxo {
                txid: txid.to_vec(),
                index: 0,
                script: script.clone(),
                value_zat: *value,
                height: 110,
            })
            .unwrap();
        }
        Funded {
            _dir: dir,
            w,
            account,
            script,
        }
    }

    impl Funded {
        fn uuid(&self) -> String {
            self.account.uuid.clone()
        }

        fn spendable(&self) -> u64 {
            let v = self.w.spendable(self.uuid(), None).unwrap();
            assert_eq!(v.ironwood, 0);
            v.transparent
        }

        fn summary_transparent(&self) -> crate::types::PoolBalance {
            self.w
                .wallet_summary(None)
                .unwrap()
                .unwrap()
                .account_balances[0]
                .transparent
        }

        /// Asserts that nothing was created: no transaction to broadcast and
        /// every output still spendable.
        fn assert_untouched(&self, spendable: u64) {
            assert!(self.w.rebroadcast_inventory().unwrap().is_empty());
            assert_eq!(self.spendable(), spendable);
        }
    }

    fn decode(raw: &[u8]) -> Transaction {
        let params = regtest().params().unwrap();
        Transaction::read(
            raw,
            BranchId::for_height(&params, BlockHeight::from_u32(TIP + 1)),
        )
        .unwrap()
    }

    #[test]
    fn keep_largest_splits_off_the_smallest() {
        let z = |v| Zatoshis::from_u64(v).unwrap();
        let (kept, n, value) =
            keep_largest(vec![(1, 5), (2, 9), (3, 5), (4, 7)], |x| z(x.1), 2).unwrap();
        assert_eq!(kept, vec![(2, 9), (4, 7)]);
        assert_eq!((n, value), (2, z(10)));
        let (kept, n, value) = keep_largest(vec![(1, 5)], |x| z(x.1), 3).unwrap();
        assert_eq!((kept.len(), n, value), (1, 0, Zatoshis::ZERO));
        let (kept, n, _) = keep_largest(vec![(1, 5), (2, 5)], |x| z(x.1), 1).unwrap();
        // Equal values keep their order.
        assert_eq!((kept, n), (vec![(1, 5)], 1));
    }

    #[test]
    fn an_output_of_5000_is_not_eligible() {
        let f = funded(&[5_000, 5_001]);
        assert_eq!(f.spendable(), 5_001);
        let est =
            f.w.estimate_send_max(f.uuid(), ALPHA_TRANSPARENT.into(), None, None)
                .unwrap_err();
        // 5,001 does not cover the fee of 10,000.
        assert!(
            matches!(
                est,
                WalletError::InsufficientFunds {
                    available: 5_001,
                    required: 10_054
                }
            ),
            "{est:?}"
        );
        // The summary judges the outputs of an address together: the two sum
        // to 10,001, above the marginal fee, and count as spendable.
        let s = f.summary_transparent();
        assert_eq!((s.spendable, s.uneconomic), (10_001, 0), "{s:?}");
    }

    #[test]
    fn the_100000_and_4000_pair() {
        let f = funded(&[100_000, 4_000]);
        // The summary counts the 4,000 as spendable, selection does not.
        let s = f.summary_transparent();
        assert_eq!(s.spendable, 104_000, "{s:?}");
        assert_eq!(f.spendable(), 100_000);
        let est =
            f.w.estimate_send_max(f.uuid(), ALPHA_UA.into(), None, None)
                .unwrap();
        assert_eq!(
            est,
            SendMaxEstimate {
                amount: 85_000,
                fee: 15_000,
                paid_address: ALPHA_UA.into(),
                paid_pool: PaidPool::Ironwood,
                left_behind: LeftBehind::default(),
            }
        );
        let est =
            f.w.estimate_send_max(f.uuid(), ALPHA_TRANSPARENT.into(), None, None)
                .unwrap();
        assert_eq!((est.amount, est.fee), (90_000, 10_000));
    }

    #[test]
    fn the_transparent_cap_leaves_the_smallest_outputs() {
        let values: Vec<u64> = (0..TRANSPARENT_INPUT_CAP as u64 + 2)
            .map(|i| 10_000 + i)
            .collect();
        let f = funded(&values);
        let total: u64 = values.iter().sum();
        let est =
            f.w.estimate_send_max(f.uuid(), ALPHA_TRANSPARENT.into(), None, None)
                .unwrap();
        assert_eq!(
            est.left_behind,
            LeftBehind {
                transparent_count: 2,
                transparent_value: 10_000 + 10_001,
                note_count: 0,
                note_value: 0,
            }
        );
        // One logical action per input: 1,333 inputs against one output.
        assert_eq!(est.fee, 5_000 * TRANSPARENT_INPUT_CAP as u64);
        assert_eq!(est.amount, total - 20_001 - est.fee);
    }

    #[test]
    fn send_max_from_transparent_outputs_to_a_transparent_address() {
        let f = funded(&[100_000, 70_000]);
        let res =
            f.w.send_max(
                f.uuid(),
                SEED.to_vec(),
                ALPHA_TRANSPARENT.into(),
                None,
                10_000,
                Some(160_000),
                None,
            )
            .unwrap();
        assert_eq!((res.amount, res.result.fee), (160_000, 10_000));
        assert_eq!(res.left_behind, LeftBehind::default());
        assert_eq!(res.result.paid_address, ALPHA_TRANSPARENT);
        let tx = decode(res.result.transactions[0].raw.as_ref().unwrap());
        let bundle = tx.transparent_bundle().unwrap();
        assert_eq!((bundle.vin.len(), bundle.vout.len()), (2, 1));
        assert_eq!(bundle.vout[0].value().into_u64(), 160_000);
        assert!(tx.sapling_bundle().is_none() && tx.orchard_bundle().is_none());
        assert!(tx.ironwood_bundle().is_none());
        assert_eq!(f.w.rebroadcast_inventory().unwrap().len(), 1);
        assert_eq!(f.spendable(), 0);
    }

    #[test]
    fn a_send_that_spends_its_inputs_exactly() {
        let f = funded(&[100_000]);
        let res =
            f.w.send(
                f.uuid(),
                SEED.to_vec(),
                ALPHA_TRANSPARENT.into(),
                90_000,
                None,
                10_000,
                None,
            )
            .unwrap();
        assert_eq!(res.fee, 10_000);
        let tx = decode(res.transactions[0].raw.as_ref().unwrap());
        let bundle = tx.transparent_bundle().unwrap();
        // No change: the inputs are the payment and the fee.
        assert_eq!((bundle.vin.len(), bundle.vout.len()), (1, 1));
        assert!(tx.ironwood_bundle().is_none());
        assert_eq!(f.w.rebroadcast_inventory().unwrap().len(), 1);
        assert_eq!(f.spendable(), 0);
    }

    /// The amount the caller approved is the amount paid, or nothing is
    /// created.
    #[test]
    fn send_max_checks_the_expected_amount() {
        let f = funded(&[100_000]);
        let est =
            f.w.estimate_send_max(f.uuid(), ALPHA_TRANSPARENT.into(), None, None)
                .unwrap();
        assert_eq!(est.amount, 90_000);
        // An output arrives between the estimate and the send.
        f.w.put_utxo(Utxo {
            txid: vec![0xee; 32],
            index: 0,
            script: f.script.clone(),
            value_zat: 20_000,
            height: 110,
        })
        .unwrap();
        let send = |expected| {
            f.w.send_max(
                f.uuid(),
                SEED.to_vec(),
                ALPHA_TRANSPARENT.into(),
                None,
                10_000,
                expected,
                None,
            )
        };
        assert!(matches!(
            send(Some(est.amount)),
            Err(WalletError::AmountChanged {
                expected: 90_000,
                actual: 110_000
            })
        ));
        f.assert_untouched(120_000);
        assert_eq!(send(Some(110_000)).unwrap().amount, 110_000);
    }

    #[test]
    fn the_transparent_floor() {
        let f = funded(&[100_000]);
        let estimate = |amount| {
            f.w.estimate_transfer(f.uuid(), ALPHA_TRANSPARENT.into(), amount, None, None)
        };
        assert!(matches!(
            estimate(53),
            Err(WalletError::AmountBelowMinimum { minimum: 54 })
        ));
        // 54 pays, with Ironwood change.
        assert_eq!(estimate(54).unwrap().fee, 15_000);
        assert!(matches!(
            f.w.send(
                f.uuid(),
                SEED.to_vec(),
                ALPHA_TRANSPARENT.into(),
                53,
                None,
                20_000,
                None
            ),
            Err(WalletError::AmountBelowMinimum { minimum: 54 })
        ));
        f.assert_untouched(100_000);

        // Send max to a transparent address needs the fee and 54.
        let f = funded(&[10_053]);
        let err =
            f.w.estimate_send_max(f.uuid(), ALPHA_TRANSPARENT.into(), None, None)
                .unwrap_err();
        assert!(
            matches!(
                err,
                WalletError::InsufficientFunds {
                    available: 10_053,
                    required: 10_054
                }
            ),
            "{err:?}"
        );
        let f = funded(&[10_054]);
        let est =
            f.w.estimate_send_max(f.uuid(), ALPHA_TRANSPARENT.into(), None, None)
                .unwrap();
        assert_eq!((est.amount, est.fee), (54, 10_000));
    }

    #[test]
    fn checks_that_fail_store_nothing() {
        let f = funded(&[100_000]);
        let params = regtest().params().unwrap();
        let Some(Address::Unified(alpha)) = Address::decode(&params, ALPHA_UA) else {
            panic!("ALPHA_UA is not a unified address");
        };
        let t: TransparentAddress = *alpha.transparent().unwrap();
        let expired =
            UnifiedAddress::from_receivers(None, alpha.sapling().copied(), Some(t), None, Some(1))
                .unwrap()
                .encode(&params);

        let send = |address: &str, amount, max_fee| {
            f.w.send(
                f.uuid(),
                SEED.to_vec(),
                address.into(),
                amount,
                None,
                max_fee,
                None,
            )
            .unwrap_err()
        };
        assert!(matches!(
            send(ALPHA_TRANSPARENT, 90_000, 9_999),
            WalletError::FeeTooHigh {
                fee: 10_000,
                max_fee: 9_999
            }
        ));
        assert!(matches!(
            send(&expired, 50_000, 20_000),
            WalletError::AddressExpired { .. }
        ));
        assert!(matches!(
            f.w.send_max(
                f.uuid(),
                SEED.to_vec(),
                ALPHA_TRANSPARENT.into(),
                None,
                9_999,
                None,
                None
            ),
            Err(WalletError::FeeTooHigh {
                fee: 10_000,
                max_fee: 9_999
            })
        ));
        assert!(matches!(
            f.w.send_max(
                f.uuid(),
                SEED.to_vec(),
                expired.clone(),
                None,
                20_000,
                None,
                None
            ),
            Err(WalletError::AddressExpired { .. })
        ));
        assert!(matches!(
            f.w.estimate_send_max(f.uuid(), expired, None, None),
            Err(WalletError::AddressExpired { .. })
        ));
        f.assert_untouched(100_000);
    }

    #[test]
    fn send_max_arguments_are_checked() {
        let f = funded(&[100_000]);
        let send_max = |uuid: String, seed: &[u8], address: &str, memo: Option<Vec<u8>>| {
            f.w.send_max(
                uuid,
                seed.to_vec(),
                address.into(),
                memo,
                20_000,
                None,
                None,
            )
            .unwrap_err()
        };
        assert!(matches!(
            send_max(f.uuid(), &OTHER_SEED, ALPHA_UA, None),
            WalletError::SeedMismatch
        ));
        assert!(matches!(
            send_max(f.uuid(), &SEED, ALPHA_TRANSPARENT, Some(vec![])),
            WalletError::MemoNotAllowed
        ));
        assert!(matches!(
            send_max(
                f.uuid(),
                &SEED,
                "zregtestsapling1uwt3xqagtnemsucergl4f5lcjj0qsh4hu2avqs6gp3fq62yke38dcyxg5ddewwx5pnjrkfmrr6w",
                None
            ),
            WalletError::UnsupportedRecipient { .. }
        ));
        assert!(matches!(
            send_max(uuid::Uuid::new_v4().to_string(), &SEED, ALPHA_UA, None),
            WalletError::AccountNotFound
        ));
        assert!(matches!(
            f.w.estimate_send_max(
                uuid::Uuid::new_v4().to_string(),
                ALPHA_UA.into(),
                None,
                None
            ),
            Err(WalletError::AccountNotFound)
        ));
        f.assert_untouched(100_000);

        // Before the wallet knows the chain.
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("wallet.sqlite");
        let w = Wallet::open(path.to_str().unwrap().to_owned(), regtest(), None).unwrap();
        let account = w
            .create_account("test".into(), SEED.to_vec(), tree_state(99, 0), None, None)
            .unwrap();
        assert!(matches!(
            w.estimate_send_max(account.uuid, ALPHA_UA.into(), None, None),
            Err(WalletError::NotSynced { .. })
        ));
    }

    /// Compares send max with librustzcash's `propose_send_max_transfer`
    /// for accounts without transparent funds, on wallet databases that a
    /// harness run left behind (the Go test TestHarnessSendMaxOracleWallets
    /// writes them). Run with:
    ///
    ///   ZECLIB_ORACLE_DBS=<path>:<path> cargo test --release -- --ignored oracle
    #[test]
    #[ignore]
    fn oracle_parity_without_transparent_funds() {
        let paths = std::env::var("ZECLIB_ORACLE_DBS").expect("ZECLIB_ORACLE_DBS is not set");
        for path in paths.split(':') {
            let w = Wallet::open(path.into(), regtest(), None).unwrap();
            let account = w.accounts().unwrap().remove(0);
            let est = w
                .estimate_send_max(account.uuid.clone(), ALPHA_UA.into(), None, None)
                .unwrap();
            let uuid = parse_account(&account.uuid).unwrap();
            let mut db = w.db();
            let oracle = zcash_client_backend::data_api::wallet::propose_send_max_transfer::<
                _,
                _,
                _,
                Infallible,
            >(
                &mut *db,
                &w.params,
                uuid,
                &[zcash_protocol::ShieldedPool::Ironwood],
                &StandardFeeRule::Zip317,
                zcash_address::ZcashAddress::try_from_encoded(ALPHA_UA).unwrap(),
                None,
                zcash_client_backend::data_api::MaxSpendMode::MaxSpendable,
                confirmations_policy(None).unwrap(),
                &zcash_client_backend::data_api::locking::LockedInputPolicy::Exclude,
                None,
            )
            .unwrap();
            let step = oracle.steps().first();
            let fee = step.balance().fee_required().into_u64();
            let amount = step
                .transaction_request()
                .payments()
                .values()
                .next()
                .and_then(|p| p.amount())
                .unwrap()
                .into_u64();
            let notes = step.shielded_inputs().map_or(0, |s| s.notes().len());
            eprintln!(
                "{path}: {notes} notes, oracle amount {amount} fee {fee}, send max amount {} fee {}",
                est.amount, est.fee
            );
            assert_eq!((est.amount, est.fee), (amount, fee), "{path}");
            assert_eq!(est.left_behind, LeftBehind::default(), "{path}");
        }
    }
}
