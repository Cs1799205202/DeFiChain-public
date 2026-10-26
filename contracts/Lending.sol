// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "./Oracle.sol";

interface IERC20L {
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
}

/// Over-collateralised lending, Aave/Compound simplified to one market:
/// deposit `collateral` (e.g. WETH), borrow `debt` (e.g. USD) up to LTV.
/// Positions whose debt exceeds the liquidation threshold can be liquidated by anyone,
/// who repays part of the debt and seizes collateral plus a bonus.
contract Lending {
    IERC20L public immutable collateral;
    IERC20L public immutable debt;
    IOracle public immutable oracle;

    uint256 public constant LTV_BPS = 7500;               // borrow up to 75% of collateral value
    uint256 public constant LIQ_THRESHOLD_BPS = 8000;     // liquidatable above 80%
    uint256 public constant LIQ_BONUS_BPS = 500;          // liquidator receives 5% extra collateral
    uint256 public constant CLOSE_FACTOR_BPS = 5000;      // at most 50% of the debt per liquidation

    /// Simple interest per second, scaled by 1e18. Exaggerated so it shows in a short run.
    uint256 public immutable ratePerSecond;
    uint256 public borrowIndex = 1e18;
    uint256 public lastAccrual;
    uint256 public totalDebtShares;

    mapping(address => uint256) public collateralOf;
    mapping(address => uint256) public debtSharesOf;
    address[] public borrowers;
    mapping(address => bool) private known;

    event Deposit(address indexed user, uint256 amount);
    event Withdraw(address indexed user, uint256 amount);
    event Borrow(address indexed user, uint256 amount);
    event Repay(address indexed user, uint256 amount);
    event Liquidate(address indexed liquidator, address indexed user, uint256 repaid, uint256 seized);

    constructor(address collateral_, address debt_, address oracle_, uint256 ratePerSecond_) {
        collateral = IERC20L(collateral_);
        debt = IERC20L(debt_);
        oracle = IOracle(oracle_);
        ratePerSecond = ratePerSecond_;
        lastAccrual = block.timestamp;
    }

    function accrue() public {
        uint256 dt = block.timestamp - lastAccrual;
        if (dt == 0) return;
        borrowIndex += (borrowIndex * ratePerSecond * dt) / 1e18;
        lastAccrual = block.timestamp;
    }

    function debtOf(address user) public view returns (uint256) {
        uint256 index = borrowIndex;
        uint256 dt = block.timestamp - lastAccrual;
        if (dt > 0) index += (index * ratePerSecond * dt) / 1e18;
        return (debtSharesOf[user] * index + 1e18 - 1) / 1e18; // round debt up
    }

    function collateralValue(address user) public view returns (uint256) {
        return (collateralOf[user] * oracle.price()) / 1e18;
    }

    /// Health factor scaled by 1e18; below 1e18 the position can be liquidated.
    function healthFactor(address user) public view returns (uint256) {
        uint256 d = debtOf(user);
        if (d == 0) return type(uint256).max;
        return (collateralValue(user) * LIQ_THRESHOLD_BPS * 1e18) / (d * 10000);
    }

    function borrowersCount() external view returns (uint256) {
        return borrowers.length;
    }

    function deposit(uint256 amount) external {
        accrue();
        require(collateral.transferFrom(msg.sender, address(this), amount), "transfer");
        collateralOf[msg.sender] += amount;
        emit Deposit(msg.sender, amount);
    }

    function withdraw(uint256 amount) external {
        accrue();
        require(collateralOf[msg.sender] >= amount, "collateral");
        collateralOf[msg.sender] -= amount;
        require(debtOf(msg.sender) * 10000 <= collateralValue(msg.sender) * LTV_BPS, "would be undercollateralised");
        require(collateral.transfer(msg.sender, amount), "transfer");
        emit Withdraw(msg.sender, amount);
    }

    function borrow(uint256 amount) external {
        accrue();
        uint256 newShares = (amount * 1e18 + borrowIndex - 1) / borrowIndex;
        debtSharesOf[msg.sender] += newShares;
        totalDebtShares += newShares;
        require(debtOf(msg.sender) * 10000 <= collateralValue(msg.sender) * LTV_BPS, "exceeds LTV");
        if (!known[msg.sender]) { known[msg.sender] = true; borrowers.push(msg.sender); }
        require(debt.transfer(msg.sender, amount), "pool liquidity");
        emit Borrow(msg.sender, amount);
    }

    /// Repay up to `amount`; pass type(uint256).max to repay everything.
    function repay(uint256 amount) external returns (uint256 paid) {
        accrue();
        paid = _reduceDebt(msg.sender, amount);
        require(debt.transferFrom(msg.sender, address(this), paid), "transfer");
        emit Repay(msg.sender, paid);
    }

    function liquidate(address user, uint256 repayAmount) external returns (uint256 seized, uint256 paid) {
        accrue();
        require(healthFactor(user) < 1e18, "healthy");
        uint256 maxRepay = (debtOf(user) * CLOSE_FACTOR_BPS) / 10000;
        if (repayAmount > maxRepay) repayAmount = maxRepay;
        // Compute the uncapped seizure to check whether collateral is the binding constraint.
        // If seizure would exceed collateral, scale repayAmount down so the liquidator pays
        // exactly for what they receive, preventing the "donation" bug where repay is charged
        // at the uncapped amount while only a fraction of collateral is actually seized.
        uint256 price = oracle.price();
        seized = (repayAmount * (10000 + LIQ_BONUS_BPS) * 1e18) / (10000 * price);
        if (seized > collateralOf[user]) {
            seized = collateralOf[user];
            // Derive the repayAmount that corresponds to exactly the available collateral.
            repayAmount = (seized * 10000 * price) / ((10000 + LIQ_BONUS_BPS) * 1e18);
        }
        paid = _reduceDebt(user, repayAmount);
        collateralOf[user] -= seized;
        require(debt.transferFrom(msg.sender, address(this), paid), "transfer");
        require(collateral.transfer(msg.sender, seized), "transfer");
        emit Liquidate(msg.sender, user, paid, seized);
    }

    function _reduceDebt(address user, uint256 amount) internal returns (uint256 paid) {
        uint256 owed = (debtSharesOf[user] * borrowIndex + 1e18 - 1) / 1e18;
        paid = amount > owed ? owed : amount;
        uint256 burned = paid == owed ? debtSharesOf[user] : (paid * 1e18) / borrowIndex;
        debtSharesOf[user] -= burned;
        totalDebtShares -= burned;
    }
}
