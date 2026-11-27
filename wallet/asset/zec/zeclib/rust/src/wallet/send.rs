// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Fee estimates, sends and the spendable value.

use std::convert::Infallible;
use std::fmt::Display;
use std::panic::{self, AssertUnwindSafe};
use std::time::SystemTime;

use rand::rand_core::Rng;
use sapling::{
    Diversifier, MerklePath, PaymentAddress, ProofGenerationKey, Rseed,
    bundle::GrothProofBytes,
    circuit,
    keys::EphemeralSecretKey,
    prover::{OutputProver, SpendProver},
    value::{NoteValue, ValueCommitTrapdoor},
};
use secrecy::{ExposeSecret, SecretVec};
use shardtree::error::ShardTreeError;
use zcash_client_backend::{
    data_api::{
        Account as _, CoinbaseFilter, InputSource, MaxSpendMode, TargetValue, WalletRead,
        error::Error as DataApiError,
        locking::{LockFilter, LockedInputPolicy},
        wallet::{
            ConfirmationsPolicy, SpendingKeys, TargetHeight, create_proposed_transactions,
            input_selection::{
                GreedyInputSelector, GreedyInputSelectorError, SpendPolicy, TransparentSpendPolicy,
            },
            propose_transfer,
        },
    },
    fees::{ChangeError, DustOutputPolicy, StandardFeeRule, zip317::SingleOutputChangeStrategy},
    proposal::{Proposal, ProposalError},
    wallet::{OvkPolicy, WalletTransparentOutput},
};
use zcash_client_sqlite::{AccountUuid, ReceivedNoteId, util::SystemClock};
use zcash_keys::keys::UnifiedSpendingKey;
use zcash_primitives::transaction::{
    TxId, builder::default_tx_expiry_delta, components::orchard::bundle_version_for_branch,
};
use zcash_protocol::{
    PoolType, ShieldedPool,
    consensus::{BlockHeight, BranchId, NetworkUpgrade, Parameters},
    memo::MemoBytes,
    value::Zatoshis,
};
use zip321::{Payment, PaymentError, TransactionRequest};

use super::{Db, Wallet, confirmations_policy, parse_account, secret_seed};
use crate::{
    error::{WalletError, guard},
    network::Params,
    recipient::{PaidPool, Recipient, resolve_recipient},
};

/// A Sapling prover that holds no parameters and proves nothing.
///
/// The wallet never spends a Sapling note and never pays a Sapling receiver,
/// and every proposal is checked for a Sapling part before it is built (see
/// [`check_proposal`]), so the transaction builder never calls a Sapling
/// prover. The stand-in satisfies the type signature of
/// `create_proposed_transactions` without linking the Sapling parameters. It
/// is a backstop: reaching it is a bug, and it panics, which the FFI guard
/// turns into [`WalletError::Internal`].
pub(super) struct NoSaplingProver;

const SAPLING_PROVER_CALLED: &str = "the Sapling prover stand-in was called";

impl SpendProver for NoSaplingProver {
    type Proof = GrothProofBytes;

    fn prepare_circuit(
        _proof_generation_key: ProofGenerationKey,
        _diversifier: Diversifier,
        _rseed: Rseed,
        _value: NoteValue,
        _alpha: jubjub::Fr,
        _rcv: ValueCommitTrapdoor,
        _anchor: bls12_381::Scalar,
        _merkle_path: MerklePath,
    ) -> Option<circuit::Spend> {
        panic!("{SAPLING_PROVER_CALLED}");
    }

    fn create_proof<R: Rng>(&self, _circuit: circuit::Spend, _rng: &mut R) -> Self::Proof {
        panic!("{SAPLING_PROVER_CALLED}");
    }

    fn encode_proof(_proof: Self::Proof) -> GrothProofBytes {
        panic!("{SAPLING_PROVER_CALLED}");
    }
}

impl OutputProver for NoSaplingProver {
    type Proof = GrothProofBytes;

    fn prepare_circuit(
        _esk: &EphemeralSecretKey,
        _payment_address: PaymentAddress,
        _rcm: jubjub::Fr,
        _value: NoteValue,
        _rcv: ValueCommitTrapdoor,
    ) -> circuit::Output {
        panic!("{SAPLING_PROVER_CALLED}");
    }

    fn create_proof<R: Rng>(&self, _circuit: circuit::Output, _rng: &mut R) -> Self::Proof {
        panic!("{SAPLING_PROVER_CALLED}");
    }

    fn encode_proof(_proof: Self::Proof) -> GrothProofBytes {
        panic!("{SAPLING_PROVER_CALLED}");
    }
}

/// A transaction created by [`Wallet::send`].
#[derive(Debug, uniffi::Record)]
pub struct CreatedTransaction {
    /// The transaction ID in the byte order of the wire.
    pub txid: Vec<u8>,
    /// The serialized transaction, or nothing if it could not be read back
    /// from the database after it was created (see [`SendResult`]).
    pub raw: Option<Vec<u8>>,
}

/// The result of [`Wallet::send`].
#[derive(Debug, uniffi::Record)]
pub struct SendResult {
    /// The created transactions, in the order they are to be broadcast.
    pub transactions: Vec<CreatedTransaction>,
    /// The total fee in zatoshis.
    pub fee: u64,
    /// The address the payment names, which the wallet stores as the
    /// recipient of the output: the given address, or for a unified address
    /// without an Orchard receiver, its transparent receiver.
    pub paid_address: String,
    /// The pool the payment lands in.
    pub paid_pool: PaidPool,
    /// Why a raw transaction is missing from `transactions`, if one is. The
    /// transactions exist and are stored in the wallet either way, and the
    /// rebroadcast inventory reads them from the database.
    pub read_error: Option<String>,
}

