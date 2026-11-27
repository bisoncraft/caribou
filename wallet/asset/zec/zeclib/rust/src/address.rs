// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Address helpers that need no wallet database.

use zcash_address::unified::{MetadataTypecode, Typecode};
use zcash_keys::address::{Address, UnifiedAddress};

use crate::{
    error::{WalletError, guard},
    network::{Network, Params},
    recipient::{PaidPool, resolve_recipient, transparent_fallback},
};

fn decode_unified(params: &Params, address: &str) -> Result<UnifiedAddress, WalletError> {
    match Address::decode(params, address) {
        Some(Address::Unified(ua)) => Ok(*ua),
        Some(_) => Err(WalletError::invalid("not a unified address")),
        None => Err(WalletError::invalid("not a valid address for this network")),
    }
}

/// Returns the transparent receiver of a unified address as a transparent
/// address, or nothing when the unified address has no transparent receiver.
#[uniffi::export]
pub fn transparent_receiver(
    network: Network,
    unified_address: String,
) -> Result<Option<String>, WalletError> {
    guard(|| {
        let params = network.params()?;
        let ua = decode_unified(&params, &unified_address)?;
        Ok(ua
            .transparent()
            .map(|ta| Address::Transparent(*ta).encode(&params)))
    })
}

/// The kind of an address.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum AddressKind {
    Transparent,
    Unified,
    Sapling,
    /// A ZIP 320 transparent-source-only address.
    Tex,
}

/// The kind of a receiver of a unified address.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum ReceiverKind {
    /// An Orchard receiver, which receives into the Ironwood pool from NU6.3
    /// on.
    Orchard,
    Sapling,
    Transparent,
}

/// A receiver of a unified address.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct Receiver {
    pub kind: ReceiverKind,
    /// An address a sender can pay this receiver at on its own, as the
    /// wallet itself names it when paying that receiver alone: the unified
    /// address reduced to the receiver and the address's expiry, or for a
    /// transparent receiver of an address without an expiry, the bare
    /// transparent address. Absent for a Sapling receiver, which the wallet
    /// does not pay.
    pub address: Option<String>,
}

/// What [`parse_address`] finds in an address.
#[derive(Clone, Debug, PartialEq, Eq, uniffi::Record)]
pub struct ParsedAddress {
    pub kind: AddressKind,
    /// The receivers of a unified address, Orchard first, then Sapling, then
    /// transparent. Empty for any other kind.
    pub receivers: Vec<Receiver>,
    /// The expiry height and time of a unified address.
    pub expiry_height: Option<u32>,
    pub expiry_time: Option<u64>,
    /// Whether the wallet can pay the address. A payment to it lands in
    /// `paid_pool` and names `paid_address`, as a send would. When it cannot,
    /// `unpayable` says why. The expiry is not considered, because parsing
    /// knows neither the chain tip nor the time: a caller compares
    /// `expiry_height` with its tip and `expiry_time` with the clock.
    pub payable: bool,
    pub paid_pool: Option<PaidPool>,
    pub paid_address: Option<String>,
    pub unpayable: Option<String>,
}

/// Parses an address of `network`: its kind, the receivers of a unified
/// address, its expiry, and whether and how the wallet pays it. It takes no
/// database lock and makes no network request, so it suits validating input
/// as it is typed. It does not say whether the address is the wallet's own;
/// that is the caller's business ([`Wallet::owns_address`]).
///
/// An address that is not valid for the network fails with
/// [`WalletError::InvalidArgument`].
///
/// [`Wallet::owns_address`]: crate::Wallet::owns_address
#[uniffi::export]
pub fn parse_address(network: Network, address: String) -> Result<ParsedAddress, WalletError> {
    guard(|| {
        let params = network.params()?;
        let decoded = Address::decode(&params, &address)
            .ok_or_else(|| WalletError::invalid("not a valid address for this network"))?;
        let mut parsed = ParsedAddress {
            kind: AddressKind::Transparent,
            receivers: vec![],
            expiry_height: None,
            expiry_time: None,
            payable: false,
            paid_pool: None,
            paid_address: None,
            unpayable: None,
        };
        match &decoded {
            Address::Transparent(_) => {}
            Address::Sapling(_) => parsed.kind = AddressKind::Sapling,
            Address::Tex(_) => parsed.kind = AddressKind::Tex,
            Address::Unified(ua) => {
                parsed.kind = AddressKind::Unified;
                parsed.expiry_height = ua.expiry_height().map(u32::from);
                parsed.expiry_time = ua.expiry_time();
                if let Some(orchard) = orchard_only(&params, ua)? {
                    parsed.receivers.push(Receiver {
                        kind: ReceiverKind::Orchard,
                        address: Some(orchard),
                    });
                }
                if ua.sapling().is_some() {
                    parsed.receivers.push(Receiver {
                        kind: ReceiverKind::Sapling,
                        address: None,
                    });
                }
                if let Some(t) = ua.transparent() {
                    parsed.receivers.push(Receiver {
                        kind: ReceiverKind::Transparent,
                        address: Some(transparent_fallback(&params, ua, t)?.encode()),
                    });
                }
            }
        }
        match resolve_recipient(&params, &address) {
            Ok(r) => {
                parsed.payable = true;
                parsed.paid_pool = Some(r.pool);
                parsed.paid_address = Some(r.address.encode());
            }
            Err(WalletError::UnsupportedRecipient { message }) => parsed.unpayable = Some(message),
            Err(e) => return Err(e),
        }
        Ok(parsed)
    })
}

