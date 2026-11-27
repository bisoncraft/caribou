// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Resolution of a payment recipient to the receiver that is paid.

use std::time::{SystemTime, UNIX_EPOCH};

use transparent::address::TransparentAddress;
use zcash_address::{
    ZcashAddress,
    unified::{MetadataTypecode, Typecode},
};
use zcash_keys::address::{Address, UnifiedAddress};
use zcash_protocol::consensus::BlockHeight;

use crate::{error::WalletError, network::Params};

/// The pool a payment lands in.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum PaidPool {
    /// An Orchard receiver, paid in the Ironwood pool from NU6.3 on.
    Ironwood,
    /// A transparent receiver.
    Transparent,
}

/// A recipient resolved to the receiver that is paid.
#[derive(Clone, Debug)]
pub(crate) struct Recipient {
    /// The address the payment request names.
    pub(crate) address: ZcashAddress,
    pub(crate) pool: PaidPool,
    /// For a transparent receiver, the serialized size of the output that
    /// pays it: 8 bytes of value, a one-byte script length and the script.
    pub(crate) transparent_output_size: Option<usize>,
    /// The expiry height and time of the unified address given, which a
    /// sender has to honor whatever receiver it pays (ZIP 316).
    pub(crate) expiry_height: Option<BlockHeight>,
    pub(crate) expiry_time: Option<u64>,
}

impl Recipient {
    /// The smallest amount a transparent receiver can be paid, and zero for
    /// an Ironwood one.
    pub(crate) fn min_amount(&self) -> u64 {
        self.transparent_output_size
            .map_or(0, min_transparent_amount)
    }

    /// Checks the expiry of the recipient's unified address for a
    /// transaction that targets the block at `target` and expires at
    /// `tx_expiry`, with the rules of ZIP 316 that the transaction builder
    /// applies: the address has not expired at the chain tip, one block
    /// below `target`, or at `now`, and the transaction cannot be mined after
    /// the address's expiry height. A `tx_expiry` of zero is a transaction
    /// that never expires, later than any expiry height.
    pub(crate) fn check_expiry(
        &self,
        target: BlockHeight,
        tx_expiry: BlockHeight,
        now: SystemTime,
    ) -> Result<(), WalletError> {
        let expired = |message: String| Err(WalletError::AddressExpired { message });
        if let Some(expiry) = self.expiry_height {
            let tip = target.saturating_sub(1);
            if tip > expiry {
                return expired(format!(
                    "the address expired at height {expiry}, and the chain is at {tip}"
                ));
            }
            if tx_expiry > expiry || u32::from(tx_expiry) == 0 {
                return expired(format!(
                    "the address expires at height {expiry}, before the transaction would at \
                     {tx_expiry}"
                ));
            }
        }
        if let Some(expiry) = self.expiry_time {
            let now = now.duration_since(UNIX_EPOCH).map_or(0, |d| d.as_secs());
            if now > expiry {
                return expired(format!("the address expired at Unix time {expiry}"));
            }
        }
        Ok(())
    }
}

/// The smallest value of a transparent output of `output_size` serialized
/// bytes that nodes relay and mine. zebrad refuses a smaller one as dust:
/// three times the fee of 100 zatoshis per 1,000 bytes for the output and the
/// 148 bytes of the input that would spend it, rounded down before the
/// tripling (zebra-chain `transparent::Output::is_dust`). For P2PKH and P2SH
/// outputs it is 54 zatoshis.
pub(crate) fn min_transparent_amount(output_size: usize) -> u64 {
    3 * ((100 * (output_size as u64 + 148)) / 1000)
}

/// The serialized size of an output paying a transparent address.
fn transparent_output_size(address: &TransparentAddress) -> usize {
    let script_len = match address {
        // OP_DUP OP_HASH160 <20 bytes> OP_EQUALVERIFY OP_CHECKSIG
        TransparentAddress::PublicKeyHash(_) => 25,
        // OP_HASH160 <20 bytes> OP_EQUAL
        TransparentAddress::ScriptHash(_) => 23,
    };
    8 + 1 + script_len
}

fn unsupported(message: &str) -> WalletError {
    WalletError::UnsupportedRecipient {
        message: message.into(),
    }
}

