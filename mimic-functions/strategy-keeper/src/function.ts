/**
 * W2 strategy-keeper — Mimic function (checker relay + gas gate).
 *
 * The W2 decision logic was always pinned to bounded on-chain helpers, so it
 * stays there: `StrategyKeeperExecutor.checker()` returns canExec plus the
 * exact perform calldata. This function reads it through an oracle-backed
 * EvmCall query and forwards execPayload verbatim as an EvmCall intent — no
 * off-chain re-derivation, no payload interpretation. A modified payload is
 * structurally impossible here: the bytes come from the contract view, not
 * from this code.
 *
 * What W2 does choose is when to relay and the intent's max fee: work that
 * can wait out a gas spike waits for the gas feed to come under its ceiling,
 * and every intent's max fee is capped. See src/feecap.ts.
 *
 * keepers#27's no-op Rebalance suppression is gone (keepers#31): every
 * strategy built after everstrat-xyz/contracts#51 reports isHealthy() while
 * its pool is not calm, so the executor never selects a Rebalance that
 * `_isCalm()` would revert. Ship this only once no pre-fix strategy is
 * registered, or the gas-burning loop returns for that strategy.
 *
 * If any view errors, emit nothing: the next tick retries. A relay that
 * guesses is worse than a relay that waits. An execPayload that does not
 * decode as perform(uint8) is relayed verbatim under the flat maxFee.
 *
 * One log class per tick: `relay` / `deferred-gas` / `read-error` /
 * `config-error` (idle ticks log no upkeep, as before).
 */

import { BigInt, environment, EvmCallBuilder, log } from '@mimicprotocol/lib-ts'

import { StrategyKeeperExecutor } from './types/StrategyKeeperExecutor'
import { actionName, decodePerformAction } from './actions'
import { decideFee, FeePolicy, Outcome } from './feecap'
import { inputs } from './types'

export default function main(): void {
  const policyResult = FeePolicy.fromInputs()
  if (policyResult.isError) {
    log.error('W2 config-error: ' + policyResult.error + ' — emitting nothing until the trigger inputs are fixed')
    return
  }
  const policy = policyResult.unwrap()

  const executor = new StrategyKeeperExecutor(inputs.executor, inputs.chainId)

  const status = executor.checker()
  if (status.isError) {
    log.warning('W2 read-error: checker() unavailable: ' + status.error + ' — emitting nothing this tick')
    return
  }

  const result = status.unwrap()
  if (!result.canExec) {
    log.info('W2 strategy-keeper: no upkeep — ' + result.execPayload.toString())
    return
  }

  const action = decodePerformAction(result.execPayload)
  const name = actionName(action)

  // The runner's clock is milliseconds; batch expiry and the gas feed's
  // updatedAt are block-time seconds (CLAUDE.md §2). Convert once, here.
  const now = BigInt.fromU64(environment.getContext().timestamp).div(BigInt.fromI32(1000)).toI64()

  const decision = decideFee(executor, inputs.chainId, action, policy, now)
  if (decision.outcome == Outcome.ConfigError) {
    log.error('W2 config-error: ' + decision.note + ' — emitting nothing until the trigger inputs are fixed')
    return
  }
  if (decision.outcome == Outcome.ReadError) {
    log.warning('W2 read-error: ' + name + ': ' + decision.note + ' — emitting nothing this tick')
    return
  }
  if (decision.outcome == Outcome.DeferGas) {
    log.info('W2 deferred-gas: ' + name + ', ' + decision.note + ' — retrying next tick')
    return
  }

  log.info('W2 relay: ' + name + ', forwarding checker() execPayload verbatim — ' + decision.note)

  EvmCallBuilder.forChain(inputs.chainId)
    .addCall(inputs.executor, result.execPayload)
    .addUser(inputs.smartAccount)
    .build()
    .send(decision.fee)
}
