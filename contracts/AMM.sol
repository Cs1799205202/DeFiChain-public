// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function balanceOf(address who) external view returns (uint256);
}

/// Constant-product market maker (Uniswap V2 core, simplified): reserve0 * reserve1 = k,
/// 0.3% fee on the input amount. LP shares are tracked in this contract, not as a token.
contract AMM {
    IERC20 public immutable token0;
    IERC20 public immutable token1;
    uint256 public reserve0;
    uint256 public reserve1;
    uint256 public totalShares;
    mapping(address => uint256) public shares;

    uint256 public constant FEE_BPS = 30; // 0.30%

    event Swap(address indexed trader, address tokenIn, uint256 amountIn, uint256 amountOut, uint256 reserve0, uint256 reserve1);
    event AddLiquidity(address indexed provider, uint256 amount0, uint256 amount1, uint256 shares);
    event RemoveLiquidity(address indexed provider, uint256 amount0, uint256 amount1, uint256 shares);

    constructor(address token0_, address token1_) {
        token0 = IERC20(token0_);
        token1 = IERC20(token1_);
    }

    /// Output for an exact input, after the fee: dy = y * dx' / (x + dx'), dx' = dx * (1 - fee).
    function getAmountOut(uint256 amountIn, uint256 reserveIn, uint256 reserveOut) public pure returns (uint256) {
        require(amountIn > 0 && reserveIn > 0 && reserveOut > 0, "bad amounts");
        uint256 inWithFee = amountIn * (10000 - FEE_BPS);
        return (inWithFee * reserveOut) / (reserveIn * 10000 + inWithFee);
    }

    /// Spot price of token0 in token1, scaled by 1e18. Manipulable within one block.
    function spotPrice0() external view returns (uint256) {
        require(reserve0 > 0, "empty pool");
        return (reserve1 * 1e18) / reserve0;
    }

    function swap(address tokenIn, uint256 amountIn, uint256 minAmountOut) external returns (uint256 amountOut) {
        bool zeroForOne = tokenIn == address(token0);
        require(zeroForOne || tokenIn == address(token1), "unknown token");
        (IERC20 inTok, IERC20 outTok, uint256 rIn, uint256 rOut) =
            zeroForOne ? (token0, token1, reserve0, reserve1) : (token1, token0, reserve1, reserve0);
        amountOut = getAmountOut(amountIn, rIn, rOut);
        require(amountOut >= minAmountOut, "slippage");
        require(inTok.transferFrom(msg.sender, address(this), amountIn), "transfer in");
        require(outTok.transfer(msg.sender, amountOut), "transfer out");
        if (zeroForOne) { reserve0 += amountIn; reserve1 -= amountOut; }
        else { reserve1 += amountIn; reserve0 -= amountOut; }
        emit Swap(msg.sender, tokenIn, amountIn, amountOut, reserve0, reserve1);
    }

    /// Deposit at the current ratio (the first provider sets it). Excess of one side is not refunded,
    /// so callers should quote with the reserves first.
    function addLiquidity(uint256 amount0, uint256 amount1) external returns (uint256 minted) {
        require(token0.transferFrom(msg.sender, address(this), amount0), "transfer0");
        require(token1.transferFrom(msg.sender, address(this), amount1), "transfer1");
        if (totalShares == 0) {
            minted = sqrt(amount0 * amount1);
        } else {
            uint256 s0 = (amount0 * totalShares) / reserve0;
            uint256 s1 = (amount1 * totalShares) / reserve1;
            minted = s0 < s1 ? s0 : s1;
        }
        require(minted > 0, "zero shares");
        shares[msg.sender] += minted;
        totalShares += minted;
        reserve0 += amount0;
        reserve1 += amount1;
        emit AddLiquidity(msg.sender, amount0, amount1, minted);
    }

    function removeLiquidity(uint256 burned) external returns (uint256 amount0, uint256 amount1) {
        require(burned > 0 && shares[msg.sender] >= burned, "shares");
        amount0 = (burned * reserve0) / totalShares;
        amount1 = (burned * reserve1) / totalShares;
        shares[msg.sender] -= burned;
        totalShares -= burned;
        reserve0 -= amount0;
        reserve1 -= amount1;
        require(token0.transfer(msg.sender, amount0), "transfer0");
        require(token1.transfer(msg.sender, amount1), "transfer1");
        emit RemoveLiquidity(msg.sender, amount0, amount1, burned);
    }

    function sqrt(uint256 y) internal pure returns (uint256 z) {
        if (y > 3) {
            z = y;
            uint256 x = y / 2 + 1;
            while (x < z) { z = x; x = (y / x + x) / 2; }
        } else if (y != 0) {
            z = 1;
        }
    }
}
