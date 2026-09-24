/**
 * W1 trigger inputs, built once from `scripts/.env` (see `env.template`).
 *
 * Every key `manifest.yaml` declares has to appear here: a trigger created
 * with a missing input fails manifest validation at creation time, not at run
 * time. All three scripts build from this one object so they cannot drift
 * apart — they already had, once, when `create-trigger.ts` omitted
 * `smartAccount` and the runbook omitted `controller` / `exitQueue`.
 */
import { CronTriggerConfig, TriggerType } from '@mimicprotocol/sdk'
import { config } from 'dotenv'

config({ path: './scripts/.env' })

export interface QueueKeeperInputs {
  [key: string]: string | number
  chainId: number
  executor: string
  controller: string
  exitQueue: string
  amm: string
  helper: string
  smartAccount: string
  maxBatches: number
  maxRequestsPerBatch: number
  maxFee: string
}

/** Stand-in for the smart account before a trigger exists. See `inputs()`. */
const UNASSIGNED_SMART_ACCOUNT = '0x0000000000000000000000000000000000000000'

export function required(name: string): string {
  const value = process.env[name]
  if (!value) throw new Error(`${name} is not set — copy scripts/env.template to scripts/.env and fill it in`)
  return value
}

/**
 * @param requireSmartAccount Pass `true` only when creating the trigger.
 *
 * Dry-run and prefill do not settle, so they may use the zero address.
 * `create-trigger.ts` demands the real Mimic smart account (Protocol App,
 * this chain): that value is the trigger input *and* what ADMIN passes to
 * `allowExecutorCaller()`. A live trigger with `0x0` would `.addUser` the
 * zero address.
 */
export function inputs(requireSmartAccount = false): QueueKeeperInputs {
  return {
    chainId: Number(required('CHAIN_ID')),
    executor: required('QUEUE_EXECUTOR_ADDRESS'),
    controller: required('CONTROLLER_ADDRESS'),
    exitQueue: required('EXIT_QUEUE_ADDRESS'),
    // Needed only for its pause flag: queueUpkeepStatus() refuses to recommend
    // work while the AMM is paused, and Controller.priceBatch is whenNotPaused
    // on the Controller alone — so W1 has to check the AMM itself.
    amm: required('AMM_ADDRESS'),
    // MimicHelper used for the Controller balance read. lib-ts hardcodes one
    // helper address for every chain and it is not deployed on Base Sepolia;
    // the input keeps the function portable across Mimic's per-chain helper
    // deployments. On Base Sepolia: 0x5cf82cBED1110fc2f75B3413d53abac492931804.
    helper: required('MIMIC_HELPER_ADDRESS'),
    smartAccount: requireSmartAccount ? required('SMART_ACCOUNT_ADDRESS') : UNASSIGNED_SMART_ACCOUNT,
    // 250 is far above the 25 live-priced cap. 50 is looser than
    // maxUsersPerUpkeep (default 20). See README "Why the split".
    maxBatches: Number(process.env.MAX_BATCHES ?? 250),
    maxRequestsPerBatch: Number(process.env.MAX_REQUESTS_PER_BATCH ?? 50),
    maxFee: process.env.MAX_FEE ?? '1',
  }
}

export const cronSchedule: string = process.env.CRON_SCHEDULE ?? '*/5 * * * *'

/** Milliseconds. Mimic requires every trigger to carry an expiry. */
const ONE_YEAR_MS = 365 * 24 * 60 * 60 * 1000

/**
 * The cron config all three scripts share.
 *
 * Three things the SDK enforces that the starter template got wrong, and that
 * an `as CronTriggerConfig` cast used to hide until the API rejected it:
 *   - `type` is the numeric TriggerType.Cron literal, not the string 'cron'
 *   - `delta` is a duration STRING matching /^\d+(s|m|h|d|w)$/ — `0` is invalid
 *   - `endDate` is REQUIRED (ms since epoch)
 *
 * That last one matters operationally: a Mimic trigger has a stop date. The
 * keeper goes quiet when it passes, with no on-chain signal — whatever watches
 * executor liveness has to watch this date too. Set TRIGGER_END_DATE to pin it.
 */
export function cronConfig(): CronTriggerConfig {
  const configured = process.env.TRIGGER_END_DATE
  const endDate = configured ? Date.parse(configured) : Date.now() + ONE_YEAR_MS
  if (Number.isNaN(endDate)) throw new Error(`TRIGGER_END_DATE is not a parseable date: ${configured}`)
  return {
    type: TriggerType.Cron,
    schedule: cronSchedule,
    // How long after each scheduled tick the execution stays valid. Keep it at
    // or below the cron interval so a stale tick cannot settle late.
    delta: process.env.TRIGGER_DELTA ?? '5m',
    endDate,
  }
}

/** Human-readable expiry, for the "this trigger stops on" warning. */
export function endDateNotice(): string {
  return `trigger expires ${new Date(cronConfig().endDate).toISOString()} — renew before then or the keeper goes quiet`
}
