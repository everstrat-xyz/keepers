/**
 * W2 strategy-keeper — Mimic function tests.
 *
 * The function is a relay: whatever `checker()` returns as execPayload must
 * arrive in the intent byte-for-byte. These tests pin that with adversarial
 * payloads (arbitrary selector, dynamic bytes padding) and the no-work and
 * error paths.
 *
 * What W2 chooses is when to relay and the intent's max fee. Expected fees
 * are computed here from the policy as written in docs/MIMIC_CUTOVER.md
 * ("Gas gate and fee caps") — gwei ceiling × feeCapGas, or bps of the
 * amount, at the oracle's ETH price — never read back from the function.
 */
import { expect } from 'chai'
import { ethers, Interface } from 'ethers'

import AggregatorAbi from '../abis/AggregatorV3Interface.json'
import ExitQueueAbi from '../abis/IExitQueue.json'
import QueueKeeperAbi from '../abis/IQueueKeeperExecutor.json'
import RegistryAbi from '../abis/IRegistry.json'
import ExecutorAbi from '../abis/StrategyKeeperExecutor.json'

import {
  type Context,
  type EvmCallOperation,
  mockTuple,
  NATIVE_TOKEN,
  type PriceMock,
  type RawMock,
  type RunResult,
  runWithRawMocks,
} from './helpers'

const AggregatorIface = new Interface(AggregatorAbi)
const ExecutorIface = new Interface(ExecutorAbi)
const ExitQueueIface = new Interface(ExitQueueAbi)
const QueueKeeperIface = new Interface(QueueKeeperAbi)
const RegistryIface = new Interface(RegistryAbi)

const functionDir = './build'
const chainId = 10

// Deterministic fixtures. Addresses double as read-target discriminators.
const EXECUTOR = '0x0000000000000000000000000000000000000100'
const REGISTRY = '0x0000000000000000000000000000000000000200'
const SMART_ACCOUNT = '0x0000000000000000000000000000000000000400'
const EXIT_QUEUE = '0x0000000000000000000000000000000000000600'
const QUEUE_KEEPER = '0x0000000000000000000000000000000000000700'
const GAS_FEED = '0x0000000000000000000000000000000000000800'
const ZERO_ADDRESS = '0x0000000000000000000000000000000000000000'

// checker() selector — StrategyKeeperExecutorUtils.encodeChecker()
const SEL_CHECKER = '0xcf5303cf'

// A fixed clock: the runner hands the function milliseconds, batch expiry and
// the feed's updatedAt are block-time seconds.
const NOW_MS = 1_790_000_000_000
const NOW = NOW_MS / 1000
const HOUR = 3600
const WINDOW = 3 * 24 * HOUR // ExitQueue.MAX_BATCH_PROCESSING_TIME

const ETH_USD = '2700'
const FEE_CAP_GAS = 10_000_000n

// The policy (docs/MIMIC_CUTOVER.md, "Gas gate and fee caps"), restated independently.
function gweiCapUsd(gwei: string, ethUsd = ETH_USD): bigint {
  return (FEE_CAP_GAS * ethers.parseUnits(gwei, 9) * ethers.parseUnits(ethUsd, 18)) / 10n ** 18n
}

function amountCapUsd(amountWei: bigint, bps: bigint, ethUsd = ETH_USD): bigint {
  return (((amountWei * bps) / 10_000n) * ethers.parseUnits(ethUsd, 18)) / 10n ** 18n
}

function contextAt(now: number): Context {
  return {
    user: '0x756f45e3fa69347a9a973a725e3c98bc4db0b5a0',
    settlers: [{ address: '0x0000000000000000000000000000000000000500', chainId }],
    timestamp: now,
  }
}

const inputs = {
  chainId,
  executor: EXECUTOR,
  smartAccount: SMART_ACCOUNT,
  gasPriceFeed: GAS_FEED,
  feeCapGas: Number(FEE_CAP_GAS),
  maxFee: '50',
  rebalanceMaxGwei: '0.4',
  syncMaxGwei: '0.25',
  withdrawMaxGwei: '0.3',
  withdrawUrgentMaxGwei: '3',
  withdrawRampStartHours: 24,
  withdrawRampEndHours: 12,
  amountFeeBps: 115,
}