/// The result of [`Wallet::estimate_transfer`].
#[derive(Debug, PartialEq, Eq, uniffi::Record)]
pub struct TransferEstimate {
    /// The fee in zatoshis.
    pub fee: u64,
    /// The address the payment would name, as in [`SendResult`].
    pub paid_address: String,
    /// The pool the payment would land in.
    pub paid_pool: PaidPool,
}

/// The value an account can spend now, in zatoshis.
#[derive(Debug, Default, PartialEq, Eq, uniffi::Record)]
pub struct SpendableValue {
    /// The value of the Ironwood notes a send can select.
    pub ironwood: u64,
    /// The value of the transparent outputs a send can select: outputs of
    /// more than 5,000 zatoshis that are not coinbase outputs. Input
    /// selection takes at most 1,333 of them into one transaction. The cap
    /// is the selector's default, 10% of the block space at 150 bytes per
    /// input, and not a rule of the chain.
    pub transparent: u64,
}

/// The inputs a send may select: Ironwood notes and the account's transparent
/// outputs other than coinbase outputs.
fn spend_policy() -> SpendPolicy {
    SpendPolicy::shielded_pools([ShieldedPool::Ironwood])
        .with_transparent(TransparentSpendPolicy::any_account_addr())
}

/// Maps an error of proposing or creating a transaction.
pub(super) fn send_error<DE, TE, SE, FE, CE, N>(
    e: DataApiError<DE, TE, SE, FE, CE, N>,
) -> WalletError
where
    DataApiError<DE, TE, SE, FE, CE, N>: Display,
    DE: Display,
    TE: Display,
{
    match e {
        DataApiError::InsufficientFunds {
            available,
            required,
        }
        | DataApiError::Change(ChangeError::InsufficientFunds {
            available,
            required,
        }) => WalletError::InsufficientFunds {
            available: available.into_u64(),
            required: required.into_u64(),
        },
        DataApiError::ScanRequired => WalletError::NotSynced {
            message: "the wallet has no chain tip or no scanned anchor".into(),
        },
        DataApiError::Proposal(ProposalError::AnchorNotFound(height)) => WalletError::NotSynced {
            message: format!("no note commitment tree anchor at height {height}"),
        },
        DataApiError::Proposal(e @ ProposalError::TransactionTooLarge { .. }) => {
            WalletError::TooLarge {
                message: e.to_string(),
            }
        }
        DataApiError::Payment(PaymentError::TransparentMemo) => WalletError::MemoNotAllowed,
        DataApiError::KeyNotRecognized => WalletError::SeedMismatch,
        DataApiError::DataSource(e) => WalletError::database(e),
        DataApiError::RecipientAddressExpiry { error, .. } => WalletError::AddressExpired {
            message: error.to_string(),
        },
        DataApiError::CommitmentTree(e) => match e {
            // The tree cannot give a witness at the anchor: the wallet has
            // not scanned what the witness needs.
            ShardTreeError::Query(e) => WalletError::NotSynced {
                message: format!("note commitment tree: {e}"),
            },
            ShardTreeError::Storage(e) => {
                WalletError::database(format!("note commitment tree: {e}"))
            }
            e @ ShardTreeError::Insert(_) => WalletError::Send {
                message: format!("note commitment tree: {e}"),
            },
        },
        e => WalletError::Send {
            message: e.to_string(),
        },
    }
}

/// The most Ironwood actions the wallet puts into one transaction.
///
/// From NU7 a transaction with more than 330 shielded actions is refused by
/// the mempool and by block verification (ZIP 218; zebrad's
/// `GLOBAL_SHIELDED_BUDGET`). That budget is a block's, so a transaction that
/// used all of it would leave no room for the others in its block. The wallet
/// takes half. This is the wallet's limit, not the chain's.
pub(crate) const MAX_IRONWOOD_ACTIONS: usize = 165;

/// Fails with [`WalletError::TooLarge`] if a transaction with `actions`
/// Ironwood actions exceeds [`MAX_IRONWOOD_ACTIONS`].
pub(crate) fn check_action_limit(actions: usize) -> Result<(), WalletError> {
    if actions > MAX_IRONWOOD_ACTIONS {
        return Err(WalletError::TooLarge {
            message: format!(
                "{actions} Ironwood actions, more than the {MAX_IRONWOOD_ACTIONS} the wallet \
                 puts into one transaction"
            ),
        });
    }
    Ok(())
}

/// The version of the Ironwood bundle a transaction at `target` carries.
/// Fails if Ironwood is not active there.
pub(crate) fn ironwood_bundle_version(
    params: &Params,
    target: BlockHeight,
) -> Result<orchard::bundle::BundleVersion, WalletError> {
    if !params.is_nu_active(NetworkUpgrade::Nu6_3, target) {
        return Err(WalletError::Send {
            message: format!("the Ironwood pool is not active at height {target}"),
        });
    }
    bundle_version_for_branch(
        BranchId::for_height(params, target),
        orchard::ValuePool::Ironwood,
    )
    .ok_or_else(|| WalletError::Send {
        message: format!("no Ironwood bundle version at height {target}"),
    })
}

/// Fails with [`WalletError::AmountBelowMinimum`] if `amount` is below the
/// smallest output the recipient can be paid.
pub(crate) fn check_min_amount(recipient: &Recipient, amount: Zatoshis) -> Result<(), WalletError> {
    let minimum = recipient.min_amount();
    if amount.into_u64() < minimum {
        return Err(WalletError::AmountBelowMinimum { minimum });
    }
    Ok(())
}

