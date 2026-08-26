/**
 * W2 strategy-keeper — Mimic function (checker relay).
 *
 * The W2 decision logic was always pinned to bounded on-chain helpers, so it
 * stays there: `StrategyKeeperExecutor.checker()` returns canExec plus the
 * exact perform calldata. This function reads it through an oracle-backed
 * EvmCall query and forwards execPayload verbatim as an EvmCall intent — no
 * off-chain re-derivation, no payload interpretation. A modified payload is
 * structurally impossible here: the bytes come from the contract view, not
 * from this code.
 *
 * If the view errors, emit nothing: the next tick retries. A relay that
 * guesses is worse than a relay that waits.
 */

import { EvmCallBuilder, log, TokenAmount } from '@mimicprotocol/lib-ts'
import { DenominationToken } from '@mimicprotocol/lib-ts'

import { StrategyKeeperExecutor } from './types/StrategyKeeperExecutor'
import { inputs } from './types'

export default function main(): void {
  const executor = new StrategyKeeperExecutor(inputs.executor, inputs.chainId)

  const status = executor.checker()
  if (status.isError) {
    log.warning('W2 checker() unavailable: ' + status.error + ' — emitting nothing this tick')
    return
  }

  const result = status.unwrap()
  if (!result.canExec) {
    log.info('W2 strategy-keeper: no upkeep — ' + result.execPayload.toString())
    return
  }

  log.info('W2 strategy-keeper: relaying checker() execPayload verbatim')

  const fee = TokenAmount.fromStringDecimal(DenominationToken.USD(), inputs.maxFee)

  EvmCallBuilder.forChain(inputs.chainId)
    .addCall(inputs.executor, result.execPayload)
    .addUser(inputs.smartAccount)
    .build()
    .send(fee)
}