const ethPrice = (usd = ETH_USD): PriceMock[] => [{ chainId, address: NATIVE_TOKEN, usd }]

const checkerMock = (canExec: boolean, execPayload: string): RawMock =>
  mockTuple(EXECUTOR, SEL_CHECKER, '(bool,bytes)', [[canExec, execPayload]])

const perform = (action: number): string => ExecutorIface.encodeFunctionData('perform', [action])

function call(iface: Interface, to: string, fn: string, args: unknown[], result: unknown[]): RawMock {
  return { to, data: iface.encodeFunctionData(fn, args), value: iface.encodeFunctionResult(fn, result) }
}

/** Chainlink Fast Gas: 0 decimals, answer in wei per gas. */
function gasFeed(gwei: string, ageSeconds = 60, decimals = 0, answer?: bigint): RawMock[] {
  const updatedAt = NOW - ageSeconds
  return [
    call(AggregatorIface, GAS_FEED, 'decimals', [], [decimals]),
    call(
      AggregatorIface,
      GAS_FEED,
      'latestRoundData',
      [],
      [7, answer ?? ethers.parseUnits(gwei, 9), updatedAt, updatedAt, 7]
    ),
  ]
}

// Registry keys are resolved with ethers.id — independent of the constants the
// function pins, so a wrong key there shows up as an unmocked read.
function registryMocks(): RawMock[] {
  return [
    call(ExecutorIface, EXECUTOR, 'registry', [], [REGISTRY]),
    call(RegistryIface, REGISTRY, 'getContractByKey', [ethers.id('EXIT_QUEUE')], [EXIT_QUEUE]),
    call(RegistryIface, REGISTRY, 'getContractByKey', [ethers.id('QUEUE_KEEPER_EXECUTOR')], [QUEUE_KEEPER]),
  ]
}

interface Batch {
  id: number
  canBeProcessed: boolean
  pricedAt: number
  unprocessed: number
}

function queueMocks(cursor: number, current: number, batches: Batch[]): RawMock[] {
  const mocks = [
    ...registryMocks(),
    call(ExecutorIface, EXECUTOR, 'MAX_BATCH_SCAN', [], [25]),
    call(QueueKeeperIface, QUEUE_KEEPER, 'nextLiveBatchIdToProcess', [], [cursor]),
    call(ExitQueueIface, EXIT_QUEUE, 'currentBatchId', [], [current]),
    call(ExitQueueIface, EXIT_QUEUE, 'MAX_BATCH_PROCESSING_TIME', [], [WINDOW]),
  ]
  for (const b of batches) {
    mocks.push(call(ExitQueueIface, EXIT_QUEUE, 'batchInfo', [b.id], [b.canBeProcessed, 10n ** 18n, 0, 0, b.pricedAt]))
    mocks.push(call(ExitQueueIface, EXIT_QUEUE, 'unprocessedUsersCount', [b.id], [b.unprocessed]))
  }
  return mocks
}

/** A batch priced so that it expires `hoursLeft` hours after NOW. */
const expiringIn = (id: number, hoursLeft: number, unprocessed = 1): Batch => ({
  id,
  canBeProcessed: true,
  pricedAt: NOW + hoursLeft * HOUR - WINDOW,
  unprocessed,
})

const noBatches = (): RawMock[] => queueMocks(2, 2, [])

function onlyIntent(result: RunResult): { op: EvmCallOperation; fee: bigint; token: string } {
  expect(result.success).to.be.true
  expect(result.intents).to.have.lengthOf(1)
  const intent = result.intents[0]
  expect(intent.maxFees).to.have.lengthOf(1)
  return {
    op: intent.operations[0] as EvmCallOperation,
    fee: BigInt(intent.maxFees[0].amount),
    token: intent.maxFees[0].token.toLowerCase(),
  }
}

function nothing(result: RunResult, logClass: string): string {
  expect(result.success).to.be.true
  expect(result.intents).to.have.lengthOf(0)
  const logs = JSON.stringify(result.logs)
  expect(logs).to.include(logClass)
  return logs
}

