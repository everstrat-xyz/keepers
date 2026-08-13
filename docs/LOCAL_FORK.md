# Local fork harness

Runs a keeper workflow against a **real deployment** of the EverStrat protocol
on an anvil fork of Sepolia. Until the Sepolia cutover
([issue #6](https://github.com/everstrat-xyz/keepers/issues/6)) there is nothing
else that exercises the EVM read path end to end — unit tests cover the decision
logic, but only this catches the runtime constraints.

It has already earned its keep twice:

- **`ChainRead.CallLimit` is 15 reads per execution.** The first read layer
  needed ~16 calls before touching a single batch and aborted with
  `Public:User:LimitExceeded`. This is why reads batch through Multicall3.
- **`runtime.Now()` is the wrong clock.** Batch ages compared against wall-clock
  time disagreed with the contract, which records `createdAt` from
  `block.timestamp`. Worse in production: `CREReceiverBase` rejects
  `observedAt > block.timestamp` outright, so a DON clock a second ahead of the
  chain would fail *every* report. The workflows now take both from
  `evmread.BlockTimestamp`.

## Prerequisites

- Foundry (`anvil`, `forge`, `cast`)
- A checkout of [`everstrat-xyz/contracts`](https://github.com/everstrat-xyz/contracts)
  with the CRE receivers (branch `feat/cre-keeper-executors` until it merges)
- CRE CLI

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
KEYSTONE_FORWARDER=0xF8344CFd5c43616a4366C34E3EEE75af79a74482 \
CHAIN_SELECTOR=16015286601757825753 \
MAX_REPORT_AGE=3600 \
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
| CREQueueExecutor | `0xdf69EF1E078606Da64320cAEB05aa6CacF104AD6` |
| CREStrategyExecutor | `0x244e7Fb7A340d78D0a2ab4149c1db8a9eA43fc95` |
| EVE | `0x16BBa9783b0EB15F7618436b37BEA8aA73263Ea4` |
| Whitelist | `0xb2b463eCFa606378c986f8137Af5064ad36Deb4D` |

> These are **fork addresses**, not deployments. Nothing outside
> `config.local.json` and `project.yaml`'s `local-settings` may reference them.

## 3. Run a workflow

```bash
cre workflow simulate queue-keeper --target local-settings --non-interactive --trigger-index 0
```

`config.local.json` sets `blockTag: "latest"`, because a fork's finalized block
lags the state you just created. **Never** use `latest` outside this harness:
DON nodes would observe different blocks and consensus would fail.

## 4. Drive queue activity

An empty queue only proves the workflow reads. To exercise the decision engine,
put real redemptions in the queue.

```bash
R=http://127.0.0.1:8545
TL=0xD351078d4677a063F8608cC27E4C58499CdB0210   # admin timelock
WL=0xb2b463eCFa606378c986f8137Af5064ad36Deb4D
AMM=0xd7404e37d1beA7E63caA11643d8A8A36199b8E0C
EVE=0x16BBa9783b0EB15F7618436b37BEA8aA73263Ea4
USER=0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266
UKEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80

# Whitelist the user (ADMIN_ROLE lives on the timelock; impersonate it)
cast rpc anvil_impersonateAccount $TL --rpc-url $R
cast rpc anvil_setBalance $TL 0xde0b6b3a7640000 --rpc-url $R
cast send $WL "addToWhitelist(address[])" "[$USER]" --from $TL --unlocked --rpc-url $R

# Enter. _minTokensToMint must be non-zero — 0 reverts AMMInvalidTokensToMintAmount.
cast send $AMM "enter(uint256)" 1 --value 10ether --private-key $UKEY --rpc-url $R

# Queue a redemption
cast send $EVE "approve(address,uint256)" $AMM 100000000000000000000000 --private-key $UKEY --rpc-url $R
cast send $AMM "exit(uint256,uint256,uint256)" \
  1000000000000000000 2500000000000000000000 50000000000000000 --private-key $UKEY --rpc-url $R
```

Then walk the queue through its states, running the workflow at each step and
checking that `divergence=match` in the cross-check log:

```bash
EXE=0xdf69EF1E078606Da64320cAEB05aa6CacF104AD6
CTRL=0xcF263eed76C5E827E839fEE93043D5c0e79BbB68

# (a) fresh batch — below minBatchAge, so: None
cre workflow simulate queue-keeper --target local-settings --non-interactive --trigger-index 0

# (b) age it past minBatchAge (1 day) — expect PriceBatch
cast rpc evm_increaseTime 90000 --rpc-url $R && cast rpc evm_mine --rpc-url $R

# (c) price it (the executor holds KEEPER_ROLE) — expect ProcessRequests
cast rpc anvil_impersonateAccount $EXE --rpc-url $R
cast rpc anvil_setBalance $EXE 0xde0b6b3a7640000 --rpc-url $R
cast send $CTRL "priceBatch()" --from $EXE --unlocked --rpc-url $R

# (d) drain the Controller — nothing affordable, so: None
cast rpc anvil_setBalance $CTRL 0x0 --rpc-url $R
```

Compare against the contract's own view at every step:

```bash
cast call $EXE 'queueUpkeepStatus()(uint8,uint256,uint256)' --rpc-url $R
```

### Verified states

Each row was confirmed with W1 deriving the action independently from raw
`ExitQueue` reads and its own affordability model:

| Queue state | On-chain view | W1 | Divergence |
| --- | --- | --- | --- |
| Empty | `None` | `None` | `match` |
| Batch below `minBatchAge` | `None` | `None` | `match` |
| Batch aged past `minBatchAge` | `PriceBatch(1)` | `PriceBatch(1)` | `match` |
| Batch priced, Controller funded | `ProcessRequests(1, 1)` | `ProcessRequests(1, 1)` | `match` |
| Batch priced, Controller drained | `None` | `None` | `match` |

## W2 states

`DeployAll` is core-only — it registers **no strategies** — so Rebalance,
DepositExcess, Harvest and Sync are all unreachable on a bare fork. Exit
liquidity is reachable by funding the Controller:

```bash
CTRL=0xcF263eed76C5E827E839fEE93043D5c0e79BbB68
cast rpc anvil_setBalance $CTRL 0x4563918244F40000 --rpc-url $R   # 5 ETH
cre workflow simulate strategy-keeper --target local-settings --non-interactive --trigger-index 0
```

Pause the Controller to check the short-circuit (SECURITY_ROLE):

```bash
SEC=0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC
cast rpc anvil_impersonateAccount $SEC --rpc-url $R
cast rpc anvil_setBalance $SEC 0xde0b6b3a7640000 --rpc-url $R
cast send $CTRL "pause()" --from $SEC --unlocked --rpc-url $R
```

| Protocol state | On-chain view | W2 | Divergence |
| --- | --- | --- | --- |
| Controller empty, float at 0 | `None` | `None` | `match` |
| Controller funded, float below target | `ProvideExitLiquidity(1 ETH)` | `ProvideExitLiquidity(1 ETH)` | `match` |
| Controller paused | `None` | `None` | `match` |

In the funded case W2's independent `_pendingRedemptionNeedsETH` reproduction
agreed with the contract's `pendingRedemptionNeedsETH()` **to the wei**
(999999999999999999), which is the strongest available check on the redemption
cost model.

Registering a strategy would exercise the remaining branches; that needs
`DeployUniCLStrat` plus a timelocked `addStrategy`, and is not covered here.

## Read budget

Every tick logs `readsRemaining`. W1's plan leaves 5–8 of 15 reads spare on a
small queue. If that reaches 0, the scan truncated — `scanTruncated=true` in the
same log line — which under-proposes work rather than proposing wrong work, but
means the queue has outgrown one tick's budget.
