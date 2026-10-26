// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// Unit tests for the course contracts, run with Foundry: `cd contracts && forge test -vv`.
// No forge-std needed: a test is any public function named test*, and it fails if it reverts.

import "../Token.sol";
import "../AMM.sol";
import "../Oracle.sol";
import "../Lending.sol";

/// The few Foundry cheatcodes used here (normally from forge-std's Vm.sol).
interface Vm {
    function prank(address) external;
    function startPrank(address) external;
    function stopPrank() external;
    function warp(uint256) external;
    function expectRevert(bytes calldata) external;
}

contract DeFiTest {
    Vm constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));
    Token usd;
    Token weth;
    AMM pool;
    FeedOracle feed;
    Lending lending;
    address alice = address(0xA11CE);
    address bob = address(0xB0B);

    function setUp() public {
        usd = new Token("US Dollar", "USD");
        weth = new Token("Wrapped Ether", "WETH");
        pool = new AMM(address(weth), address(usd));
        feed = new FeedOracle(2000e18);
        lending = new Lending(address(weth), address(usd), address(feed), 0);
        // Pool at 2000 USD/WETH, lending market with 1M USD to lend.
        weth.mint(address(this), 100e18);
        usd.mint(address(this), 200_000e18 + 1_000_000e18);
        weth.approve(address(pool), type(uint256).max);
        usd.approve(address(pool), type(uint256).max);
        pool.addLiquidity(100e18, 200_000e18);
        usd.transfer(address(lending), 1_000_000e18);
        for (uint256 i = 0; i < 2; i++) {
            address who = i == 0 ? alice : bob;
            weth.mint(who, 100e18);
            usd.mint(who, 100_000e18);
            vm.startPrank(who);
            weth.approve(address(pool), type(uint256).max);
            usd.approve(address(pool), type(uint256).max);
            weth.approve(address(lending), type(uint256).max);
            usd.approve(address(lending), type(uint256).max);
            vm.stopPrank();
        }
    }

    function testSpotPrice() public view {
        require(pool.spotPrice0() == 2000e18, "initial price");
    }

    function testSwapFollowsConstantProduct() public {
        uint256 k = pool.reserve0() * pool.reserve1();
        vm.prank(alice);
        uint256 out = pool.swap(address(weth), 10e18, 0);
        // dy = y * dx' / (x + dx'), dx' = 10 * 0.997: about 18132 USD.
        require(out > 18_132e18 && out < 18_133e18, "amount out");
        require(pool.reserve0() * pool.reserve1() >= k, "k must not decrease (fees stay in the pool)");
        require(pool.spotPrice0() < 2000e18, "selling WETH lowers its price");
    }

    function testSlippageGuard() public {
        vm.prank(alice);
        vm.expectRevert(bytes("slippage"));
        pool.swap(address(weth), 1e18, 1_000_000e18);
    }

    function testLiquidityRoundTrip() public {
        uint256 shares = pool.shares(address(this));
        (uint256 a0, uint256 a1) = pool.removeLiquidity(shares / 2);
        require(a0 == 50e18 && a1 == 100_000e18, "half the pool back");
    }

    function testBorrowLimitIsLTV() public {
        vm.startPrank(alice);
        lending.deposit(10e18); // worth 20,000 USD: can borrow up to 15,000
        vm.expectRevert(bytes("exceeds LTV"));
        lending.borrow(15_001e18);
        lending.borrow(15_000e18);
        vm.stopPrank();
        require(lending.healthFactor(alice) > 1e18, "healthy right after borrowing");
    }

    function testLiquidationAfterPriceDrop() public {
        vm.startPrank(alice);
        lending.deposit(10e18);
        lending.borrow(14_000e18);
        vm.stopPrank();
        vm.prank(bob);
        vm.expectRevert(bytes("healthy"));
        lending.liquidate(alice, 1_000e18);

        feed.setPrice(1500e18); // 10 WETH now worth 15,000: 80% threshold is 12,000 < 14,000 debt
        require(lending.healthFactor(alice) < 1e18, "unhealthy after the drop");
        uint256 before = weth.balanceOf(bob);
        vm.prank(bob);
        (uint256 seized, uint256 paid) = lending.liquidate(alice, 7_000e18); // the close factor caps it at 50%
        require(paid == 7_000e18, "repaid half the debt");
        require(seized == 4.9e18 && weth.balanceOf(bob) - before == seized, "7000 * 1.05 / 1500 = 4.9 WETH");
    }

    function testOnlyReporterSetsPrice() public {
        vm.prank(alice);
        vm.expectRevert(bytes("only reporter"));
        feed.setPrice(1e18);
    }

    function testInterestAccrues() public {
        Lending l = new Lending(address(weth), address(usd), address(feed), 1e12); // 1e-6 per second
        usd.mint(address(l), 100_000e18);
        vm.startPrank(alice);
        weth.approve(address(l), type(uint256).max);
        l.deposit(10e18);
        l.borrow(1_000e18);
        vm.stopPrank();
        uint256 d0 = l.debtOf(alice);
        vm.warp(block.timestamp + 1 days);
        require(l.debtOf(alice) > d0, "debt grows with time");
    }
}
