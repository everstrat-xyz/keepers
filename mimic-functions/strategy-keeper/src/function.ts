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
 * If any view errors, emit nothing: the next tick retries. A relay that
 * guesses is worse than a relay that waits. An execPayload that does not
 * decode as perform(uint8) is relayed verbatim — the pre-suppression
 * behaviour, never worse than today.
 *
 * One log class per tick: `relay` / `suppressed-noop-rebalance` /
 * `read-error` (idle ticks log no upkeep, as before).
 */

import { EvmCallBuilder, log, TokenAmount } from '@mimicprotocol/lib-ts'
import { DenominationToken } from '@mimicprotocol/lib-ts'

import { StrategyKeeperExecutor } from './types/StrategyKeeperExecutor'
import { ActionRebalance, decodePerformAction, evaluateSuppression } from './suppression'
import { inputs } from './types'

export default function main(): void {
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

  if (decodePerformAction(result.execPayload) == ActionRebalance) {
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
    log.info('W2 relay: Rebalance with at least one calm unhealthy strategy — relaying checker() execPayload verbatim')
  } else {
    log.info('W2 relay: forwarding checker() execPayload verbatim')
  }

  const fee = TokenAmount.fromStringDecimal(DenominationToken.USD(), inputs.maxFee)

  EvmCallBuilder.forChain(inputs.chainId)
    .addCall(inputs.executor, result.execPayload)
    .addUser(inputs.smartAccount)
    .build()
    .send(fee)
}
