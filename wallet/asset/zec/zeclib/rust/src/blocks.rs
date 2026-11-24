// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

use std::convert::Infallible;

use prost::Message;
use zcash_client_backend::{
    data_api::chain::{BlockSource, error::Error as ChainError},
    proto::compact_formats::CompactBlock,
};
use zcash_protocol::consensus::BlockHeight;

use crate::error::WalletError;

/// A batch of compact blocks held in memory, in the order they were given.
///
/// The scanner reads its input through [`BlockSource`]. The wallets built on
/// librustzcash usually back that with a cache of block files on disk, which
/// the block downloader fills. Here the caller passes each batch across the
/// FFI boundary directly, so the batch itself is the block source and nothing
/// is written to disk.
pub(crate) struct MemoryBlockSource {
    blocks: Vec<CompactBlock>,
}

impl MemoryBlockSource {
    /// Decodes protobuf-encoded compact blocks.
    pub(crate) fn decode(encoded: &[Vec<u8>]) -> Result<Self, WalletError> {
        let blocks = encoded
            .iter()
            .enumerate()
            .map(|(i, bytes)| {
                CompactBlock::decode(&bytes[..])
                    .map_err(|e| WalletError::invalid(format!("compact block {i}: {e}")))
            })
            .collect::<Result<Vec<_>, _>>()?;
        Ok(MemoryBlockSource { blocks })
    }

    pub(crate) fn len(&self) -> usize {
        self.blocks.len()
    }

    /// The height of the first block, if there is one.
    pub(crate) fn first_height(&self) -> Option<u64> {
        self.blocks.first().map(|b| b.height)
    }
}

impl BlockSource for MemoryBlockSource {
    type Error = Infallible;

    fn with_blocks<F, WalletErrT>(
        &self,
        from_height: Option<BlockHeight>,
        limit: Option<usize>,
        mut with_block: F,
    ) -> Result<(), ChainError<WalletErrT, Self::Error>>
    where
        F: FnMut(CompactBlock) -> Result<(), ChainError<WalletErrT, Self::Error>>,
    {
        let from_height = from_height.map_or(0, u64::from);
        self.blocks
            .iter()
            .filter(|b| b.height >= from_height)
            .take(limit.unwrap_or(usize::MAX))
            .try_for_each(|b| with_block(b.clone()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::empty_block;

    fn heights(source: &MemoryBlockSource, from: Option<u32>, limit: Option<usize>) -> Vec<u64> {
        let mut seen = vec![];
        source
            .with_blocks::<_, ()>(from.map(BlockHeight::from_u32), limit, |b| {
                seen.push(b.height);
                Ok(())
            })
            .unwrap();
        seen
    }

    fn source(range: std::ops::Range<u32>) -> MemoryBlockSource {
        let encoded: Vec<Vec<u8>> = range.map(|h| empty_block(h, 0).encode_to_vec()).collect();
        MemoryBlockSource::decode(&encoded).unwrap()
    }

    #[test]
    fn decodes_in_order() {
        let s = source(10..14);
        assert_eq!(s.len(), 4);
        assert_eq!(s.first_height(), Some(10));
        assert_eq!(heights(&s, None, None), vec![10, 11, 12, 13]);
    }

    #[test]
    fn empty_batch() {
        let s = MemoryBlockSource::decode(&[]).unwrap();
        assert_eq!(s.len(), 0);
        assert_eq!(s.first_height(), None);
        assert!(heights(&s, None, None).is_empty());
    }

    #[test]
    fn honors_from_height_and_limit() {
        let s = source(10..20);
        assert_eq!(heights(&s, Some(15), None), vec![15, 16, 17, 18, 19]);
        assert_eq!(heights(&s, Some(15), Some(2)), vec![15, 16]);
        assert_eq!(heights(&s, None, Some(3)), vec![10, 11, 12]);
        assert!(heights(&s, Some(20), None).is_empty());
    }

    #[test]
    fn stops_on_callback_error() {
        let s = source(10..20);
        let mut seen = 0;
        let res = s.with_blocks::<_, &str>(None, None, |_| {
            seen += 1;
            if seen == 3 {
                Err(ChainError::Wallet("stop"))
            } else {
                Ok(())
            }
        });
        assert!(matches!(res, Err(ChainError::Wallet("stop"))));
        assert_eq!(seen, 3);
    }

    #[test]
    fn rejects_malformed_blocks() {
        let good = empty_block(1, 0).encode_to_vec();
        // Field 1 with wire type 7, which protobuf does not define.
        let bad = vec![0x0f, 0xff, 0xff];
        let err = MemoryBlockSource::decode(&[good, bad]).err().unwrap();
        assert!(
            matches!(&err, WalletError::InvalidArgument { message }
                if message.starts_with("compact block 1:")),
            "{err:?}"
        );
    }
}
