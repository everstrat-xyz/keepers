/**
 * W2 strategy-keeper — Mimic function (checker relay + no-op suppression).
 *
 * The W2 decision logic was always pinned to bounded on-chain helpers, so it
 * stays there: `StrategyKeeperExecutor.checker()` returns canExec plus the
 * exact perform calldata. This function reads it through an oracle-backed
 * EvmCall query and forwards execPayload verbatim as an EvmCall intent — no
 * off-chain re-derivation, no payload interpretation. A modified payload is
 * structurally impossible here: the bytes come from the contract view, not
 * from this code.
 *
 * One exception (keepers#27): a Rebalance payload whose every selected
 * (`!paused && !isHealthy()`) strategy is not calm is a guaranteed on-chain
 * revert — `UniCLStrat.rebalance()` requires `_isCalm()` even when
 * `isHealthy()` is false only because the market is turbulent. Relaying it
 * burns gas every tick and moves nothing, so the tick is suppressed instead.
 * Suppression only ever withholds the contract's own bytes; it never builds
 * a payload. See src/suppression.ts.
 *
 * The one thing W2 does choose is the intent's max fee: a gas-price ceiling
 * or a share of the amount moved, per action, never above maxFee. A solver
 * cannot fill above it, so a relay during a gas spike lapses and the next
 * tick retries. See src/feecap.ts.
 *
 * If any view errors, emit nothing: the next tick retries. A relay that
 * guesses is worse than a relay that waits. An execPayload that does not
 * decode as perform(uint8) is relayed verbatim under the flat maxFee — the
 * pre-suppression behaviour, never worse than today.
 *
 * One log class per tick: `relay` / `suppressed-noop-rebalance` /
 * `read-error` / `config-error` (idle ticks log no upkeep, as before).
 */

import { BigInt, environment, EvmCallBuilder, log } from '@mimicprotocol/lib-ts'

import { StrategyKeeperExecutor } from './types/StrategyKeeperExecutor'
import { actionName, ActionRebalance, decodePerformAction } from './actions'
import { feeCap, FeePolicy } from './feecap'
import { evaluateSuppression } from './suppression'
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
  let selectedStrategies = 0
  let relayNote = 'forwarding checker() execPayload verbatim'
  if (action == ActionRebalance) {
    const verdict = evaluateSuppression(executor, inputs.chainId)
    if (verdict.error != '') {
      log.warning('W2 read-error: ' + verdict.error + ' — emitting nothing this tick')
      return
    }
    if (verdict.suppress) {
      let detail = ''
      for (let i = 0; i < verdict.checks.length; i++) {
        detail += (i == 0 ? '' : ' | ') + verdict.checks[i].describe()
      }
      log.info('W2 suppressed-noop-rebalance: every selected strategy is not calm, batch cannot move — ' + detail)
      return
    }
    selectedStrategies = verdict.checks.length
    relayNote = 'at least one calm unhealthy strategy — relaying checker() execPayload verbatim'
  }

  // The runner's clock is milliseconds; batch expiry is block-time seconds
  // (CLAUDE.md §2). Convert once, here.
  const now = BigInt.fromU64(environment.getContext().timestamp).div(BigInt.fromI32(1000)).toI64()

  const capResult = feeCap(executor, inputs.chainId, action, selectedStrategies, policy, now)
  if (capResult.isError) {
    log.warning('W2 read-error: fee cap: ' + capResult.error + ' — emitting nothing this tick')
    return
  }
  const cap = capResult.unwrap()
  log.info('W2 relay: ' + actionName(action) + ', ' + relayNote + ' — max fee ' + cap.basis)

  EvmCallBuilder.forChain(inputs.chainId)
    .addCall(inputs.executor, result.execPayload)
    .addUser(inputs.smartAccount)
    .build()
    .send(cap.fee)
}