/// The Orchard receiver of a unified address as an address a sender can pay
/// on its own: the address pruned to its Orchard receiver, with its expiry
/// metadata kept. None for an address without an Orchard receiver.
/// [`parse_address`] and the history name a receiver this way, so the two
/// agree byte for byte.
pub(crate) fn orchard_only(
    params: &Params,
    ua: &UnifiedAddress,
) -> Result<Option<String>, WalletError> {
    if ua.orchard().is_none() {
        return Ok(None);
    }
    let orchard = ua
        .prune_retaining(&[
            Typecode::ORCHARD,
            Typecode::Metadata(MetadataTypecode::ExpiryHeight),
            Typecode::Metadata(MetadataTypecode::ExpiryTime),
        ])
        .map_err(|e| WalletError::invalid(format!("the Orchard receiver: {e}")))?
        .expect("the address has an Orchard receiver");
    Ok(Some(orchard.encode(params)))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::{all_at, regtest};

    // The unified, Sapling and transparent addresses of account 0 of the
    // alpha funder wallet in the simnet harness (util/testing/zec).
    const ALPHA_UA: &str = "uregtest13pvma5dl78lka0cl83u4jmm78emspd6rm9e56xgm3ttk4pxcwcr4guhdc5u8mg7z3fyr9zjec593khe5m4c3a825yx22lcjwm4anp2xvu4ej5frpc5dm25294c8w3x0pnnxjde38tz4fpgaglrrhx0k7z5259slahnxng23mhn8lhhff8dkeznyhynrfwlu3e8mzedknqathcdlv7hd";
    const ALPHA_SAPLING: &str = "zregtestsapling1uwt3xqagtnemsucergl4f5lcjj0qsh4hu2avqs6gp3fq62yke38dcyxg5ddewwx5pnjrkfmrr6w";
    const ALPHA_TRANSPARENT: &str = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm";

    fn orchard_only(params: &Params, ua: &str) -> bool {
        match Address::decode(params, ua) {
            Some(Address::Unified(ua)) => {
                ua.orchard().is_some() && ua.sapling().is_none() && ua.transparent().is_none()
            }
            _ => false,
        }
    }

    #[test]
    fn parses_addresses() {
        let params = regtest().params().unwrap();

        let p = parse_address(regtest(), ALPHA_UA.into()).unwrap();
        assert_eq!(p.kind, AddressKind::Unified);
        assert_eq!(
            p.receivers.iter().map(|r| r.kind).collect::<Vec<_>>(),
            vec![
                ReceiverKind::Orchard,
                ReceiverKind::Sapling,
                ReceiverKind::Transparent
            ]
        );
        assert!(orchard_only(
            &params,
            p.receivers[0].address.as_ref().unwrap()
        ));
        assert_eq!(p.receivers[1].address, None);
        assert_eq!(p.receivers[2].address.as_deref(), Some(ALPHA_TRANSPARENT));
        assert_eq!((p.expiry_height, p.expiry_time), (None, None));
        assert!(p.payable);
        assert_eq!(p.paid_pool, Some(PaidPool::Ironwood));
        assert_eq!(p.paid_address.as_deref(), Some(ALPHA_UA));

        let p = parse_address(regtest(), ALPHA_TRANSPARENT.into()).unwrap();
        assert_eq!(
            (p.kind, p.receivers.len(), p.payable),
            (AddressKind::Transparent, 0, true)
        );
        assert_eq!(p.paid_pool, Some(PaidPool::Transparent));
        assert_eq!(p.paid_address.as_deref(), Some(ALPHA_TRANSPARENT));

        let p = parse_address(regtest(), ALPHA_SAPLING.into()).unwrap();
        assert_eq!((p.kind, p.payable), (AddressKind::Sapling, false));
        assert!(
            p.unpayable
                .as_deref()
                .is_some_and(|m| m.contains("Sapling"))
        );
        assert_eq!((p.paid_pool, p.paid_address), (None, None));

        let Some(Address::Transparent(transparent::address::TransparentAddress::PublicKeyHash(
            hash,
        ))) = Address::decode(&params, ALPHA_TRANSPARENT)
        else {
            panic!("not a P2PKH address");
        };
        let tex = Address::Tex(hash).encode(&params);
        let p = parse_address(regtest(), tex).unwrap();
        assert_eq!((p.kind, p.payable), (AddressKind::Tex, false));
    }

    #[test]
    fn the_orchard_receiver_keeps_the_expiry() {
        let params = regtest().params().unwrap();
        let Some(Address::Unified(alpha)) = Address::decode(&params, ALPHA_UA) else {
            panic!("ALPHA_UA is not a unified address");
        };
        let ua = UnifiedAddress::from_receivers(
            alpha.orchard().copied(),
            None,
            alpha.transparent().copied(),
            Some(zcash_protocol::consensus::BlockHeight::from_u32(500)),
            Some(9_000),
        )
        .unwrap()
        .encode(&params);
        let p = parse_address(regtest(), ua).unwrap();
        assert_eq!((p.expiry_height, p.expiry_time), (Some(500), Some(9_000)));
        let Some(Address::Unified(orchard)) =
            Address::decode(&params, p.receivers[0].address.as_ref().unwrap())
        else {
            panic!("the Orchard receiver is not a unified address");
        };
        assert!(orchard.transparent().is_none());
        assert_eq!(orchard.expiry_height().map(u32::from), Some(500));
        assert_eq!(orchard.expiry_time(), Some(9_000));
        // The transparent receiver keeps the expiry too.
        let Some(Address::Unified(transparent)) =
            Address::decode(&params, p.receivers[1].address.as_ref().unwrap())
        else {
            panic!("the transparent receiver is not a unified address");
        };
        assert!(transparent.orchard().is_none());
        assert_eq!(transparent.transparent(), alpha.transparent());
        assert_eq!(transparent.expiry_height().map(u32::from), Some(500));
        assert_eq!(transparent.expiry_time(), Some(9_000));
    }

    #[test]
    fn parse_rejects_addresses_of_another_network() {
        for network in [Network::Mainnet, Network::Testnet] {
            assert!(matches!(
                parse_address(network, ALPHA_UA.into()),
                Err(WalletError::InvalidArgument { .. })
            ));
        }
        assert!(matches!(
            parse_address(regtest(), "nonsense".into()),
            Err(WalletError::InvalidArgument { .. })
        ));
    }

    #[test]
    fn extracts_the_transparent_receiver() {
        assert_eq!(
            transparent_receiver(regtest(), ALPHA_UA.into())
                .unwrap()
                .as_deref(),
            Some(ALPHA_TRANSPARENT)
        );
    }

    #[test]
    fn rejects_non_unified_addresses() {
        for addr in [ALPHA_SAPLING, ALPHA_TRANSPARENT] {
            let err = transparent_receiver(regtest(), addr.into()).unwrap_err();
            assert!(
                matches!(&err, WalletError::InvalidArgument { message }
                    if message == "not a unified address"),
                "{err:?}"
            );
        }
    }

    #[test]
    fn rejects_addresses_of_another_network() {
        for network in [Network::Mainnet, Network::Testnet] {
            let err = transparent_receiver(network, ALPHA_UA.into()).unwrap_err();
            assert!(
                matches!(&err, WalletError::InvalidArgument { message }
                    if message.contains("not a valid address")),
                "{err:?}"
            );
        }
        let err = transparent_receiver(regtest(), "not an address".into()).unwrap_err();
        assert!(
            matches!(err, WalletError::InvalidArgument { .. }),
            "{err:?}"
        );
    }

    #[test]
    fn rejects_an_invalid_network() {
        let mut heights = all_at(1);
        heights.nu6_3 = None;
        heights.nu7 = None;
        let err = transparent_receiver(Network::Regtest { heights }, ALPHA_UA.into()).unwrap_err();
        assert!(
            matches!(err, WalletError::InvalidArgument { .. }),
            "{err:?}"
        );
    }
}
