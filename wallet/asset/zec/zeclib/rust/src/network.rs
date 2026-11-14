// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

use zcash_protocol::{
    consensus::{self, BlockHeight, NetworkType, NetworkUpgrade, Parameters},
    local_consensus::LocalNetwork,
};

use crate::error::WalletError;

/// The activation height of each network upgrade on a regtest chain. `None`
/// means the upgrade never activates.
///
/// The heights must match the node's configuration. Transactions built
/// against different heights carry the wrong consensus branch ID and are
/// rejected.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Record)]
pub struct ActivationHeights {
    pub overwinter: Option<u32>,
    pub sapling: Option<u32>,
    pub blossom: Option<u32>,
    pub heartwood: Option<u32>,
    pub canopy: Option<u32>,
    pub nu5: Option<u32>,
    pub nu6: Option<u32>,
    pub nu6_1: Option<u32>,
    pub nu6_2: Option<u32>,
    /// NU6.3, which adds the Ironwood shielded pool.
    pub nu6_3: Option<u32>,
    pub nu7: Option<u32>,
}

impl ActivationHeights {
    /// The upgrades in activation order, paired with their configured height.
    fn in_order(&self) -> [(&'static str, Option<u32>); 11] {
        [
            ("overwinter", self.overwinter),
            ("sapling", self.sapling),
            ("blossom", self.blossom),
            ("heartwood", self.heartwood),
            ("canopy", self.canopy),
            ("nu5", self.nu5),
            ("nu6", self.nu6),
            ("nu6_1", self.nu6_1),
            ("nu6_2", self.nu6_2),
            ("nu6_3", self.nu6_3),
            ("nu7", self.nu7),
        ]
    }

    /// Checks that the heights describe a chain a wallet can follow: NU6.3
    /// activates, no upgrade activates before its predecessor, and no upgrade
    /// activates when its predecessor never does. Together these make every
    /// upgrade up to NU6.3 activate.
    fn validate(&self) -> Result<(), WalletError> {
        if self.nu6_3.is_none() {
            return Err(WalletError::invalid(
                "regtest activation heights: nu6_3 must activate, the wallet holds its \
                 shielded funds in the Ironwood pool",
            ));
        }
        let mut prev: Option<(&str, Option<u32>)> = None;
        for (name, height) in self.in_order() {
            if let (Some((prev_name, prev_height)), Some(height)) = (prev, height) {
                match prev_height {
                    None => {
                        return Err(WalletError::invalid(format!(
                            "regtest activation heights: {name} activates but {prev_name} does not"
                        )));
                    }
                    Some(prev_height) if prev_height > height => {
                        return Err(WalletError::invalid(format!(
                            "regtest activation heights: {name} activates at {height}, before \
                             {prev_name} at {prev_height}"
                        )));
                    }
                    Some(_) => {}
                }
            }
            prev = Some((name, height));
        }
        Ok(())
    }

    fn to_local_network(self) -> LocalNetwork {
        let h = |v: Option<u32>| v.map(BlockHeight::from_u32);
        LocalNetwork {
            overwinter: h(self.overwinter),
            sapling: h(self.sapling),
            blossom: h(self.blossom),
            heartwood: h(self.heartwood),
            canopy: h(self.canopy),
            nu5: h(self.nu5),
            nu6: h(self.nu6),
            nu6_1: h(self.nu6_1),
            nu6_2: h(self.nu6_2),
            nu6_3: h(self.nu6_3),
            nu7: h(self.nu7),
        }
    }
}

/// The Zcash network a wallet belongs to.
#[derive(Clone, Copy, Debug, PartialEq, Eq, uniffi::Enum)]
pub enum Network {
    Mainnet,
    Testnet,
    /// A local regtest chain with the given upgrade schedule.
    Regtest {
        heights: ActivationHeights,
    },
}

impl Network {
    pub(crate) fn params(self) -> Result<Params, WalletError> {
        match self {
            Network::Mainnet => Ok(Params::Main),
            Network::Testnet => Ok(Params::Test),
            Network::Regtest { heights } => {
                heights.validate()?;
                Ok(Params::Regtest(heights.to_local_network()))
            }
        }
    }
}

/// Consensus parameters for any supported network, as the single concrete
/// type that the wallet database is parameterized by.
#[derive(Clone, Copy, Debug)]
pub(crate) enum Params {
    Main,
    Test,
    Regtest(LocalNetwork),
}

impl Params {
    /// The activation height of NU6.3, the first block that can hold funds in
    /// the Ironwood pool.
    pub(crate) fn ironwood_activation(&self) -> BlockHeight {
        self.activation_height(NetworkUpgrade::Nu6_3)
            .expect("NU6.3 activates on every network the wallet opens")
    }
}

impl Parameters for Params {
    fn network_type(&self) -> NetworkType {
        match self {
            Params::Main => NetworkType::Main,
            Params::Test => NetworkType::Test,
            Params::Regtest(_) => NetworkType::Regtest,
        }
    }

