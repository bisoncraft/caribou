// SPDX-License-Identifier: BlueOak-1.0.0
pragma solidity = 0.8.18;

interface IAggregatorV3 {
    function latestRoundData() external view returns (
        uint80 roundId,
        int256 answer,
        uint256 startedAt,
        uint256 updatedAt,
        uint80 answeredInRound
    );
    function decimals() external view returns (uint8);
}

contract MultiAggregatorV0 {
    struct Reading {
        int256 answer;
        uint256 updatedAt;
        uint8 decimals;
        bool ok;
    }

    // _decodeFeed is a pure helper so read() can use try/catch to isolate
    // abi.decode panics. Solidity's strict decoder rejects garbage-padded
    // words (e.g. a uint80 with non-zero upper bytes) even when the byte
    // length is correct, so a length check alone does not prevent a revert.
    function _decodeFeed(bytes memory d1, bytes memory d2)
        external pure
        returns (int256 answer, uint256 updatedAt, uint8 dec)
    {
        (, answer, , updatedAt, ) = abi.decode(d1, (uint80, int256, uint256, uint256, uint80));
        dec = abi.decode(d2, (uint8));
    }

    // read batches latestRoundData() and decimals() across all aggs in a
    // single eth_call. A failed staticcall or decode for any feed leaves that
    // entry's ok field false instead of reverting the whole batch.
    function read(address[] calldata aggs)
        external view
        returns (Reading[] memory out)
    {
        require(aggs.length <= 64, "too many aggregators");
        out = new Reading[](aggs.length);
        for (uint256 i; i < aggs.length; i++) {
            (bool s1, bytes memory d1) = aggs[i].staticcall{gas: 200_000}(
                abi.encodeWithSelector(IAggregatorV3.latestRoundData.selector));
            (bool s2, bytes memory d2) = aggs[i].staticcall{gas: 200_000}(
                abi.encodeWithSelector(IAggregatorV3.decimals.selector));
            // Fast-path: short returndata means an EOA or broken proxy.
            if (!s1 || !s2 || d1.length < 160 || d2.length < 32) {
                continue;
            }
            try this._decodeFeed(d1, d2) returns (int256 answer, uint256 updatedAt, uint8 dec) {
                out[i] = Reading({
                    answer: answer,
                    updatedAt: updatedAt,
                    decimals: dec,
                    ok: true
                });
            } catch {
                continue;
            }
        }
    }
}
