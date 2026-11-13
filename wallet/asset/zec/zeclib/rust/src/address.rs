// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Address helpers that need no wallet database.

use zcash_keys::address::{Address, UnifiedAddress};

use crate::{
    error::{WalletError, guard},
    network::{Network, Params},
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::{all_at, regtest};

    // The unified, Sapling and transparent addresses of account 0 of the
    // alpha funder wallet in the simnet harness (util/testing/zec).
    const ALPHA_UA: &str = "uregtest13pvma5dl78lka0cl83u4jmm78emspd6rm9e56xgm3ttk4pxcwcr4guhdc5u8mg7z3fyr9zjec593khe5m4c3a825yx22lcjwm4anp2xvu4ej5frpc5dm25294c8w3x0pnnxjde38tz4fpgaglrrhx0k7z5259slahnxng23mhn8lhhff8dkeznyhynrfwlu3e8mzedknqathcdlv7hd";
    const ALPHA_SAPLING: &str = "zregtestsapling1uwt3xqagtnemsucergl4f5lcjj0qsh4hu2avqs6gp3fq62yke38dcyxg5ddewwx5pnjrkfmrr6w";
    const ALPHA_TRANSPARENT: &str = "tmQbvCAJZfBjBztopcwVDbujsC33vFH1whm";

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
