/**
 * W2 trigger inputs, built once from `scripts/.env` (see `env.template`).
 *
 * Every key `manifest.yaml` declares has to appear here: a trigger created
 * with a missing input fails manifest validation at creation time, not at run
 * time. All three scripts build from this one object so they cannot drift
 * apart — they already had, once, when `create-trigger.ts` omitted
 * `smartAccount`.
 */
import { CronTriggerConfig, TriggerType } from '@mimicprotocol/sdk'
import { config } from 'dotenv'

config({ path: './scripts/.env' })

export interface StrategyKeeperInputs {
  [key: string]: string | number
  chainId: number
  executor: string
  smartAccount: string
  gasPriceFeed: string
  feeCapGas: number
  maxFee: string
  rebalanceMaxGwei: string
  syncMaxGwei: string
  withdrawMaxGwei: string
  withdrawUrgentMaxGwei: string
  withdrawRampStartHours: number
  withdrawRampEndHours: number
  amountFeeBps: number
}

/**
 * Gas-gate and fee-cap defaults, derived in docs/MIMIC_CUTOVER.md ("Gas gate
 * and fee caps") from mainnet base fees, the Chainlink Fast Gas feed and W2's
 * own settlements.
 *
 * FEE_CAP_GAS is not a gas limit: a gas-gated intent's max fee is its gwei
 * ceiling × this, so it only has to stay above the largest bill any W2 action
 * can run up (~9.8M gas-equivalent for four v2 strategies rebalancing at
 * once, at the worst observed solver markup). MAX_FEE applies only to
 * undecodable payloads and as the clamp on amount-based caps.
 */
const FEE_DEFAULTS = {
  FEE_CAP_GAS: '10000000',
  MAX_FEE: '50',
  REBALANCE_MAX_GWEI: '0.4',
  SYNC_MAX_GWEI: '0.25',
  WITHDRAW_MAX_GWEI: '0.3',
  WITHDRAW_URGENT_MAX_GWEI: '3',
  WITHDRAW_RAMP_START_HOURS: '24',
  WITHDRAW_RAMP_END_HOURS: '12',
  AMOUNT_FEE_BPS: '115',
}

function fee(name: keyof typeof FEE_DEFAULTS): string {
  return process.env[name] ?? FEE_DEFAULTS[name]
}

function wholeNumber(name: keyof typeof FEE_DEFAULTS): number {
  const value = Number(fee(name))
  // The manifest types these uint32: a fraction or a negative would fail
  // trigger creation, not a tick — say which variable.
  if (!Number.isInteger(value) || value < 0) throw new Error(`${name} must be a whole number, got ${fee(name)}`)
  return value
}

/** Chainlink Fast Gas / Gwei — mainnet only. Elsewhere set GAS_PRICE_FEED, or run cap-only. */
const MAINNET_FAST_GAS_FEED = '0x169E633A2D1E6c10dD91238Ba11c4A708dfEF37C'
const NO_FEED = '0x0000000000000000000000000000000000000000'

function gasPriceFeed(chainId: number): string {
  const configured = process.env.GAS_PRICE_FEED
  if (configured) return configured
  if (chainId === 1) return MAINNET_FAST_GAS_FEED
  // Without a feed every gas-priced action relays under its cap alone: the
  // solver still cannot fill above ceiling × FEE_CAP_GAS, but nothing waits
  // out a spike below that. Say so rather than degrade silently.
  console.warn(`no gas feed for chain ${chainId}: set GAS_PRICE_FEED, or W2 runs cap-only`)
  return NO_FEED
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
export function inputs(requireSmartAccount = false): StrategyKeeperInputs {
  const chainId = Number(required('CHAIN_ID'))
  return {
    chainId,
    executor: required('STRATEGY_EXECUTOR_ADDRESS'),
    smartAccount: requireSmartAccount ? required('SMART_ACCOUNT_ADDRESS') : UNASSIGNED_SMART_ACCOUNT,
    gasPriceFeed: gasPriceFeed(chainId),
    feeCapGas: wholeNumber('FEE_CAP_GAS'),
    maxFee: fee('MAX_FEE'),
    rebalanceMaxGwei: fee('REBALANCE_MAX_GWEI'),
    syncMaxGwei: fee('SYNC_MAX_GWEI'),
    withdrawMaxGwei: fee('WITHDRAW_MAX_GWEI'),
    withdrawUrgentMaxGwei: fee('WITHDRAW_URGENT_MAX_GWEI'),
    withdrawRampStartHours: wholeNumber('WITHDRAW_RAMP_START_HOURS'),
    withdrawRampEndHours: wholeNumber('WITHDRAW_RAMP_END_HOURS'),
    amountFeeBps: wholeNumber('AMOUNT_FEE_BPS'),
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