    fn activation_height(&self, nu: NetworkUpgrade) -> Option<BlockHeight> {
        match self {
            Params::Main => consensus::MAIN_NETWORK.activation_height(nu),
            Params::Test => consensus::TEST_NETWORK.activation_height(nu),
            Params::Regtest(local) => local.activation_height(nu),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::all_at;

    #[test]
    fn mainnet_and_testnet_use_built_in_parameters() {
        let main = Network::Mainnet.params().unwrap();
        assert_eq!(main.network_type(), NetworkType::Main);
        assert_eq!(
            main.activation_height(NetworkUpgrade::Sapling),
            Some(BlockHeight::from_u32(419_200))
        );
        assert_eq!(main.ironwood_activation(), BlockHeight::from_u32(3_428_143));

        let test = Network::Testnet.params().unwrap();
        assert_eq!(test.network_type(), NetworkType::Test);
        assert_eq!(
            test.activation_height(NetworkUpgrade::Sapling),
            Some(BlockHeight::from_u32(280_000))
        );
        assert_eq!(test.ironwood_activation(), BlockHeight::from_u32(4_134_000));
    }

    #[test]
    fn regtest_uses_configured_heights() {
        let mut heights = all_at(1);
        heights.nu5 = Some(5);
        heights.nu6 = Some(5);
        heights.nu6_1 = Some(9);
        heights.nu6_2 = Some(9);
        heights.nu6_3 = Some(12);
        heights.nu7 = None;
        let params = Network::Regtest { heights }.params().unwrap();

        assert_eq!(params.network_type(), NetworkType::Regtest);
        assert_eq!(
            params.activation_height(NetworkUpgrade::Canopy),
            Some(BlockHeight::from_u32(1))
        );
        assert_eq!(
            params.activation_height(NetworkUpgrade::Nu6_1),
            Some(BlockHeight::from_u32(9))
        );
        assert_eq!(params.ironwood_activation(), BlockHeight::from_u32(12));
        assert_eq!(params.activation_height(NetworkUpgrade::Nu7), None);
    }

    #[test]
    fn regtest_requires_ironwood() {
        let mut heights = all_at(1);
        heights.nu6_3 = None;
        heights.nu7 = None;
        let err = Network::Regtest { heights }.params().unwrap_err();
        assert!(
            matches!(&err, WalletError::InvalidArgument { message } if message.contains("nu6_3")),
            "{err:?}"
        );
    }

    #[test]
    fn regtest_rejects_a_gap_in_the_schedule() {
        let mut heights = all_at(1);
        heights.canopy = None;
        let err = Network::Regtest { heights }.params().unwrap_err();
        assert!(
            matches!(&err, WalletError::InvalidArgument { message }
                if message.contains("nu5 activates but canopy does not")),
            "{err:?}"
        );
    }

    #[test]
    fn regtest_rejects_out_of_order_heights() {
        let mut heights = all_at(10);
        heights.nu7 = Some(9);
        let err = Network::Regtest { heights }.params().unwrap_err();
        assert!(
            matches!(&err, WalletError::InvalidArgument { message }
                if message.contains("nu7 activates at 9, before nu6_3 at 10")),
            "{err:?}"
        );
    }

    #[test]
    fn regtest_allows_a_schedule_that_ends_at_ironwood() {
        let mut heights = all_at(1);
        heights.nu7 = None;
        assert!(Network::Regtest { heights }.params().is_ok());
    }
}
