/**
 * W1 queue-keeper — Gelato TypeScript Web3 Function.
 *
 * Each tick: read queue state from the chain, decide the next action, and
 * either return the `perform` calldata for Gelato to submit, or return
 * canExec=false with the reason.
 *
 * Ported from the CRE-era Go workflow (`queue-keeper/main.go`). What changed:
 * no DON report, no envelope, no 15-read budget (Gelato Web3 Functions have
 * no such cap — but the scan is still bounded by config.maxBatchScan so a
 * long stall cannot produce an unbounded tick). What did not change: the
 * decision engine, the "no amounts in payloads" rule, and the on-chain
 * cross-check with divergence classification.
 */

import { Web3Function, Web3FunctionContext } from "@gelatonetwork/web3-functions-sdk";
import { ethers } from "ethers";

import { Action, State, decide, requestCost } from "./src/decide";
import { classify, unexplained } from "./src/divergence";
import { encode } from "./src/params";

// Minimal ABIs — only what this function reads.
const REGISTRY_ABI = [
  "function getContractByKey(bytes32 key) view returns (address)",
];
const EXECUTOR_ABI = [
  "function paused() view returns (bool)",
  "function nextBatchIdToProcess() view returns (uint256)",
  "function minBatchAge() view returns (uint256)",
  "function maxUsersPerUpkeep() view returns (uint256)",
  "function queueUpkeepStatus() view returns (uint8 action, uint256 batchId, uint256 count)",
  "function perform(uint8 action, bytes params) external",
];
const EXIT_QUEUE_ABI = [
  "function currentBatchId() view returns (uint256)",
  "function MAX_BATCH_PROCESSING_TIME() view returns (uint256)",
  "function paused() view returns (bool)",
  "function batchInfo(uint256) view returns (bool canBeProcessed, uint256 finalEvePrice, uint256 totalTokensToBurn, uint256 createdAt, uint256 pricedAt)",
  "function unprocessedUsersCount(uint256) view returns (uint256)",
  "function unprocessedUsers(uint256,uint256,uint256) view returns (address[])",
  "function requestInfo(uint256,address) view returns (bool processed, bool closedDueToSlippage, uint256 evePriceAtRequestTime, uint256 tokensToBurn, uint256 priceTolerance)",
];
const PAUSABLE_ABI = ["function paused() view returns (bool)"];

const PERFORM_IFACE = new ethers.utils.Interface(["function perform(uint8 action, bytes params)"]);

interface UserConfig {
  registryAddress: string;
  queueExecutorAddress: string;
  /** Caps the off-chain scan width. 0 uses the default (250). */
  maxBatchScan?: number;
}

const DEFAULT_MAX_BATCH_SCAN = 250;

// Auth keys in the EverStrat Registry are keccak256 of the key name (Auth.sol).
const CONTROLLER_KEY = ethers.utils.keccak256(ethers.utils.toUtf8Bytes("CONTROLLER"));
const EXIT_QUEUE_KEY = ethers.utils.keccak256(ethers.utils.toUtf8Bytes("EXIT_QUEUE"));
const AMM_KEY = ethers.utils.keccak256(ethers.utils.toUtf8Bytes("AMM"));

