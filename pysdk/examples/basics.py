#!/usr/bin/env python3
"""Warm-up (Part 2, task 0): talk to your own chain with web3.py.

Start a devnet first (another terminal):   go run ./cmd/devnet
Then:                                     python3 pysdk/examples/basics.py [http://127.0.0.1:28001]

It deploys an ERC-20 token as alice, mints, transfers to bob, reads balances and the
Transfer events, and waits for confirmations. Every step is a standard Ethereum JSON-RPC
call: the same script works against any Ethereum node.
"""
import os
import sys
import time

from eth_account import Account
from eth_utils import keccak
from web3 import Web3

HERE = os.path.dirname(os.path.abspath(__file__))
ARTIFACTS = os.path.join(HERE, "..", "..", "client", "artifacts")


def key_for(name: str) -> str:
    """The devnet's named accounts: key = keccak256("defichain-key:" + name). Public!"""
    return "0x" + keccak(text="defichain-key:" + name).hex()


def load(name: str):
    with open(os.path.join(ARTIFACTS, name + ".abi")) as f:
        abi = f.read()
    with open(os.path.join(ARTIFACTS, name + ".bin")) as f:
        return abi, "0x" + f.read().strip()


def send(w3, acct, tx):
    """Fill in the fee fields, sign, send, wait for the receipt."""
    tx.update({"from": acct.address, "chainId": w3.eth.chain_id, "type": 2,
               "nonce": w3.eth.get_transaction_count(acct.address, "pending"),
               "maxPriorityFeePerGas": w3.to_wei(1, "gwei"), "maxFeePerGas": w3.to_wei(1, "gwei")})
    call = {k: tx[k] for k in ("from", "to", "data", "value") if k in tx}
    tx["gas"] = w3.eth.estimate_gas(call)  # fails here, with the reason, if it would revert
    signed = acct.sign_transaction(tx)
    raw = getattr(signed, "raw_transaction", None) or signed.rawTransaction
    h = w3.eth.send_raw_transaction(raw)
    rc = w3.eth.wait_for_transaction_receipt(h, timeout=60, poll_latency=0.2)
    print(f"  tx {h.hex()[:18]}... in block {rc.blockNumber}, gas {rc.gasUsed}, status {rc.status}")
    return rc


def main():
    url = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:28001"
    w3 = Web3(Web3.HTTPProvider(url))
    print(f"connected to {url}: chain id {w3.eth.chain_id}, head block {w3.eth.block_number}")
    head = w3.eth.get_block("latest")
    print(f"head {head.number} mined by {head.miner}, difficulty {head.difficulty}, total difficulty {head.totalDifficulty}")

    alice = Account.from_key(key_for("alice"))
    bob = Account.from_key(key_for("bob"))
    print(f"alice {alice.address}: {w3.from_wei(w3.eth.get_balance(alice.address), 'ether')} ETH")

    print("deploying Token('Course Coin', 'CC') ...")
    abi, bytecode = load("Token")
    Token = w3.eth.contract(abi=abi, bytecode=bytecode)
    rc = send(w3, alice, Token.constructor("Course Coin", "CC").build_transaction({"from": alice.address, "gas": 0, "nonce": 0,
                                                                                    "maxFeePerGas": 0, "maxPriorityFeePerGas": 0}))
    token = w3.eth.contract(address=rc.contractAddress, abi=abi)
    print(f"  Token at {token.address}")

    print("minting 1000 CC to alice, sending 250 to bob ...")
    for fn in (token.functions.mint(alice.address, 1000 * 10**18), token.functions.transfer(bob.address, 250 * 10**18)):
        send(w3, alice, fn.build_transaction({"from": alice.address, "gas": 0, "nonce": 0, "maxFeePerGas": 0, "maxPriorityFeePerGas": 0}))
    for who, acct in (("alice", alice), ("bob", bob)):
        print(f"  {who}: {token.functions.balanceOf(acct.address).call() / 10**18} {token.functions.symbol().call()}")

    print("Transfer events of this token:")
    for ev in token.events.Transfer.get_logs(from_block=0):
        print(f"  block {ev.blockNumber}: {ev.args['from'][:10]} -> {ev.args['to'][:10]}  {ev.args['value'] / 10**18}")

    print("a transfer that cannot succeed (bob has 250, sends 1000):")
    try:
        send(w3, bob, token.functions.transfer(alice.address, 1000 * 10**18).build_transaction(
            {"from": bob.address, "gas": 0, "nonce": 0, "maxFeePerGas": 0, "maxPriorityFeePerGas": 0}))
    except Exception as e:
        print(f"  rejected before sending: {e}")

    # In a PoW chain a block can be replaced by a heavier fork: wait for confirmations.
    target = rc.blockNumber + 6
    print(f"waiting until block {target} (6 confirmations of the deployment) ...")
    while w3.eth.block_number < target:
        time.sleep(1)
    still = w3.eth.get_transaction_receipt(rc.transactionHash)
    print(f"  deployment is in block {still.blockNumber} {still.blockHash.hex()[:18]}... (was {rc.blockHash.hex()[:18]}...)")
    print("done")


if __name__ == "__main__":
    main()