const USD_TOKEN = '0x0000000000000000000000000000000000000348'

const run = (
  payload: string,
  extra: RawMock[],
  overrides: Record<string, unknown> = {},
  prices = ethPrice()
): Promise<RunResult> =>
  runWithRawMocks(
    functionDir,
    contextAt(NOW_MS),
    { ...inputs, ...overrides },
    [checkerMock(true, payload), ...extra],
    prices
  )

describe('Strategy keeper (W2)', () => {
  it('relays the checker() execPayload verbatim', async () => {
    // perform(WithdrawShortfall=2) as the contract would build it
    const payload = perform(2)
    const { op } = onlyIntent(await run(payload, [...queueMocks(1, 2, [expiringIn(1, 60)]), ...gasFeed('0.1')]))

    expect(op.opType).to.be.equal(2) // EvmCall
    expect(op.user).to.be.equal(SMART_ACCOUNT)
    expect(op.calls).to.have.lengthOf(1)
    expect(op.calls[0].target.toLowerCase()).to.be.equal(EXECUTOR)
    // byte-for-byte relay, not a re-encoding
    expect(op.calls[0].data.toLowerCase()).to.be.equal(payload.toLowerCase())
  })

  it('relays arbitrary bytes without touching them, under the flat maxFee', async () => {
    // not a valid perform() call — the relay must not inspect or reformat it,
    // and there is no action to gate or price it by
    const payload = '0xdeadbeef' + 'ff'.repeat(67)
    const { op, fee, token } = onlyIntent(await run(payload, []))

    expect(op.calls[0].data.toLowerCase()).to.be.equal(payload.toLowerCase())
    expect(token).to.equal(USD_TOKEN)
    expect(fee).to.equal(ethers.parseUnits('50', 18))
  })

  it('emits nothing when canExec is false', async () => {
    const result = await runWithRawMocks(functionDir, contextAt(NOW_MS), inputs, [checkerMock(false, '0x')])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })

  it('emits nothing when checker() reverts', async () => {
    // no mock for the checker() read at all → oracle has no response
    const result = await runWithRawMocks(functionDir, contextAt(NOW_MS), inputs, [])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })
})