/// Checks a proposal before it is built: one transaction, nothing in the
/// Sapling or Orchard pool, the payment in the pool `recipient` resolved to,
/// the expiry of the recipient's unified address, a size a block can hold, no
/// more Ironwood actions than [`MAX_IRONWOOD_ACTIONS`], and a fee within
/// `max_fee` if one is given. Returns the fee. An estimate runs the same
/// checks as a send.
pub(crate) fn check_proposal<N>(
    params: &Params,
    proposal: &Proposal<StandardFeeRule, N>,
    recipient: &Recipient,
    max_fee: Option<u64>,
) -> Result<u64, WalletError> {
    if proposal.steps().len() != 1 {
        return Err(WalletError::Send {
            message: format!(
                "the proposal has {} transactions, not one",
                proposal.steps().len()
            ),
        });
    }
    let step = proposal.steps().first();
    for pool in [PoolType::SAPLING, PoolType::ORCHARD] {
        if step.input_in_pool(pool) || step.output_in_pool(pool) || step.change_in_pool(pool) {
            return Err(WalletError::Send {
                message: format!("the proposal has a part in the {pool:?} pool"),
            });
        }
    }
    // The selector pays the receiver that recipient resolution chose. This
    // holds for the pinned librustzcash and is checked, not assumed.
    let expected = match recipient.pool {
        PaidPool::Ironwood => PoolType::IRONWOOD,
        PaidPool::Transparent => PoolType::TRANSPARENT,
    };
    if step.payment_pools().values().any(|pool| *pool != expected) {
        return Err(WalletError::Send {
            message: format!(
                "the proposal pays the {:?} pool, not the {expected:?} pool",
                step.payment_pools()
            ),
        });
    }
    let target = BlockHeight::from(proposal.min_target_height());
    let tx_expiry = target + default_tx_expiry_delta(BranchId::for_height(params, target));
    recipient.check_expiry(target, tx_expiry, SystemTime::now())?;
    proposal
        .check_transaction_size()
        .map_err(|e| WalletError::TooLarge {
            message: e.to_string(),
        })?;
    if step.involves(PoolType::IRONWOOD) {
        let version = ironwood_bundle_version(params, target)?;
        let actions = step
            .ironwood_action_count(step.ironwood_bundle_padding(), version)
            .map_err(|e| WalletError::Send {
                message: format!("counting the Ironwood actions: {e}"),
            })?;
        check_action_limit(actions)?;
    }
    let fee = step.balance().fee_required().into_u64();
    if let Some(max_fee) = max_fee
        && fee > max_fee
    {
        return Err(WalletError::FeeTooHigh { fee, max_fee });
    }
    Ok(fee)
}

pub(super) fn parse_memo(
    memo: Option<Vec<u8>>,
    recipient: &Recipient,
) -> Result<Option<MemoBytes>, WalletError> {
    let Some(memo) = memo else {
        return Ok(None);
    };
    if recipient.pool == PaidPool::Transparent {
        return Err(WalletError::MemoNotAllowed);
    }
    MemoBytes::from_bytes(&memo)
        .map(Some)
        .map_err(|e| WalletError::invalid(format!("memo: {e}")))
}

fn parse_amount(amount: u64) -> Result<Zatoshis, WalletError> {
    match Zatoshis::from_u64(amount) {
        Ok(z) if z > Zatoshis::ZERO => Ok(z),
        Ok(_) => Err(WalletError::invalid("the amount is zero")),
        Err(_) => Err(WalletError::invalid("the amount is out of range")),
    }
}

/// Reads a created transaction back from the database and serializes it.
fn read_raw(db: &Db, txid: TxId) -> Result<Vec<u8>, String> {
    let tx = db
        .get_transaction(txid)
        .map_err(|e| e.to_string())?
        .ok_or_else(|| format!("the created transaction {txid} is not stored"))?;
    let mut raw = Vec::new();
    tx.write(&mut raw).map_err(|e| e.to_string())?;
    Ok(raw)
}

/// The inputs a send can select now: the account's Ironwood notes and its
/// transparent outputs other than coinbase outputs, under a confirmations
/// policy. Transparent outputs come largest first.
pub(crate) struct Eligible {
    pub(crate) target: TargetHeight,
    pub(crate) anchor: BlockHeight,
    pub(crate) notes: zcash_client_backend::data_api::ReceivedNotes<ReceivedNoteId>,
    pub(crate) transparent: Vec<WalletTransparentOutput<AccountUuid>>,
}

/// Lists the inputs a send can select: Ironwood notes above the marginal fee
/// that are mined at or below the anchor and have the policy's
/// confirmations, and transparent outputs above the marginal fee that are not
/// coinbase outputs. Locked outputs are left out. The transparent list is not
/// capped.
///
/// The queries are the selector's, but with a target of all funds, which in
/// zcash_client_sqlite takes another branch than the target value of an
/// ordinary send. That branch also refuses a note that is not yet stable while
/// a range of its shard is unscanned. So the list is exact once the wallet is
/// scanned to the tip, and during a scan it can include notes that a send
/// cannot select yet.
pub(crate) fn eligible_inputs(
    db: &Db,
    uuid: AccountUuid,
    policy: ConfirmationsPolicy,
) -> Result<Option<Eligible>, WalletError> {
    let Some((target, anchor)) = db.get_target_and_anchor_heights(policy.trusted())? else {
        return Ok(None);
    };
    let exclude = LockedInputPolicy::Exclude;
    let notes = db.select_spendable_notes(
        uuid,
        TargetValue::AllFunds(MaxSpendMode::MaxSpendable),
        &[ShieldedPool::Ironwood],
        target,
        policy,
        &[],
        LockFilter::Policy(&exclude),
    )?;
    let transparent = db.select_spendable_transparent_outputs(
        uuid,
        target,
        policy,
        CoinbaseFilter::NonCoinbaseOnly,
        None,
        TargetValue::AllFunds(MaxSpendMode::MaxSpendable),
        usize::MAX,
        &StandardFeeRule::Zip317,
        LockFilter::Policy(&exclude),
    )?;
    Ok(Some(Eligible {
        target,
        anchor,
        notes,
        transparent,
    }))
}