Web3Function.onRun(async (context: Web3FunctionContext) => {
  const { userArgs, multiChainProvider } = context;
  const cfg = userArgs as unknown as UserConfig;

  if (!cfg.registryAddress || !cfg.queueExecutorAddress) {
    return { canExec: false, message: "config: registryAddress and queueExecutorAddress are required" };
  }

  // multiChainProvider.default() is an already-connected ethers v5 provider
  // for the task's chain.
  const provider: ethers.providers.Provider = await multiChainProvider.default();
  const latest = await provider.getBlock("latest");
  if (!latest) {
    return { canExec: false, message: "cannot read latest block" };
  }

  const registry = new ethers.Contract(cfg.registryAddress, REGISTRY_ABI, provider);
  const executor = new ethers.Contract(cfg.queueExecutorAddress, EXECUTOR_ABI, provider);

  // Address book: every protocol address comes from the Registry, never from
  // config, so a redeploy that re-registers a contract cannot leave the keeper
  // pointed at a dead address.
  const [controllerAddr, exitQueueAddr, ammAddr] = await Promise.all([
    registry.getContractByKey(CONTROLLER_KEY),
    registry.getContractByKey(EXIT_QUEUE_KEY),
    registry.getContractByKey(AMM_KEY),
  ]);

  const exitQueue = new ethers.Contract(exitQueueAddr, EXIT_QUEUE_ABI, provider);
  const controller = new ethers.Contract(controllerAddr, PAUSABLE_ABI, provider);
  const amm = new ethers.Contract(ammAddr, PAUSABLE_ABI, provider);

  const maxScan = BigInt(cfg.maxBatchScan && cfg.maxBatchScan > 0 ? cfg.maxBatchScan : DEFAULT_MAX_BATCH_SCAN);

  const state = await readState(provider, {
    executor,
    exitQueue,
    controller,
    amm,
    blockTimestamp: BigInt(latest.timestamp),
    maxScan,
  });

  const decision = decide(state);

  // Cross-check against the gas-bounded on-chain view. A failure here loses
  // the cross-check, not the decision — the keeper must keep working.
  let divergenceClass = "unavailable";
  try {
    const onChain = await executor.queueUpkeepStatus();
    const d = classify(
      decision,
      {
        action: onChain.action,
        batchId: ethers.BigNumber.from(onChain.batchId).toBigInt(),
        count: ethers.BigNumber.from(onChain.count).toBigInt(),
      },
      state,
    );
    divergenceClass = d.class;
    if (unexplained(d)) {
      console.error("W1 divergence from on-chain view is unexplained:", d.explanation);
    } else {
      console.log("W1 cross-check:", d.class, "—", d.explanation);
    }
  } catch (e) {
    console.warn("queueUpkeepStatus cross-check unavailable:", (e as Error).message);
  }

  const header =
    `W1 queue-keeper: action=${decision.action} batch=${decision.batchId} ` +
    `end=${decision.endIndex} divergence=${divergenceClass}`;

  if (decision.action === Action.None) {
    return { canExec: false, message: `${header} — ${decision.reason}` };
  }

  // execPayload is the exact calldata for perform — byte-identical to what the
  // on-chain checker() would produce for the same action.
  const params = encode(decision.action, {
    action: decision.action,
    batchId: decision.batchId,
    startIndex: 0n,
    endIndex: decision.endIndex,
  });
  const execPayload = PERFORM_IFACE.encodeFunctionData("perform", [decision.action, params]);

  return {
    canExec: true,
    callData: [
      {
        to: cfg.queueExecutorAddress,
        data: execPayload,
      },
    ],
    message: `${header} — ${decision.reason}`,
  };
});

