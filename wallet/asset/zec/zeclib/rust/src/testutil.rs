// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//! Helpers shared by the unit tests: a regtest network and a synthetic chain
//! of blocks without transactions.

use prost::Message;
use zcash_client_backend::proto::{
    compact_formats::{ChainMetadata, CompactBlock},
    service::TreeState,
};

use crate::network::{ActivationHeights, Network};

/// Activation heights with every upgrade active from `height`.
pub(crate) fn all_at(height: u32) -> ActivationHeights {
    ActivationHeights {
        overwinter: Some(height),
        sapling: Some(height),
        blossom: Some(height),
        heartwood: Some(height),
        canopy: Some(height),
        nu5: Some(height),
        nu6: Some(height),
        nu6_1: Some(height),
        nu6_2: Some(height),
        nu6_3: Some(height),
        nu7: Some(height),
    }
}

/// A regtest network with every upgrade active from height 1, as in the
/// simnet harness.
pub(crate) fn regtest() -> Network {
    Network::Regtest { heights: all_at(1) }
}

/// The hash of the synthetic block at `height` on chain `fork`. Blocks on
/// different forks have different hashes at every height.
pub(crate) fn block_hash(height: u32, fork: u8) -> Vec<u8> {
    let mut hash = vec![fork; 32];
    hash[..4].copy_from_slice(&height.to_le_bytes());
    hash
}

/// A block without transactions at `height` on chain `fork`, whose parent is
/// the block below it on the same fork.
pub(crate) fn empty_block(height: u32, fork: u8) -> CompactBlock {
    CompactBlock {
        height: height.into(),
        hash: block_hash(height, fork),
        prev_hash: block_hash(height.saturating_sub(1), fork),
        time: 1_700_000_000 + height,
        header: vec![],
        vtx: vec![],
        chain_metadata: Some(ChainMetadata::default()),
    }
}

/// The encoded blocks of `range` on chain `fork`.
pub(crate) fn encoded_blocks(range: std::ops::Range<u32>, fork: u8) -> Vec<Vec<u8>> {
    range
        .map(|h| empty_block(h, fork).encode_to_vec())
        .collect()
}

/// The encoded tree state of the synthetic block at `height` on chain `fork`.
/// The note commitment trees are empty, which is consistent with a chain that
/// has no transactions.
pub(crate) fn tree_state(height: u32, fork: u8) -> Vec<u8> {
    let mut hash = block_hash(height, fork);
    // A tree state carries the hash as hex in display (reversed) byte order.
    hash.reverse();
    TreeState {
        network: "regtest".into(),
        height: height.into(),
        hash: hex::encode(hash),
        time: 1_700_000_000 + height,
        sapling_tree: String::new(),
        orchard_tree: String::new(),
        ironwood_tree: String::new(),
    }
    .encode_to_vec()
}