describe('W2 gas gate', () => {
  describe('Sync: flat gwei ceiling', () => {
    it('relays while the feed is under the ceiling, capped at ceiling × feeCapGas', async () => {
      const result = await run(perform(5), gasFeed('0.1'))
      const { fee, token } = onlyIntent(result)
      expect(token).to.equal(USD_TOKEN)
      expect(fee).to.equal(gweiCapUsd('0.25'))
      expect(JSON.stringify(result.logs)).to.include('gas feed 0.1 gwei ≤ ceiling 0.25 gwei')
    })

    it('relays with the feed exactly at the ceiling', async () => {
      expect(onlyIntent(await run(perform(5), gasFeed('0.25'))).fee).to.equal(gweiCapUsd('0.25'))
    })

    it('defers while the feed is above the ceiling', async () => {
      const logs = nothing(await run(perform(5), gasFeed('0.26')), 'deferred-gas')
      expect(logs).to.include('gas feed 0.26 gwei > ceiling 0.25 gwei')
    })

    it('tracks the oracle ETH price in the cap', async () => {
      const result = await run(perform(5), gasFeed('0.1'), {}, ethPrice('3100.5'))
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd('0.25', '3100.5'))
    })

    it('emits nothing when the ETH price is unavailable', async () => {
      nothing(await run(perform(5), gasFeed('0.1'), {}, []), 'read-error')
    })
  })

  describe('Rebalance: gwei ceiling, raised for a masked withdrawal', () => {
    it('relays under the rebalance ceiling when no batch is expiring', async () => {
      expect(onlyIntent(await run(perform(1), [...noBatches(), ...gasFeed('0.3')])).fee).to.equal(gweiCapUsd('0.4'))
    })

    it('defers above the rebalance ceiling when no batch is expiring', async () => {
      nothing(await run(perform(1), [...noBatches(), ...gasFeed('0.5')]), 'deferred-gas')
    })

    it('goes through a gas spike when the withdrawal it masks nears expiry', async () => {
      // checker() returns Rebalance first, so the shortfall behind it can only
      // settle if the Rebalance itself goes through.
      const result = await run(perform(1), [...queueMocks(1, 2, [expiringIn(1, 6)]), ...gasFeed('2')])
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd('3'))
      expect(JSON.stringify(result.logs)).to.include('raised for the withdrawal it masks')
    })
  })

  describe('WithdrawShortfall: gwei ceiling ramping to the batch deadline', () => {
    it('uses the floor ceiling while expiry is more than rampStart hours out', async () => {
      const result = await run(perform(2), [...queueMocks(1, 2, [expiringIn(1, 48)]), ...gasFeed('0.1')])
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd('0.3'))
      expect(JSON.stringify(result.logs)).to.include('batch expires in 48.0h')
    })

    it('interpolates linearly between rampStart and rampEnd', async () => {
      // 18h left: 6 of the 12 ramp hours gone → 0.3 + (3 − 0.3) × 6/12 = 1.65 gwei
      const result = await run(perform(2), [...queueMocks(1, 2, [expiringIn(1, 18)]), ...gasFeed('0.1')])
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd('1.65'))
    })

    it('uses the urgent ceiling from rampEnd hours out', async () => {
      const result = await run(perform(2), [...queueMocks(1, 2, [expiringIn(1, 6)]), ...gasFeed('0.1')])
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd('3'))
    })

    it('defers a spike far from expiry, relays the same spike near it', async () => {
      nothing(await run(perform(2), [...queueMocks(1, 2, [expiringIn(1, 48)]), ...gasFeed('2')]), 'deferred-gas')
      const near = await run(perform(2), [...queueMocks(1, 2, [expiringIn(1, 6)]), ...gasFeed('2')])
      expect(onlyIntent(near).fee).to.equal(gweiCapUsd('3'))
    })

    it('prices by the earliest-expiring batch that still owes users', async () => {
      // batch 1: expired (costs 0 on-chain); batch 2: fully processed;
      // batch 3: 30h left; batch 4: 6h left; batch 5: unpriced current.
      const batches: Batch[] = [
        expiringIn(1, -1),
        expiringIn(2, 3, 0),
        expiringIn(3, 30),
        expiringIn(4, 6),
        { id: 5, canBeProcessed: false, pricedAt: 0, unprocessed: 2 },
      ]
      const result = await run(perform(2), [...queueMocks(1, 6, batches), ...gasFeed('0.1')])
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd('3'))
      expect(JSON.stringify(result.logs)).to.include('batch expires in 6.0h')
    })

    it('does not scan past the executor MAX_BATCH_SCAN', async () => {
      // cursor 1, current 40: the contract sees ids 1..25 only, so the
      // urgent batch at id 26 must not raise the ceiling. Ids 2..25 are idle.
      const batches: Batch[] = [expiringIn(1, 48)]
      for (let id = 2; id <= 25; id++) batches.push({ id, canBeProcessed: true, pricedAt: NOW - HOUR, unprocessed: 0 })
      batches.push(expiringIn(26, 1))
      const result = await run(perform(2), [...queueMocks(1, 40, batches), ...gasFeed('0.1')])
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd('0.3'))
    })

    it('falls back to the floor when no expiring batch is visible (view skew)', async () => {
      const result = await run(perform(2), [...noBatches(), ...gasFeed('0.1')])
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd('0.3'))
      expect(JSON.stringify(result.logs)).to.include('no expiring batch seen')
    })

    it('emits nothing when the deadline scan cannot be read', async () => {
      // cursor 1 < current 2, but batchInfo(1) is not mocked
      nothing(await run(perform(2), [...queueMocks(1, 2, []), ...gasFeed('0.1')]), 'read-error')
    })
  })

  describe('an unusable feed degrades to the cap alone', () => {
    // A withdrawal deadline does not wait for Chainlink: without a reading the
    // tick relays, bounded only by ceiling × feeCapGas at fill time.
    const cases: [string, Record<string, unknown>, RawMock[], string][] = [
      ['no feed configured', { gasPriceFeed: ZERO_ADDRESS }, [], 'no gasPriceFeed configured'],
      ['a stale feed, however high it reads', {}, gasFeed('9', 4 * HOUR), 'stale, updated 4.0h ago'],
      ['an unreadable feed', {}, [], 'decimals()'],
      ['a non-positive answer', {}, gasFeed('0', 60, 0, 0n), 'answer 0'],
    ]
    for (const [name, overrides, feed, reason] of cases) {
      it(`relays at the cap on ${name}`, async () => {
        const result = await run(perform(5), feed, overrides)
        expect(onlyIntent(result).fee).to.equal(gweiCapUsd('0.25'))
        const logs = JSON.stringify(result.logs)
        expect(logs).to.include('gas feed unavailable')
        expect(logs).to.include(reason)
      })
    }

    it('refuses a feed that answers in the wrong unit (a price feed by mistake)', async () => {
      const logs = nothing(await run(perform(5), gasFeed('0.1', 60, 8)), 'config-error')
      expect(logs).to.include('gasPriceFeed reports 8 decimals')
    })
  })
})

