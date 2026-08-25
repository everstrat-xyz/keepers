# Local fork harness

Runs the freeze-watch workflow (W4) against a **real deployment** of the
EverStrat protocol on an anvil fork of Sepolia. Unit tests cover the decision
logic, but only this catches the runtime constraints.

It earned its keep twice during the CRE era, and both lessons survive the
Gelato migration:

- **CRE's `ChainRead.CallLimit` is 15 reads per execution.** The first read
  layer needed ~16 calls before touching a single batch and aborted with
  `Public:User:LimitExceeded`. This is why W4's reads batch through Multicall3
  — and why W1, now free of any read cap on Gelato, still bounds its scan by
  config so a long stall cannot produce an unbounded tick.
- **The wall clock is the wrong clock.** Batch ages compared against
  wall-clock time disagreed with the contract, which records `createdAt` from
  `block.timestamp`. W1's TS port keeps the rule: `state.now` comes from the
  observed block, never `Date.now()`.

## Prerequisites

- Foundry (`anvil`, `forge`, `cast`)
- A checkout of [`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts)
  with the Gelato-era executors
- CRE CLI (W4 only, until its own migration lands)

## 1. Fork Sepolia

Fork near head. Public RPCs prune state, so an older fork block will fail with
`historical state ... is not available` the moment anvil lazily fetches an
account it has not seen — including Multicall3.

```bash
BN=$(cast block-number --rpc-url https://ethereum-sepolia-rpc.publicnode.com)
anvil --fork-url https://ethereum-sepolia-rpc.publicnode.com \
      --fork-block-number $((BN-5)) --port 8545
```

Sanity check — Multicall3 must be reachable, since every batched read goes
through it:

```bash
cast codesize 0xcA11bde05977b3631167028862bE2a173976CA11 --rpc-url http://127.0.0.1:8545  # 3808
```

## 2. Deploy the protocol

```bash
cd ../contracts
forge build   # `forge clean` first if artifacts reference deleted sources

# PRIVATE_KEY below is Foundry's well-known anvil account #0 key — public
# fixture data, not a secret. Secret-scanning greps will flag it; that is the
# expected, reviewed exception.
PRIVATE_KEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
PRICE_FEED=0x694AA1769357215DE4FAC081bf1f309aDC325306 \
WETH_ADDRESS=0xfFf9976782d46CC05630D1f6eBAb18b2324d6B14 \
DAO_ADDRESS=0x70997970C51812dc3A010C7d01b50e0d17dc79C8 \
SECURITY_ADDRESS=0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC \
DAO_TREASURY_ADDRESS=0x90F79bf6EB2c4f870365E785982E1f101E93b906 \
WHITELIST_SIGNER_ADDRESS=0x0000000000000000000000000000000000000000 \
EXIT_LIQUIDITY_TARGET_ETH=1000000000000000000 \
CONTROLLER_RESERVE_ETH=100000000000000000 \
PERFORMANCE_FEE_BPS=1000 \
forge script script/DeployAll.s.sol:DeployAll \
  --rpc-url http://127.0.0.1:8545 --broadcast --slow
```

The deployer key and nonce sequence are fixed, so the addresses are
**deterministic** — they are the ones already in `config.local.json`:

| Contract | Address |
| --- | --- |
| Registry | `0x9E53ccCEb400c5466Ec14161e5f999ADBee78aB3` |
| Controller | `0xcF263eed76C5E827E839fEE93043D5c0e79BbB68` |
| ExitQueue | `0xd1fAbab961625d790342859f77886F77243Fb372` |
| AMM | `0xd7404e37d1beA7E63caA11643d8A8A36199b8E0C` |
| QueueKeeperExecutor | `0xdf69EF1E078606Da64320cAEB05aa6CacF104AD6` |
| StrategyKeeperExecutor | `0x244e7Fb7A340d78D0a2ab4149c1db8a9eA43fc95` |
| EVE | `0x16BBa9783b0EB15F7618436b37BEA8aA73263Ea4` |
| Whitelist | `0xb2b463eCFa606378c986f8137Af5064ad36Deb4D` |

> These are **fork addresses**, not deployments. Nothing outside
> `config.local.json` and `project.yaml`'s `local-settings` may reference them.

## 3. Run freeze-watch

```bash
cre workflow simulate freeze-watch --target local-settings --non-interactive --trigger-index 0
```

`config.local.json` sets `blockTag: "latest"`, because a fork's finalized block
lags the state you just created. **Never** use `latest` outside this harness:
DON nodes would observe different blocks and consensus would fail.

## 4. What to look for

W4's keeper-health read now checks the Gelato-era allowlist surface instead of
the CRE binding views:

```bash
EXE=0xdf69EF1E078606Da64320cAEB05aa6CacF104AD6

# Executors deploy inert — zero allowed callers is the expected fresh state.
cast call $EXE 'executorCallerCount()(uint256)' --rpc-url http://127.0.0.1:8545
```

To simulate a bound executor, impersonate an ADMIN_ROLE holder and allowlist a
caller; W4 should then report the keeper bound. With `gelatoProxyAddress` set
in the config to an address *not* on the allowlist, W4 must report
bound-but-broken — that is the state its `KeeperHealth.Bound` check exists to
catch.

## Read budget

Every tick logs `readsRemaining`. W4's plan leaves several of its 15 reads
spare on a small queue. If that reaches 0, the scan truncated —
`scanTruncated=true` in the same log line — which under-reports rather than
mis-reports, but means the queue has outgrown one tick's budget.
