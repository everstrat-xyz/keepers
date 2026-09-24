import { Bytes } from '@mimicprotocol/lib-ts'

// StrategyAction ordinals, pinned to IStrategyKeeperExecutor.StrategyAction.
// Solidity enums reorder silently; if the contract enum moves, these must move
// with it or suppression targets the wrong action and every fee cap is priced
// for the wrong work.
export const ActionNone: i32 = 0
export const ActionRebalance: i32 = 1
export const ActionWithdrawShortfall: i32 = 2
export const ActionDepositExcess: i32 = 3
export const ActionHarvestPerformanceFees: i32 = 4
export const ActionSync: i32 = 5
export const ActionProvideExitLiquidity: i32 = 6

// perform(uint8) selector. checker() builds execPayload as
// `abi.encodeCall(this.perform, (action))`: 4-byte selector + one 32-byte
// word holding the enum ordinal.
const PERFORM_SELECTOR = '0x16d9fdd2'
const PERFORM_PAYLOAD_LENGTH = 36

/**
 * Extracts the action ordinal from a `perform(uint8)` payload, or -1 when the
 * bytes are not exactly selector + one word. Anything undecodable is relayed
 * verbatim by the caller — the pre-suppression behaviour, never worse than
 * today — under the flat maxFee, since there is no work to price it by.
 */
export function decodePerformAction(execPayload: Bytes): i32 {
  if (execPayload.length != PERFORM_PAYLOAD_LENGTH) return -1
  if (!execPayload.toHexString().startsWith(PERFORM_SELECTOR)) return -1
  // uint8 sits in the low byte of the word; any set bit above it is not a
  // valid ordinal encoding.
  for (let i = 4; i < PERFORM_PAYLOAD_LENGTH - 1; i++) {
    if (execPayload[i] != 0) return -1
  }
  return execPayload[PERFORM_PAYLOAD_LENGTH - 1]
}

export function actionName(action: i32): string {
  if (action == ActionNone) return 'None'
  if (action == ActionRebalance) return 'Rebalance'
  if (action == ActionWithdrawShortfall) return 'WithdrawShortfall'
  if (action == ActionDepositExcess) return 'DepositExcess'
  if (action == ActionHarvestPerformanceFees) return 'HarvestPerformanceFees'
  if (action == ActionSync) return 'Sync'
  if (action == ActionProvideExitLiquidity) return 'ProvideExitLiquidity'
  return 'Action(' + action.toString() + ')'
}