describe('W2 amount caps: DepositExcess / Harvest / ProvideExitLiquidity', () => {
  const status = (action: number, amount: bigint): RawMock =>
    call(ExecutorIface, EXECUTOR, 'strategyUpkeepStatus', [], [action, amount])

  it('caps a deposit at amountFeeBps of the view amount, without reading the gas feed', async () => {
    const amount = ethers.parseEther('0.10533')
    // no gasFeed mocks: the solver alone decides whether gas is cheap enough
    const { op, fee } = onlyIntent(await run(perform(3), [status(3, amount)]))
    expect(fee).to.equal(amountCapUsd(amount, 115n))
    // the amount prices the fee only — the payload is still the checker's
    expect(op.calls[0].data.toLowerCase()).to.equal(perform(3))
  })

  for (const action of [4, 6]) {
    it(`prices action ${action} the same way`, async () => {
      const amount = ethers.parseEther('0.5')
      expect(onlyIntent(await run(perform(action), [status(action, amount)])).fee).to.equal(amountCapUsd(amount, 115n))
    })
  }

  it('clamps to maxFee', async () => {
    // 115 bps of 5 ETH at $2700 is $155.25
    const result = await run(perform(3), [status(3, ethers.parseEther('5'))])
    expect(onlyIntent(result).fee).to.equal(ethers.parseUnits('50', 18))
    expect(JSON.stringify(result.logs)).to.include('clamped to maxFee')
  })

  it('emits nothing when strategyUpkeepStatus() disagrees with checker()', async () => {
    const logs = nothing(await run(perform(3), [status(2, ethers.parseEther('1'))]), 'read-error')
    expect(logs).to.include('would price the wrong work')
  })
})

describe('W2 config-error', () => {
  const cases: [string, Record<string, unknown>, string][] = [
    ['a malformed gwei value', { rebalanceMaxGwei: '0,4' }, 'rebalanceMaxGwei'],
    ['more than 9 gwei decimals', { syncMaxGwei: '0.0000000001' }, 'syncMaxGwei'],
    ['a zero ceiling', { withdrawMaxGwei: '0' }, 'withdrawMaxGwei is 0'],
    ['an urgent ceiling below the floor', { withdrawUrgentMaxGwei: '0.2' }, 'withdrawUrgentMaxGwei'],
    ['a ramp that ends before it starts', { withdrawRampStartHours: 12 }, 'withdrawRampStartHours'],
    ['a zero amount share', { amountFeeBps: 0 }, 'amountFeeBps'],
    ['a zero fee-cap gas', { feeCapGas: 0 }, 'feeCapGas is 0'],
    ['a malformed maxFee', { maxFee: '$50' }, 'maxFee'],
  ]
  for (const [name, overrides, field] of cases) {
    it(`emits nothing on ${name}, naming the field`, async () => {
      const logs = nothing(await run(perform(5), gasFeed('0.1'), overrides), 'config-error')
      expect(logs).to.include(field)
    })
  }
})
