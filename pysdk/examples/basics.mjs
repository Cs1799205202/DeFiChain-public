// ethers.js v6 against a defichain node: deploy, call, events, revert reasons.
// npm install ethers@6 ; node pysdk/examples/basics.mjs [http://127.0.0.1:28001]
import { readFileSync } from "node:fs";
import { ethers } from "ethers";

const rpc = process.argv[2] ?? "http://127.0.0.1:28001";
const artifacts = process.argv[3] ?? new URL("../../client/artifacts", import.meta.url).pathname;
const provider = new ethers.JsonRpcProvider(rpc, undefined, { pollingInterval: 500 });
const net = await provider.getNetwork();
console.log(`chain id ${net.chainId}, block ${await provider.getBlockNumber()}`);

const key = ethers.keccak256(ethers.toUtf8Bytes("defichain-key:alice")); // devnet account, public key!
const alice = new ethers.Wallet(key, provider);
const bob = ethers.computeAddress(ethers.keccak256(ethers.toUtf8Bytes("defichain-key:bob")));
console.log(`alice ${alice.address}: ${ethers.formatEther(await provider.getBalance(alice.address))} ETH`);

const abi = JSON.parse(readFileSync(`${artifacts}/Token.abi`, "utf8"));
const bin = "0x" + readFileSync(`${artifacts}/Token.bin`, "utf8").trim();
const token = await new ethers.ContractFactory(abi, bin, alice).deploy("Ethers Coin", "ETC");
await token.waitForDeployment();
console.log(`token at ${await token.getAddress()}`);

await (await token.mint(alice.address, ethers.parseEther("100"))).wait();
const rc = await (await token.transfer(bob, ethers.parseEther("40"))).wait();
console.log(`transfer in block ${rc.blockNumber}, gas ${rc.gasUsed}, status ${rc.status}`);
console.log(`bob has ${ethers.formatEther(await token.balanceOf(bob))} ${await token.symbol()}`);
for (const ev of await token.queryFilter(token.filters.Transfer(), 0)) {
  console.log(`  Transfer ${ev.args.from.slice(0, 10)} -> ${ev.args.to.slice(0, 10)} ${ethers.formatEther(ev.args.value)}`);
}
try {
  await token.transfer(bob, ethers.parseEther("1000"));
} catch (e) {
  console.log(`rejected: ${e.reason ?? e.shortMessage}`);
}
// Live events (ethers polls eth_getFilterChanges under the hood).
let seen = 0;
await token.on("Transfer", (from, to, value) => { seen++; });
await (await token.transfer(bob, 1n)).wait();
await new Promise((r) => setTimeout(r, 1500));
console.log(`live Transfer events seen: ${seen}`);
await token.removeAllListeners();
