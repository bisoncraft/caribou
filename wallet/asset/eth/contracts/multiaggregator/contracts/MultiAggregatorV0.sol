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

    // read batches latestRoundData() and decimals() across all aggs in a
    // single eth_call. A failed staticcall for any feed leaves that entry's
    // ok field false instead of reverting the whole batch.
    function read(address[] calldata aggs)
        external view
        returns (Reading[] memory out)
    {
        out = new Reading[](aggs.length);
        for (uint256 i; i < aggs.length; i++) {
            (bool s1, bytes memory d1) = aggs[i].staticcall(
                abi.encodeWithSelector(IAggregatorV3.latestRoundData.selector));
            (bool s2, bytes memory d2) = aggs[i].staticcall(
                abi.encodeWithSelector(IAggregatorV3.decimals.selector));
            if (!s1 || !s2) {
                continue;
            }
            (, int256 answer, , uint256 updatedAt, ) =
                abi.decode(d1, (uint80, int256, uint256, uint256, uint80));
            uint8 dec = abi.decode(d2, (uint8));
            out[i] = Reading({
                answer: answer,
                updatedAt: updatedAt,
                decimals: dec,
                ok: true
            });
        }
    }
}