/** Reads the full decision snapshot at one block. */
async function readState(
  provider: ethers.providers.Provider,
  deps: {
    executor: ethers.Contract;
    exitQueue: ethers.Contract;
    controller: ethers.Contract;
    amm: ethers.Contract;
    blockTimestamp: bigint;
    maxScan: bigint;
  },
): Promise<State> {
  const { executor, exitQueue, controller, amm } = deps;

  // ethers v5 returns BigNumber for every uint; the decision engine works in
  // native bigint. One coercion point — anything entering State passes through
  // w(), so a BigNumber leaking into decide() cannot happen silently.
  const w = (v: ethers.BigNumber | bigint | number): bigint => ethers.BigNumber.from(v).toBigInt();

  const [executorPaused, controllerPaused, exitQueuePaused, ammPaused] = await Promise.all([
    executor.paused(),
    controller.paused(),
    exitQueue.paused(),
    amm.paused(),
  ]);
  const paused = executorPaused || controllerPaused || exitQueuePaused || ammPaused;

  const [currentBatchIdR, maxProcessingR, cursorR, minBatchAgeR, maxUsersPerUpkeepR] = await Promise.all([
    exitQueue.currentBatchId(),
    exitQueue.MAX_BATCH_PROCESSING_TIME(),
    executor.nextBatchIdToProcess(),
    executor.minBatchAge(),
    executor.maxUsersPerUpkeep(),
  ]);
  const currentBatchId = w(currentBatchIdR);
  const maxProcessing = w(maxProcessingR);
  const cursor = w(cursorR);
  const minBatchAge = w(minBatchAgeR);
  const maxUsersPerUpkeep = w(maxUsersPerUpkeepR);

  const state: State = {
    now: deps.blockTimestamp,
    paused,
    currentBatchId,
    nextBatchIdToProcess: cursor,
    controllerBalance: 0n,
    maxBatchProcessingTime: maxProcessing,
    minBatchAge,
    maxUsersPerUpkeep,
    batches: {},
    scanTruncatedAt: 0n,
  };

  // A paused protocol means every action reverts, so skip the scan entirely.
  if (paused) return state;

  // The Controller's own balance — the only budget affordability depends on.
  state.controllerBalance = (await provider.getBalance(await controller.getAddress())).toBigInt();

  // Scan range: the executor's cursor through the current batch (inclusive of
  // the current one, because PriceBatch needs its age and unprocessed count).
  let first = cursor;
  const last = currentBatchId;
  if (first > last) first = last;
  const scanEnd = first + deps.maxScan;

  // Phase 1: batchInfo + unprocessedUsersCount for every batch in range.
  const ids: bigint[] = [];
  for (let id = first; id <= last && id < scanEnd; id++) {
    ids.push(id);
    const [info, unprocessed] = await Promise.all([
      exitQueue.batchInfo(id),
      exitQueue.unprocessedUsersCount(id),
    ]);
    state.batches[id.toString()] = {
      id,
      canBeProcessed: info.canBeProcessed,
      finalEvePrice: w(info.finalEvePrice),
      totalTokensToBurn: w(info.totalTokensToBurn),
      createdAt: w(info.createdAt),
      pricedAt: w(info.pricedAt),
      unprocessedCount: w(unprocessed),
      requests: [],
    };
  }

  // Record where the scan actually stopped, so "found nothing" is
  // distinguishable from "did not look". scanTruncatedAt is the last batch id
  // actually fetched; zero means the scan reached the current batch.
  if (ids.length > 0) {
    const reached = ids[ids.length - 1];
    if (reached < currentBatchId) state.scanTruncatedAt = reached;
  }

  // Phase 2/3: user lists + requestInfo, in the same order decide walks.
  // Empty and expired batches are skippable (no user read). Unpriced batches
  // need PriceBatch, not users. A priced in-window head whose prefix is 0
  // (first request overruns the Controller) is not skippable — the view
  // continues — so we load the next candidate rather than stopping early.
  for (const id of ids) {
    const b = state.batches[id.toString()];
    if (!b.canBeProcessed || b.unprocessedCount === 0n) continue;
    if (b.pricedAt > 0n && state.now > b.pricedAt + maxProcessing) continue;

    let limit = b.unprocessedCount;
    if (limit > maxUsersPerUpkeep) limit = maxUsersPerUpkeep;

    const users: string[] = (await exitQueue.unprocessedUsers(id, 0n, limit)) as string[];
    if (users.length === 0) continue;

    const requests = await Promise.all(
      users.map(async (u) => {
        const r = await exitQueue.requestInfo(id, u);
        return {
          user: u,
          processed: r.processed as boolean,
          closedDueToSlippage: r.closedDueToSlippage as boolean,
          evePriceAtRequestTime: w(r.evePriceAtRequestTime),
          tokensToBurn: w(r.tokensToBurn),
          priceTolerance: w(r.priceTolerance),
        };
      }),
    );
    b.requests = requests;

    // decide will pick this batch if it has an affordable prefix; later ids
    // cannot be chosen this tick — stop scanning once one is found.
    let cumulative = 0n;
    let affordable = 0n;
    for (const req of requests) {
      const cost = requestCost(b.finalEvePrice, req);
      if (cumulative + cost > state.controllerBalance) break;
      cumulative += cost;
      affordable++;
    }
    if (affordable > 0n) break;
  }

  return state;
}