/// The address a payment to the transparent receiver of a unified address
/// names. Without expiry metadata it is the bare transparent address. With
/// it, it is the unified address reduced to its transparent receiver and its
/// expiry, so that the payment still names a unified address and the
/// transaction builder applies the address's expiry. ZIP 316 revision 2
/// carries the expiry metadata and allows a unified address without a
/// shielded receiver; the pinned zcash_address implements revisions 0 and 2,
/// and revision 1 is withdrawn.
pub(crate) fn transparent_fallback(
    params: &Params,
    ua: &UnifiedAddress,
    t: &TransparentAddress,
) -> Result<ZcashAddress, WalletError> {
    if ua.expiry_height().is_none() && ua.expiry_time().is_none() {
        return Ok(Address::Transparent(*t).to_zcash_address(params));
    }
    let receiver = match t {
        TransparentAddress::PublicKeyHash(_) => Typecode::P2PKH,
        TransparentAddress::ScriptHash(_) => Typecode::P2SH,
    };
    match ua.prune_retaining(&[
        receiver,
        Typecode::Metadata(MetadataTypecode::ExpiryHeight),
        Typecode::Metadata(MetadataTypecode::ExpiryTime),
    ]) {
        Ok(Some(pruned)) => Ok(Address::Unified(Box::new(pruned)).to_zcash_address(params)),
        Ok(None) => Err(unsupported("a unified address without a payable receiver")),
        Err(e) => Err(unsupported(&format!(
            "the transparent receiver cannot be paid with the address's metadata: {e}"
        ))),
    }
}

