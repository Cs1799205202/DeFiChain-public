// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// Price of the collateral token in the debt token, scaled by 1e18.
interface IOracle {
    function price() external view returns (uint256);
}

/// Off-chain feed: a reporter pushes the "real" market price (like Chainlink, much simplified).
contract FeedOracle is IOracle {
    address public reporter;
    uint256 public override price;
    uint256 public updatedAt;

    event PriceUpdated(uint256 price, uint256 at);

    constructor(uint256 initialPrice) {
        reporter = msg.sender;
        price = initialPrice;
        updatedAt = block.timestamp;
    }

    function setPrice(uint256 newPrice) external {
        require(msg.sender == reporter, "only reporter");
        price = newPrice;
        updatedAt = block.timestamp;
        emit PriceUpdated(newPrice, block.timestamp);
    }
}

interface ISpot {
    function spotPrice0() external view returns (uint256);
}

/// Reads the AMM spot price directly. Anyone who can move the pool can move this price:
/// the classic oracle-manipulation design flaw, kept on purpose as a lab variant.
contract AMMOracle is IOracle {
    ISpot public immutable pool;

    constructor(address pool_) {
        pool = ISpot(pool_);
    }

    function price() external view override returns (uint256) {
        return pool.spotPrice0();
    }
}