impl Wallet {
    /// Derives the spending key of an account from a seed, and fails with
    /// [`WalletError::SeedMismatch`] unless the seed is the one the account
    /// was derived from.
    pub(super) fn spending_key(
        &self,
        db: &Db,
        uuid: AccountUuid,
        seed: &SecretVec<u8>,
    ) -> Result<UnifiedSpendingKey, WalletError> {
        let account = db.get_account(uuid)?.ok_or(WalletError::AccountNotFound)?;
        let derivation = account
            .source()
            .key_derivation()
            .ok_or_else(|| WalletError::invalid("the account is not derived from a seed"))?;
        let ufvk = account
            .ufvk()
            .ok_or_else(|| WalletError::invalid("the account has no viewing key"))?;
        let usk = UnifiedSpendingKey::from_seed(
            &self.params,
            seed.expose_secret(),
            derivation.account_index(),
        )
        .map_err(|e| WalletError::invalid(format!("deriving the spending key: {e:?}")))?;
        if usk.to_unified_full_viewing_key().encode(&self.params) != ufvk.encode(&self.params) {
            return Err(WalletError::SeedMismatch);
        }
        Ok(usk)
    }

    /// Proposes a payment from an account. Nothing is stored or locked.
    fn propose(
        &self,
        db: &mut Db,
        uuid: AccountUuid,
        recipient: Recipient,
        amount: Zatoshis,
        memo: Option<MemoBytes>,
        policy: ConfirmationsPolicy,
    ) -> Result<Proposal<StandardFeeRule, ReceivedNoteId>, WalletError> {
        if db.get_account(uuid)?.is_none() {
            return Err(WalletError::AccountNotFound);
        }
        let payment = Payment::new(recipient.address, Some(amount), memo, None, None, vec![])
            .map_err(|e| match e {
                PaymentError::TransparentMemo => WalletError::MemoNotAllowed,
                e => WalletError::invalid(format!("payment: {e}")),
            })?;
        let request = TransactionRequest::new(vec![payment])
            .map_err(|e| WalletError::invalid(format!("payment request: {e}")))?;
        let change = SingleOutputChangeStrategy::<StandardFeeRule, Db>::new(
            StandardFeeRule::Zip317,
            None,
            ShieldedPool::Ironwood,
            DustOutputPolicy::default(),
        );
        propose_transfer::<_, _, _, _, Infallible>(
            db,
            &self.params,
            uuid,
            &GreedyInputSelector::<Db>::new(),
            &change,
            request,
            policy,
            &spend_policy(),
            None,
            None,
        )
        .map_err(|e| match e {
            DataApiError::NoteSelection(
                GreedyInputSelectorError::UnsupportedAddress(_)
                | GreedyInputSelectorError::UnsupportedTexAddress,
            ) => WalletError::UnsupportedRecipient {
                message: "no receiver of the address can be paid".into(),
            },
            e => send_error(e),
        })
    }

    /// The result of a send to `recipient` of the created transactions
    /// `txids`, read back from the database. From here on the send exists:
    /// a failure while reading a transaction back, or a panic that unwinds,
    /// is reported in the result and does not lose the transaction IDs.
    pub(super) fn created(
        &self,
        db: &Db,
        txids: Vec<TxId>,
        fee: u64,
        recipient: &Recipient,
    ) -> SendResult {
        created_with(txids, fee, recipient, |txid| read_raw(db, txid))
    }
}

/// The result of a send of the transactions `txids`, whose raw bytes `read`
/// returns. A failure or a panic of `read` leaves the transaction without its
/// raw bytes and is reported in `read_error`. It does not lose the
/// transaction ID.
fn created_with(
    txids: Vec<TxId>,
    fee: u64,
    recipient: &Recipient,
    read: impl Fn(TxId) -> Result<Vec<u8>, String>,
) -> SendResult {
    {
        let mut read_error = None;
        let transactions = txids
            .into_iter()
            .map(|txid| {
                let raw = match panic::catch_unwind(AssertUnwindSafe(|| read(txid))) {
                    Ok(Ok(raw)) => Some(raw),
                    Ok(Err(e)) => {
                        read_error.get_or_insert(e);
                        None
                    }
                    Err(_) => {
                        read_error.get_or_insert(format!("reading transaction {txid} panicked"));
                        None
                    }
                };
                CreatedTransaction {
                    txid: txid.as_ref().to_vec(),
                    raw,
                }
            })
            .collect();
        SendResult {
            transactions,
            fee,
            paid_address: recipient.address.encode(),
            paid_pool: recipient.pool,
            read_error,
        }
    }
}