/// Resolves a recipient address to the receiver that is paid.
///
/// - A transparent address is paid as it is.
/// - A unified address with an Orchard receiver is paid at that receiver,
///   whatever other receivers it has. The payment request names the unified
///   address itself, and the transaction builder pays its Orchard receiver
///   ahead of the others.
/// - A unified address without an Orchard receiver is paid at its transparent
///   receiver, also when it has a Sapling receiver. The payment request names
///   the transparent receiver alone, because the builder would otherwise pay
///   the Sapling receiver: as a bare transparent address, or for an address
///   with an expiry, as a unified address of that receiver and the expiry.
/// - The expiry of a unified address is kept in either case, for
///   [`Recipient::check_expiry`].
/// - A unified address with no Orchard or transparent receiver, a Sapling
///   address and a TEX address are rejected. The wallet builds no Sapling
///   outputs and no ZIP 320 transaction pairs.
pub(crate) fn resolve_recipient(params: &Params, address: &str) -> Result<Recipient, WalletError> {
    let decoded = Address::decode(params, address)
        .ok_or_else(|| unsupported("not a valid address for this network"))?;
    match decoded {
        Address::Transparent(t) => Ok(Recipient {
            address: decoded.to_zcash_address(params),
            pool: PaidPool::Transparent,
            transparent_output_size: Some(transparent_output_size(&t)),
            expiry_height: None,
            expiry_time: None,
        }),
        Address::Unified(ua) if ua.orchard().is_some() => Ok(Recipient {
            expiry_height: ua.expiry_height(),
            expiry_time: ua.expiry_time(),
            address: Address::Unified(ua).to_zcash_address(params),
            pool: PaidPool::Ironwood,
            transparent_output_size: None,
        }),
        Address::Unified(ua) => match ua.transparent() {
            Some(t) => Ok(Recipient {
                address: transparent_fallback(params, &ua, t)?,
                pool: PaidPool::Transparent,
                transparent_output_size: Some(transparent_output_size(t)),
                expiry_height: ua.expiry_height(),
                expiry_time: ua.expiry_time(),
            }),
            None => Err(unsupported(
                "a unified address without an Orchard or a transparent receiver",
            )),
        },
        Address::Sapling(_) => Err(unsupported("a Sapling address")),
        Address::Tex(_) => Err(unsupported("a TEX address")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::regtest;

    // Account 0 of the alpha funder of the simnet harness: a unified address
    // with Orchard, Sapling and transparent receivers, and its Sapling and
    // transparent receivers on their own.
    const ALPHA_UA: &str = "uregtest13pvma5dl78lka0cl83u4jmm78emspd6rm9e56xgm3ttk4pxcwcr4guhdc5u8mg7z3fyr9zjec593khe5m4c3a825yx22lcjwm4anp2xvu4ej5frpc5dm25294c8w3x0pnnxjde38tz4fpgaglrrhx0k7z5259slahnxng23mhn8lhhff8dkeznyhynrfwlu3e8mzedknqathcdlv7hd";
    const ALPHA_SAPLING: &str = "zregtestsapling1uwt3xqagtnemsucergl4f5lcjj0qsh4hu2avqs6gp3fq62yke38dcyxg5ddewwx5pnjrkfmrr6w";
    const ALPHA_TRANSPARENT: &str = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm";

    fn params() -> Params {
        regtest().params().unwrap()
    }

    /// ALPHA_UA reduced to the given receivers.
    fn pruned(typecodes: &[Typecode]) -> String {
        let params = params();
        let Some(Address::Unified(ua)) = Address::decode(&params, ALPHA_UA) else {
            panic!("ALPHA_UA is not a unified address");
        };
        ua.prune_retaining(typecodes)
            .unwrap()
            .expect("the address has the receivers")
            .encode(&params)
    }

    fn tex() -> String {
        let params = params();
        let Some(Address::Transparent(t)) = Address::decode(&params, ALPHA_TRANSPARENT) else {
            panic!("ALPHA_TRANSPARENT is not a transparent address");
        };
        let transparent::address::TransparentAddress::PublicKeyHash(hash) = t else {
            panic!("not a P2PKH address");
        };
        Address::Tex(hash).encode(&params)
    }

    fn assert_unsupported(address: &str, needle: &str) {
        let err = resolve_recipient(&params(), address).unwrap_err();
        assert!(
            matches!(&err, WalletError::UnsupportedRecipient { message } if message.contains(needle)),
            "{address}: want UnsupportedRecipient containing {needle:?}, got {err:?}"
        );
    }

    #[test]
    fn recipient_matrix() {
        let params = params();
        let paid = |address: &str| {
            let r = resolve_recipient(&params, address).unwrap();
            (r.address.encode(), r.pool)
        };

        // A transparent address is paid as it is.
        assert_eq!(
            paid(ALPHA_TRANSPARENT),
            (ALPHA_TRANSPARENT.into(), PaidPool::Transparent)
        );

        // With an Orchard receiver, the unified address is paid in the
        // Ironwood pool and named as it is.
        for ua in [
            ALPHA_UA.to_owned(),
            pruned(&[Typecode::ORCHARD]),
            pruned(&[Typecode::ORCHARD, Typecode::P2PKH]),
            pruned(&[Typecode::ORCHARD, Typecode::SAPLING]),
        ] {
            assert_eq!(paid(&ua), (ua.clone(), PaidPool::Ironwood));
        }

        // Without one, the transparent receiver is paid, also beside a
        // Sapling receiver.
        assert_eq!(
            paid(&pruned(&[Typecode::SAPLING, Typecode::P2PKH])),
            (ALPHA_TRANSPARENT.into(), PaidPool::Transparent)
        );

        // Nothing payable.
        assert_unsupported(
            &pruned(&[Typecode::SAPLING]),
            "without an Orchard or a transparent receiver",
        );
        assert_unsupported(ALPHA_SAPLING, "a Sapling address");
        assert_unsupported(&tex(), "a TEX address");
        assert_unsupported("not an address", "not a valid address");
    }

    #[test]
    fn the_expiry_of_a_unified_address_is_kept() {
        let params = params();
        let Some(Address::Unified(alpha)) = Address::decode(&params, ALPHA_UA) else {
            panic!("ALPHA_UA is not a unified address");
        };
        let t = *alpha.transparent().unwrap();
        let with = |orchard: bool, height: Option<u32>, time: Option<u64>| {
            UnifiedAddress::from_receivers(
                orchard.then(|| *alpha.orchard().unwrap()),
                alpha.sapling().copied(),
                Some(t),
                height.map(BlockHeight::from_u32),
                time,
            )
            .unwrap()
            .encode(&params)
        };

        // Paid at the transparent receiver, a unified address with an expiry
        // is named as a unified address of that receiver and the expiry, so
        // that the builder checks the expiry too.
        let r = resolve_recipient(&params, &with(false, Some(500), Some(9_000))).unwrap();
        assert_eq!(r.pool, PaidPool::Transparent);
        assert_eq!(r.expiry_height, Some(BlockHeight::from_u32(500)));
        assert_eq!(r.expiry_time, Some(9_000));
        let Some(Address::Unified(named)) = Address::decode(&params, &r.address.encode()) else {
            panic!(
                "the payment does not name a unified address: {}",
                r.address.encode()
            );
        };
        assert_eq!(named.transparent(), Some(&t));
        assert!(named.orchard().is_none() && named.sapling().is_none());
        assert_eq!(named.expiry_height(), Some(BlockHeight::from_u32(500)));
        assert_eq!(named.expiry_time(), Some(9_000));

        // Without an expiry it is named as the bare transparent address.
        let r = resolve_recipient(&params, &with(false, None, None)).unwrap();
        assert_eq!(r.address.encode(), ALPHA_TRANSPARENT);
        assert_eq!((r.expiry_height, r.expiry_time), (None, None));

        // Paid at the Orchard receiver, the address is named as given and its
        // expiry kept.
        let ua = with(true, None, Some(1));
        let r = resolve_recipient(&params, &ua).unwrap();
        assert_eq!(
            (r.pool, r.address.encode(), r.expiry_time),
            (PaidPool::Ironwood, ua, Some(1))
        );
    }

    #[test]
    fn expiry_rules() {
        let params = params();
        let Some(Address::Unified(alpha)) = Address::decode(&params, ALPHA_UA) else {
            panic!("ALPHA_UA is not a unified address");
        };
        let ua = UnifiedAddress::from_receivers(
            alpha.orchard().copied(),
            None,
            None,
            Some(BlockHeight::from_u32(100)),
            Some(1_000),
        )
        .unwrap()
        .encode(&params);
        let r = resolve_recipient(&params, &ua).unwrap();
        let h = BlockHeight::from_u32;
        let at = |secs| UNIX_EPOCH + std::time::Duration::from_secs(secs);
        // The chain tip is one below the target.
        assert!(r.check_expiry(h(101), h(100), at(1_000)).is_ok());
        assert!(matches!(
            r.check_expiry(h(102), h(100), at(1_000)),
            Err(WalletError::AddressExpired { .. })
        ));
        assert!(matches!(
            r.check_expiry(h(90), h(101), at(1_000)),
            Err(WalletError::AddressExpired { .. })
        ));
        assert!(matches!(
            r.check_expiry(h(90), h(100), at(1_001)),
            Err(WalletError::AddressExpired { .. })
        ));
        // A transaction that never expires cannot pay an address that does.
        assert!(matches!(
            r.check_expiry(h(90), h(0), at(1_000)),
            Err(WalletError::AddressExpired { .. })
        ));
        // Without an expiry nothing is checked.
        let r = resolve_recipient(&params, ALPHA_UA).unwrap();
        assert!(
            r.check_expiry(h(1_000_000), h(2_000_000), at(4_000_000_000))
                .is_ok()
        );
    }

    #[test]
    fn transparent_floor() {
        // zebrad's dust rule for the two standard scripts.
        assert_eq!(min_transparent_amount(34), 54);
        assert_eq!(min_transparent_amount(32), 54);
        let r = resolve_recipient(&params(), ALPHA_TRANSPARENT).unwrap();
        assert_eq!((r.transparent_output_size, r.min_amount()), (Some(34), 54));
        let r = resolve_recipient(&params(), ALPHA_UA).unwrap();
        assert_eq!((r.transparent_output_size, r.min_amount()), (None, 0));
        let r =
            resolve_recipient(&params(), &pruned(&[Typecode::SAPLING, Typecode::P2PKH])).unwrap();
        assert_eq!(r.min_amount(), 54);
    }

    #[test]
    fn rejects_addresses_of_another_network() {
        let mainnet = crate::network::Network::Mainnet.params().unwrap();
        for address in [ALPHA_UA, ALPHA_TRANSPARENT] {
            let err = resolve_recipient(&mainnet, address).unwrap_err();
            assert!(
                matches!(&err, WalletError::UnsupportedRecipient { message }
                    if message.contains("not a valid address")),
                "{address}: {err:?}"
            );
        }
    }
}