#[uniffi::export]
impl Wallet {
    /// Returns the fee, in zatoshis, of a payment of `amount` zatoshis to
    /// `address` from the account, and the address and pool the payment
    /// would name. The payment is proposed as [`send`](Wallet::send) would
    /// propose it and then discarded: nothing is stored or locked. See
    /// `send` for the arguments and errors.
    pub fn estimate_transfer(
        &self,
        account_uuid: String,
        address: String,
        amount: u64,
        memo: Option<Vec<u8>>,
        min_confirmations: Option<u32>,
    ) -> Result<TransferEstimate, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let recipient = resolve_recipient(&self.params, &address)?;
            let memo = parse_memo(memo, &recipient)?;
            let amount = parse_amount(amount)?;
            check_min_amount(&recipient, amount)?;
            let policy = confirmations_policy(min_confirmations)?;
            let paid = recipient.clone();
            let paid_address = recipient.address.encode();
            let paid_pool = recipient.pool;
            let mut db = self.db();
            let proposal = self.propose(&mut db, uuid, recipient, amount, memo, policy)?;
            Ok(TransferEstimate {
                fee: check_proposal(&self.params, &proposal, &paid, None)?,
                paid_address,
                paid_pool,
            })
        })
    }

    /// Pays `amount` zatoshis to `address` from the account, and stores the
    /// transaction in the wallet as unmined. Nothing is broadcast: the caller
    /// broadcasts the returned transactions.
    ///
    /// A unified address is paid at its Orchard receiver, in the Ironwood
    /// pool, if it has one, and otherwise at its transparent receiver. A
    /// Sapling address, a TEX address and a unified address with neither
    /// receiver fail with [`WalletError::UnsupportedRecipient`].
    ///
    /// The inputs are the account's Ironwood notes and its transparent
    /// outputs other than coinbase outputs, transparent outputs first. Change
    /// goes to the Ironwood pool. `min_confirmations` selects the
    /// confirmations policy, as for [`wallet_summary`](Wallet::wallet_summary).
    ///
    /// `memo` is the memo of the recipient's output, up to 512 bytes. An
    /// empty memo is still a memo: it is rejected for a transparent recipient
    /// and sent as an empty memo to a shielded one. No memo is `None`.
    /// `seed` is the account's ZIP 32 seed. It is checked against the account
    /// after the recipient, memo and amount, and before anything is
    /// proposed.
    /// A payment to a transparent receiver below the smallest output nodes
    /// relay, 54 zatoshis for the standard scripts, fails with
    /// [`WalletError::AmountBelowMinimum`] before anything is proposed. A
    /// transaction with more than [`MAX_IRONWOOD_ACTIONS`] Ironwood actions
    /// fails with [`WalletError::TooLarge`]. The send fails before anything is
    /// built if its fee exceeds `max_fee`.
    ///
    /// The seed is erased when the call returns, and so are the shielded
    /// spending keys derived from it. The transparent account private key is
    /// not: zcash_keys does not erase it when it is dropped, so a copy can
    /// remain in freed memory.
    ///
    /// Proposing and creating run under one hold of the database lock, so no
    /// other call can select the same inputs in between. Once this returns,
    /// the transaction is stored and its inputs count as spent until it is
    /// mined or expires, whether or not it is ever broadcast.
    #[allow(clippy::too_many_arguments)]
    pub fn send(
        &self,
        account_uuid: String,
        seed: Vec<u8>,
        address: String,
        amount: u64,
        memo: Option<Vec<u8>>,
        max_fee: u64,
        min_confirmations: Option<u32>,
    ) -> Result<SendResult, WalletError> {
        guard(|| {
            let seed = secret_seed(seed)?;
            let uuid = parse_account(&account_uuid)?;
            let recipient = resolve_recipient(&self.params, &address)?;
            let memo = parse_memo(memo, &recipient)?;
            let amount = parse_amount(amount)?;
            check_min_amount(&recipient, amount)?;
            let policy = confirmations_policy(min_confirmations)?;

            let paid = recipient.clone();
            let mut db = self.db();
            let usk = self.spending_key(&db, uuid, &seed)?;
            drop(seed);
            let proposal = self.propose(&mut db, uuid, recipient, amount, memo, policy)?;
            let fee = check_proposal(&self.params, &proposal, &paid, Some(max_fee))?;

            let txids = create_proposed_transactions::<_, _, Infallible, _, Infallible, _>(
                &mut *db,
                &self.params,
                &SystemClock,
                &mut rand::rand_core::UnwrapErr(rand::rngs::SysRng),
                &NoSaplingProver,
                &NoSaplingProver,
                &SpendingKeys::from_unified_spending_key(usk),
                OvkPolicy::Sender,
                &proposal,
                None,
            )
            .map_err(send_error)?;
            Ok(self.created(&db, txids.into(), fee, &paid))
        })
    }

    /// Returns the value the account can spend now: the value of the inputs
    /// a send can select, under the confirmations policy of
    /// `min_confirmations` (see [`wallet_summary`](Wallet::wallet_summary)).
    ///
    /// The figure is exact once the wallet is scanned to the tip. During a
    /// scan it can include notes that a send cannot select yet (see
    /// `eligible_inputs`). The spendable balances of the summary do not
    /// always match it either: they count transparent coinbase outputs, which
    /// a send does not spend, and they can count a note that is not yet at or
    /// below the anchor a proposal uses. Both values are zero while the
    /// wallet lacks a chain tip.
    pub fn spendable(
        &self,
        account_uuid: String,
        min_confirmations: Option<u32>,
    ) -> Result<SpendableValue, WalletError> {
        guard(|| {
            let uuid = parse_account(&account_uuid)?;
            let policy = confirmations_policy(min_confirmations)?;
            let db = self.db();
            if db.get_account(uuid)?.is_none() {
                return Err(WalletError::AccountNotFound);
            }
            let Some(eligible) = eligible_inputs(&db, uuid, policy)? else {
                return Ok(SpendableValue::default());
            };
            let ironwood = eligible
                .notes
                .total_value()
                .map_err(|e| WalletError::database(format!("note values: {e:?}")))?
                .into_u64();
            let transparent = eligible
                .transparent
                .iter()
                .map(|o| o.value().into_u64())
                .sum();
            Ok(SpendableValue {
                ironwood,
                transparent,
            })
        })
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use nonempty::NonEmpty;
    use sapling::Rseed;
    use transparent::{
        address::{Script, TransparentAddress},
        bundle::{OutPoint, TxOut},
    };
    use zcash_address::ZcashAddress;
    use zcash_client_backend::{
        fees::{ChangeValue, TransactionBalance},
        proposal::{ShieldedInputs, Step},
        wallet::{Note, ReceivedNote},
    };
    use zcash_client_sqlite::error::SqliteClientError;
    use zcash_keys::address::Address;
    use zcash_protocol::consensus::BlockHeight;
    use zcash_script::script;

    use super::*;
    use crate::testutil::regtest;

    // Account 0 of the alpha funder of the simnet harness: a unified address
    // with Orchard, Sapling and transparent receivers, and its transparent
    // receiver.
    const ALPHA_UA: &str = "uregtest13pvma5dl78lka0cl83u4jmm78emspd6rm9e56xgm3ttk4pxcwcr4guhdc5u8mg7z3fyr9zjec593khe5m4c3a825yx22lcjwm4anp2xvu4ej5frpc5dm25294c8w3x0pnnxjde38tz4fpgaglrrhx0k7z5259slahnxng23mhn8lhhff8dkeznyhynrfwlu3e8mzedknqathcdlv7hd";
    const ALPHA_TRANSPARENT: &str = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm";

    /// An unspent output of `value` paying ALPHA_TRANSPARENT, at an outpoint
    /// distinguished by `n`.
    fn transparent_input(value: u64, n: u8) -> WalletTransparentOutput<()> {
        let params = regtest().params().unwrap();
        let Some(Address::Transparent(TransparentAddress::PublicKeyHash(hash))) =
            Address::decode(&params, ALPHA_TRANSPARENT)
        else {
            panic!("not a P2PKH address");
        };
        let mut code = vec![0x76, 0xa9, 0x14];
        code.extend_from_slice(&hash);
        code.extend_from_slice(&[0x88, 0xac]);
        WalletTransparentOutput::from_parts(
            OutPoint::new([n; 32], 0),
            TxOut::new(z(value), Script(script::Code(code))),
            Some(BlockHeight::from_u32(10)),
            None,
            None,
            None,
        )
        .unwrap()
    }

    const ALPHA_SAPLING: &str = "zregtestsapling1uwt3xqagtnemsucergl4f5lcjj0qsh4hu2avqs6gp3fq62yke38dcyxg5ddewwx5pnjrkfmrr6w";

    /// A Sapling note of `value` paying ALPHA_SAPLING, as a selected input.
    fn sapling_input(value: u64) -> ShieldedInputs<u32> {
        let params = params();
        let Some(Address::Sapling(pa)) = Address::decode(&params, ALPHA_SAPLING) else {
            panic!("not a Sapling address");
        };
        let note = sapling::Note::from_parts(
            *pa,
            sapling::value::NoteValue::from_raw(value),
            Rseed::AfterZip212([1; 32]),
        );
        ShieldedInputs::from_parts(NonEmpty::singleton(ReceivedNote::from_parts(
            1,
            TxId::from_bytes([9; 32]),
            0,
            Note::Sapling(note),
            zip32::Scope::External,
            incrementalmerkletree::Position::from(0),
            Some(BlockHeight::from_u32(10)),
            None,
        )))
    }

    /// A step that spends one transparent output of 100,000 and the given
    /// shielded inputs, returns `change`, and pays the rest less `fee` to
    /// `recipient` in `pool`.
    fn step_with(
        n: u8,
        recipient: &str,
        pool: PoolType,
        fee: u64,
        change: Vec<ChangeValue>,
        shielded: Option<(ShieldedInputs<u32>, u64)>,
        ironwood_active: bool,
    ) -> Step<u32> {
        let change_value: u64 = change.iter().map(|c| c.value().into_u64()).sum();
        let shielded_value = shielded.as_ref().map_or(0, |(_, v)| *v);
        let request = TransactionRequest::new(vec![Payment::without_memo(
            ZcashAddress::try_from_encoded(recipient).unwrap(),
            z(100_000 + shielded_value - fee - change_value),
        )])
        .unwrap();
        let shielded_part = shielded.is_some();
        Step::from_parts(
            &[],
            request,
            BTreeMap::from([(0, pool)]),
            vec![transparent_input(100_000, n)],
            shielded.map(|(inputs, _)| inputs),
            // A step with a shielded part needs an anchor.
            (pool != PoolType::TRANSPARENT || !change.is_empty() || shielded_part)
                .then(|| BlockHeight::from_u32(10)),
            vec![],
            TransactionBalance::new(change, z(fee)).unwrap(),
            false,
            ironwood_active,
        )
        .unwrap()
    }

    fn step(n: u8, recipient: &str, pool: PoolType, fee: u64) -> Step<u32> {
        step_with(n, recipient, pool, fee, vec![], None, true)
    }

    fn proposal(steps: Vec<Step<u32>>) -> Proposal<StandardFeeRule, u32> {
        Proposal::multi_step(
            StandardFeeRule::Zip317,
            BlockHeight::from_u32(20).into(),
            ConfirmationsPolicy::default(),
            NonEmpty::from_vec(steps).unwrap(),
        )
        .unwrap()
    }

    fn params() -> Params {
        regtest().params().unwrap()
    }

    fn to(address: &str) -> Recipient {
        resolve_recipient(&params(), address).unwrap()
    }

    #[test]
    fn the_action_limit() {
        assert!(check_action_limit(MAX_IRONWOOD_ACTIONS).is_ok());
        assert!(matches!(
            check_action_limit(MAX_IRONWOOD_ACTIONS + 1),
            Err(WalletError::TooLarge { .. })
        ));
        assert_eq!(MAX_IRONWOOD_ACTIONS, 165);
    }

    fn assert_send_error(err: WalletError, needle: &str) {
        assert!(
            matches!(&err, WalletError::Send { message } if message.contains(needle)),
            "want Send containing {needle:?}, got {err:?}"
        );
    }

    #[test]
    fn proposals_are_checked() {
        let p = params();
        let transparent = proposal(vec![step(
            1,
            ALPHA_TRANSPARENT,
            PoolType::TRANSPARENT,
            10_000,
        )]);
        let t = to(ALPHA_TRANSPARENT);
        assert_eq!(check_proposal(&p, &transparent, &t, None).unwrap(), 10_000);
        assert_eq!(
            check_proposal(&p, &transparent, &t, Some(10_000)).unwrap(),
            10_000
        );
        assert!(matches!(
            check_proposal(&p, &transparent, &t, Some(9_999)),
            Err(WalletError::FeeTooHigh {
                fee: 10_000,
                max_fee: 9_999
            })
        ));

        let ironwood = proposal(vec![step(1, ALPHA_UA, PoolType::IRONWOOD, 15_000)]);
        let u = to(ALPHA_UA);
        assert_eq!(
            check_proposal(&p, &ironwood, &u, Some(15_000)).unwrap(),
            15_000
        );
        // With Ironwood change as well.
        let change = proposal(vec![step_with(
            1,
            ALPHA_TRANSPARENT,
            PoolType::TRANSPARENT,
            15_000,
            vec![ChangeValue::shielded(
                ShieldedPool::Ironwood,
                z(40_000),
                None,
            )],
            None,
            true,
        )]);
        assert_eq!(check_proposal(&p, &change, &t, None).unwrap(), 15_000);

        // The payment is in another pool than the one resolution chose.
        assert_send_error(
            check_proposal(&p, &transparent, &u, None).unwrap_err(),
            "pays the",
        );

        // A Sapling payment, a Sapling input, Sapling change.
        let sapling_payment = proposal(vec![step(1, ALPHA_UA, PoolType::SAPLING, 10_000)]);
        assert_send_error(
            check_proposal(&p, &sapling_payment, &u, None).unwrap_err(),
            "Sapling",
        );
        let sapling_in = proposal(vec![step_with(
            1,
            ALPHA_TRANSPARENT,
            PoolType::TRANSPARENT,
            15_000,
            vec![],
            Some((sapling_input(50_000), 50_000)),
            true,
        )]);
        assert_send_error(
            check_proposal(&p, &sapling_in, &t, None).unwrap_err(),
            "Sapling",
        );
        let sapling_change = proposal(vec![step_with(
            1,
            ALPHA_TRANSPARENT,
            PoolType::TRANSPARENT,
            15_000,
            vec![ChangeValue::shielded(
                ShieldedPool::Sapling,
                z(40_000),
                None,
            )],
            None,
            true,
        )]);
        assert_send_error(
            check_proposal(&p, &sapling_change, &t, None).unwrap_err(),
            "Sapling",
        );

        // An Orchard payment and Orchard change. A step with either can only
        // be built for a height before Ironwood.
        let orchard_payment = proposal(vec![step_with(
            1,
            ALPHA_UA,
            PoolType::ORCHARD,
            10_000,
            vec![],
            None,
            false,
        )]);
        assert_send_error(
            check_proposal(&p, &orchard_payment, &u, None).unwrap_err(),
            "Orchard",
        );
        let orchard_change = proposal(vec![step_with(
            1,
            ALPHA_TRANSPARENT,
            PoolType::TRANSPARENT,
            15_000,
            vec![ChangeValue::shielded(
                ShieldedPool::Orchard,
                z(40_000),
                None,
            )],
            None,
            false,
        )]);
        assert_send_error(
            check_proposal(&p, &orchard_change, &t, None).unwrap_err(),
            "Orchard",
        );

        let two = proposal(vec![
            step(1, ALPHA_TRANSPARENT, PoolType::TRANSPARENT, 10_000),
            step(2, ALPHA_TRANSPARENT, PoolType::TRANSPARENT, 10_000),
        ]);
        assert_send_error(
            check_proposal(&p, &two, &t, None).unwrap_err(),
            "2 transactions",
        );
    }

    #[test]
    fn an_expired_recipient_fails_the_check() {
        // The proposals target height 20 and expire 120 blocks later, at 140.
        let p = params();
        let Some(Address::Transparent(t)) = Address::decode(&p, ALPHA_TRANSPARENT) else {
            panic!("not a transparent address");
        };
        let Some(Address::Sapling(sapling)) = Address::decode(&p, ALPHA_SAPLING) else {
            panic!("not a Sapling address");
        };
        let ua = |height: Option<u32>, time: Option<u64>| {
            zcash_keys::address::UnifiedAddress::from_receivers(
                None,
                Some(*sapling),
                Some(t),
                height.map(BlockHeight::from_u32),
                time,
            )
            .unwrap()
            .encode(&p)
        };
        let check = |address: &str| {
            let r = to(address);
            let pay = r.address.encode();
            let proposal = proposal(vec![step(1, &pay, PoolType::TRANSPARENT, 10_000)]);
            check_proposal(&p, &proposal, &r, None)
        };
        let expired = |res: Result<u64, WalletError>, needle: &str| {
            assert!(
                matches!(&res, Err(WalletError::AddressExpired { message }) if message.contains(needle)),
                "want AddressExpired containing {needle:?}, got {res:?}"
            );
        };
        expired(check(&ua(None, Some(1))), "Unix time 1");
        expired(check(&ua(Some(18), None)), "expired at height 18");
        expired(
            check(&ua(Some(100), None)),
            "before the transaction would at 140",
        );
        // An expiry at the transaction's own, and a far expiry time.
        assert_eq!(check(&ua(Some(140), Some(u64::MAX))).unwrap(), 10_000);
    }

    #[test]
    fn created_reports_a_failed_or_panicking_readback() {
        let r = to(ALPHA_TRANSPARENT);
        let ids = vec![TxId::from_bytes([1; 32]), TxId::from_bytes([2; 32])];
        let res = created_with(ids.clone(), 10_000, &r, |txid| {
            if txid.as_ref()[0] == 1 {
                Err("serializing: broken".into())
            } else {
                Ok(vec![7])
            }
        });
        assert_eq!(res.transactions[0].raw, None);
        assert_eq!(res.transactions[1].raw, Some(vec![7]));
        assert_eq!(res.read_error.as_deref(), Some("serializing: broken"));
        let res = created_with(ids, 10_000, &r, |txid| {
            if txid.as_ref()[0] == 2 {
                panic!("readback");
            }
            Ok(vec![7])
        });
        assert_eq!(res.transactions.len(), 2);
        assert_eq!(res.transactions[1].txid, vec![2; 32]);
        assert_eq!(res.transactions[1].raw, None);
        assert!(res.read_error.is_some_and(|e| e.contains("panicked")));
    }

    #[test]
    fn created_keeps_a_transaction_that_cannot_be_read() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("wallet.sqlite");
        let w = Wallet::open(path.to_str().unwrap().to_owned(), regtest(), None).unwrap();
        let recipient = resolve_recipient(&w.params, ALPHA_UA).unwrap();
        let txid = TxId::from_bytes([3; 32]);
        let res = w.created(&w.db(), vec![txid], 10_000, &recipient);
        assert_eq!(res.transactions.len(), 1);
        assert_eq!(res.transactions[0].txid, vec![3; 32]);
        assert_eq!(res.transactions[0].raw, None);
        assert!(res.read_error.is_some_and(|e| e.contains("not stored")));
        assert_eq!(res.fee, 10_000);
        assert_eq!(res.paid_address, ALPHA_UA);
        assert_eq!(res.paid_pool, PaidPool::Ironwood);
    }

    type E = DataApiError<SqliteClientError, Infallible, Infallible, Infallible, Infallible, u32>;

    fn z(n: u64) -> Zatoshis {
        Zatoshis::from_u64(n).unwrap()
    }

    #[test]
    fn send_errors_are_mapped() {
        let cases: Vec<(E, WalletError)> = vec![
            (
                E::InsufficientFunds {
                    available: z(7),
                    required: z(10_007),
                },
                WalletError::InsufficientFunds {
                    available: 7,
                    required: 10_007,
                },
            ),
            (
                E::Change(ChangeError::InsufficientFunds {
                    available: z(1),
                    required: z(2),
                }),
                WalletError::InsufficientFunds {
                    available: 1,
                    required: 2,
                },
            ),
            (E::KeyNotRecognized, WalletError::SeedMismatch),
            (
                E::DataSource(SqliteClientError::AccountUnknown),
                WalletError::Database {
                    message: SqliteClientError::AccountUnknown.to_string(),
                },
            ),
            (
                E::Payment(PaymentError::TransparentMemo),
                WalletError::MemoNotAllowed,
            ),
        ];
        for (from, want) in cases {
            let got = send_error(from);
            assert_eq!(format!("{got:?}"), format!("{want:?}"));
        }

        assert!(matches!(
            send_error(E::ScanRequired),
            WalletError::NotSynced { .. }
        ));
        let err = send_error(E::Proposal(ProposalError::AnchorNotFound(
            zcash_protocol::consensus::BlockHeight::from_u32(42),
        )));
        assert!(
            matches!(&err, WalletError::NotSynced { message } if message.contains("42")),
            "{err:?}"
        );
        assert!(matches!(
            send_error(E::ProposalNotSupported),
            WalletError::Send { .. }
        ));
        let err = send_error(E::Proposal(ProposalError::TransactionTooLarge {
            estimated_size: 3_000_000,
            limit: 2_000_000,
            sapling_input_count: 0,
            orchard_input_count: 0,
            ironwood_input_count: 9_000,
        }));
        assert!(matches!(err, WalletError::TooLarge { .. }), "{err:?}");
        let err = send_error(E::CommitmentTree(ShardTreeError::Query(
            shardtree::error::QueryError::CheckpointPruned,
        )));
        assert!(matches!(err, WalletError::NotSynced { .. }), "{err:?}");
        type T = DataApiError<
            SqliteClientError,
            std::io::Error,
            Infallible,
            Infallible,
            Infallible,
            u32,
        >;
        let err = send_error(T::CommitmentTree(ShardTreeError::Storage(
            std::io::Error::other("disk"),
        )));
        assert!(matches!(err, WalletError::Database { .. }), "{err:?}");
        let err = send_error(T::CommitmentTree(ShardTreeError::Insert(
            shardtree::error::InsertionError::TreeFull,
        )));
        assert!(matches!(err, WalletError::Send { .. }), "{err:?}");
        let err = send_error(E::RecipientAddressExpiry {
            payment_index: 0,
            error: zcash_client_backend::data_api::error::AddressExpiryError::TimeExpired {
                expiry_time: 1,
            },
        });
        assert!(matches!(err, WalletError::AddressExpired { .. }), "{err:?}");
    }

    #[test]
    fn amounts_are_checked() {
        assert_eq!(parse_amount(1).unwrap(), z(1));
        assert!(matches!(
            parse_amount(0),
            Err(WalletError::InvalidArgument { .. })
        ));
        assert!(matches!(
            parse_amount(u64::MAX),
            Err(WalletError::InvalidArgument { .. })
        ));
    }
}
