/**
 * W2 strategy-keeper — Mimic function tests.
 *
 * The function is a relay plus one suppression: whatever `checker()` returns
 * as execPayload must arrive in the intent byte-for-byte, unless the payload
 * is a Rebalance whose every selected strategy is not calm — a guaranteed
 * on-chain revert (keepers#27), which must emit nothing instead. These tests
 * pin both properties with adversarial payloads (arbitrary selector, dynamic
 * bytes padding), the no-work and error paths, and a replay of the three
 * 2026-09-18 incident blocks from chain-verbatim archive reads
 * (tests/incident-fixtures.ts).
 *
 * The max fee is the one thing W2 chooses. Expected fees are computed here
 * from the policy as written in docs/MIMIC_CUTOVER.md ("Fee caps") — gas
 * budget × gwei ceiling × 1.45 markup, or bps of the amount, at the oracle's
 * ETH price — never read back from the function.
 */
import { expect } from 'chai'
import { AbiCoder, ethers, Interface } from 'ethers'

import ExitQueueAbi from '../abis/IExitQueue.json'
import QueueKeeperAbi from '../abis/IQueueKeeperExecutor.json'
import RegistryAbi from '../abis/IRegistry.json'
import ExecutorAbi from '../abis/StrategyKeeperExecutor.json'
import StrategyManagerAbi from '../abis/StrategyManager.json'

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
import {
  EXECUTOR as MAINNET_EXECUTOR,
  EXIT_QUEUE as MAINNET_EXIT_QUEUE,
  FEE_READS_26004820,
  INCIDENT_BLOCKS,
  incidentMocks,
  P1,
  P2,
  REBALANCE_PAYLOAD,
  SEL,
} from './incident-fixtures'

const ExecutorIface = new Interface(ExecutorAbi)
const ExitQueueIface = new Interface(ExitQueueAbi)
const QueueKeeperIface = new Interface(QueueKeeperAbi)
const RegistryIface = new Interface(RegistryAbi)
const StrategyManagerIface = new Interface(StrategyManagerAbi)
const CODER = AbiCoder.defaultAbiCoder()

const functionDir = './build'
const chainId = 10

// Deterministic fixtures. Addresses double as read-target discriminators.
const EXECUTOR = '0x0000000000000000000000000000000000000100'
const REGISTRY = '0x0000000000000000000000000000000000000200'
const STRATEGY_MANAGER = '0x0000000000000000000000000000000000000300'
const SMART_ACCOUNT = '0x0000000000000000000000000000000000000400'
const EXIT_QUEUE = '0x0000000000000000000000000000000000000600'
const QUEUE_KEEPER = '0x0000000000000000000000000000000000000700'
const STRATEGIES = [
  '0x0000000000000000000000000000000000000a01',
  '0x0000000000000000000000000000000000000a02',
  '0x0000000000000000000000000000000000000a03',
]

// checker() selector — StrategyKeeperExecutorUtils.encodeChecker()
const SEL_CHECKER = '0xcf5303cf'

// A fixed clock: the runner hands the function milliseconds, batch expiry is
// block-time seconds.
const NOW_MS = 1_790_000_000_000
const NOW = NOW_MS / 1000
const HOUR = 3600
const WINDOW = 3 * 24 * HOUR // ExitQueue.MAX_BATCH_PROCESSING_TIME

const ETH_USD = '2700'

// The policy (docs/MIMIC_CUTOVER.md, "Fee caps"), restated independently.
const REBALANCE_GAS_PER_STRATEGY = 1_690_000n
const SYNC_GAS_PER_STRATEGY = 210_000n
const WITHDRAW_GAS_PER_STRATEGY = 980_000n
const MARKUP_BPS = 14_500n

function gweiCapUsd(gas: bigint, gwei: string, ethUsd = ETH_USD): bigint {
  const wei = (gas * ethers.parseUnits(gwei, 9) * MARKUP_BPS) / 10_000n
  return (wei * ethers.parseUnits(ethUsd, 18)) / 10n ** 18n
}

function amountCapUsd(amountWei: bigint, bps: bigint, ethUsd = ETH_USD): bigint {
  return (((amountWei * bps) / 10_000n) * ethers.parseUnits(ethUsd, 18)) / 10n ** 18n
}

function contextAt(now: number, chain = chainId): Context {
  return {
    user: '0x756f45e3fa69347a9a973a725e3c98bc4db0b5a0',
    settlers: [{ address: '0x0000000000000000000000000000000000000500', chainId: chain }],
    timestamp: now,
  }
}

const feeInputs = {
  maxFee: '50',
  rebalanceMaxGwei: '0.4',
  syncMaxGwei: '0.25',
  withdrawMaxGwei: '0.3',
  withdrawUrgentMaxGwei: '3',
  withdrawRampStartHours: 24,
  withdrawRampEndHours: 12,
  amountFeeBps: 115,
}

const inputs = {
  chainId,
  executor: EXECUTOR,
  smartAccount: SMART_ACCOUNT,
  ...feeInputs,
}

const ethPrice = (chain = chainId, usd = ETH_USD): PriceMock[] => [{ chainId: chain, address: NATIVE_TOKEN, usd }]

const checkerMock = (canExec: boolean, execPayload: string): RawMock =>
  mockTuple(EXECUTOR, SEL_CHECKER, '(bool,bytes)', [[canExec, execPayload]])

const perform = (action: number): string => ExecutorIface.encodeFunctionData('perform', [action])

function call(iface: Interface, to: string, fn: string, args: unknown[], result: unknown[]): RawMock {
  return { to, data: iface.encodeFunctionData(fn, args), value: iface.encodeFunctionResult(fn, result) }
}

// Registry keys are resolved with ethers.id — independent of the constants the
// function pins, so a wrong key there shows up as an unmocked read.
function registryMocks(): RawMock[] {
  return [
    call(ExecutorIface, EXECUTOR, 'registry', [], [REGISTRY]),
    call(RegistryIface, REGISTRY, 'getContractByKey', [ethers.id('STRATEGY_MANAGER')], [STRATEGY_MANAGER]),
    call(RegistryIface, REGISTRY, 'getContractByKey', [ethers.id('EXIT_QUEUE')], [EXIT_QUEUE]),
    call(RegistryIface, REGISTRY, 'getContractByKey', [ethers.id('QUEUE_KEEPER_EXECUTOR')], [QUEUE_KEEPER]),
    call(StrategyManagerIface, STRATEGY_MANAGER, 'strategies', [], [STRATEGIES]),
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

const USD_TOKEN = '0x0000000000000000000000000000000000000348'

describe('Strategy keeper (W2)', () => {
  it('relays the checker() execPayload verbatim', async () => {
    // perform(WithdrawShortfall=2) as the contract would build it
    const payload = perform(2)
    const mocks = [checkerMock(true, payload), ...registryMocks(), ...queueMocks(1, 2, [expiringIn(1, 60)])]
    const result = await runWithRawMocks(functionDir, contextAt(NOW_MS), inputs, mocks, ethPrice())
    const { op } = onlyIntent(result)

    expect(op.opType).to.be.equal(2) // EvmCall
    expect(op.user).to.be.equal(SMART_ACCOUNT)
    expect(op.calls).to.have.lengthOf(1)
    expect(op.calls[0].target.toLowerCase()).to.be.equal(EXECUTOR)
    // byte-for-byte relay, not a re-encoding
    expect(op.calls[0].data.toLowerCase()).to.be.equal(payload.toLowerCase())
  })

  it('relays arbitrary bytes without touching them, under the flat maxFee', async () => {
    // not a valid perform() call — the relay must not inspect or reformat it,
    // and there is no action to price it by
    const payload = '0xdeadbeef' + 'ff'.repeat(67)
    const result = await runWithRawMocks(functionDir, contextAt(NOW_MS), inputs, [checkerMock(true, payload)])
    const { op, fee, token } = onlyIntent(result)

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

describe('W2 fee caps', () => {
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
      [checkerMock(true, payload), ...registryMocks(), ...extra],
      prices
    )

  describe('WithdrawShortfall: gwei ceiling ramping to the batch deadline', () => {
    const withdrawGas = WITHDRAW_GAS_PER_STRATEGY * 3n

    it('uses the floor ceiling while expiry is more than rampStart hours out', async () => {
      const result = await run(perform(2), queueMocks(1, 2, [expiringIn(1, 48)]))
      const { fee, token } = onlyIntent(result)
      expect(token).to.equal(USD_TOKEN)
      expect(fee).to.equal(gweiCapUsd(withdrawGas, '0.3'))
      expect(JSON.stringify(result.logs)).to.include('batch expires in 48.0h')
    })

    it('interpolates linearly between rampStart and rampEnd', async () => {
      // 18h left: 6 of the 12 ramp hours gone → 0.3 + (3 − 0.3) × 6/12 = 1.65 gwei
      const result = await run(perform(2), queueMocks(1, 2, [expiringIn(1, 18)]))
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd(withdrawGas, '1.65'))
    })

    it('uses the urgent ceiling from rampEnd hours out', async () => {
      const result = await run(perform(2), queueMocks(1, 2, [expiringIn(1, 6)]))
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd(withdrawGas, '3'))
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
      const result = await run(perform(2), queueMocks(1, 6, batches))
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd(withdrawGas, '3'))
      expect(JSON.stringify(result.logs)).to.include('batch expires in 6.0h')
    })

    it('does not scan past the executor MAX_BATCH_SCAN', async () => {
      // cursor 1, current 40: the contract sees ids 1..25 only, so the
      // urgent batch at id 26 must not raise the ceiling. Ids 2..25 are idle.
      const batches: Batch[] = [expiringIn(1, 48)]
      for (let id = 2; id <= 25; id++) batches.push({ id, canBeProcessed: true, pricedAt: NOW - HOUR, unprocessed: 0 })
      batches.push(expiringIn(26, 1))
      const result = await run(perform(2), queueMocks(1, 40, batches))
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd(withdrawGas, '0.3'))
    })

    it('scales the gas budget with the strategy count', async () => {
      const five = [
        ...STRATEGIES,
        '0x0000000000000000000000000000000000000a04',
        '0x0000000000000000000000000000000000000a05',
      ]
      const mocks = queueMocks(1, 2, [expiringIn(1, 48)]).concat([
        call(StrategyManagerIface, STRATEGY_MANAGER, 'strategies', [], [five]),
      ])
      // later mocks win the hash key — the 5-strategy list replaces the 3
      const result = await run(perform(2), mocks)
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd(WITHDRAW_GAS_PER_STRATEGY * 5n, '0.3'))
    })

    it('clamps to maxFee', async () => {
      const result = await run(perform(2), queueMocks(1, 2, [expiringIn(1, 6)]), { maxFee: '20' })
      expect(onlyIntent(result).fee).to.equal(ethers.parseUnits('20', 18))
      expect(JSON.stringify(result.logs)).to.include('clamped to maxFee')
    })

    it('falls back to the floor when no expiring batch is visible (view skew)', async () => {
      const result = await run(perform(2), queueMocks(2, 2, []))
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd(withdrawGas, '0.3'))
      expect(JSON.stringify(result.logs)).to.include('no expiring batch seen')
    })

    it('emits nothing when the deadline scan cannot be read', async () => {
      // cursor 1 < current 2, but batchInfo(1) is not mocked
      const result = await run(perform(2), queueMocks(1, 2, []))
      expect(result.success).to.be.true
      expect(result.intents).to.have.lengthOf(0)
      expect(JSON.stringify(result.logs)).to.include('read-error')
    })
  })

  describe('Sync: flat gwei ceiling', () => {
    it('prices the strategies it pokes at the sync ceiling', async () => {
      const result = await run(perform(5), [])
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd(SYNC_GAS_PER_STRATEGY * 3n, '0.25'))
    })

    it('tracks the oracle ETH price', async () => {
      const result = await run(perform(5), [], {}, ethPrice(chainId, '3100.5'))
      expect(onlyIntent(result).fee).to.equal(gweiCapUsd(SYNC_GAS_PER_STRATEGY * 3n, '0.25', '3100.5'))
    })

    it('emits nothing when the ETH price is unavailable', async () => {
      const result = await run(perform(5), [], {}, [])
      expect(result.success).to.be.true
      expect(result.intents).to.have.lengthOf(0)
      expect(JSON.stringify(result.logs)).to.include('read-error')
    })
  })

  describe('DepositExcess / Harvest / ProvideExitLiquidity: share of the amount', () => {
    const status = (action: number, amount: bigint): RawMock =>
      call(ExecutorIface, EXECUTOR, 'strategyUpkeepStatus', [], [action, amount])

    it('caps a deposit at amountFeeBps of the view amount', async () => {
      const amount = ethers.parseEther('0.10533')
      const result = await run(perform(3), [status(3, amount)])
      const { op, fee } = onlyIntent(result)
      expect(fee).to.equal(amountCapUsd(amount, 115n))
      // the amount prices the fee only — the payload is still the checker's
      expect(op.calls[0].data.toLowerCase()).to.equal(perform(3))
    })

    for (const action of [4, 6]) {
      it(`prices action ${action} the same way`, async () => {
        const amount = ethers.parseEther('0.5')
        const result = await run(perform(action), [status(action, amount)])
        expect(onlyIntent(result).fee).to.equal(amountCapUsd(amount, 115n))
      })
    }

    it('emits nothing when strategyUpkeepStatus() disagrees with checker()', async () => {
      const result = await run(perform(3), [status(2, ethers.parseEther('1'))])
      expect(result.success).to.be.true
      expect(result.intents).to.have.lengthOf(0)
      expect(JSON.stringify(result.logs)).to.include('would price the wrong work')
    })
  })

  describe('config-error', () => {
    const cases: [string, Record<string, unknown>, string][] = [
      ['a malformed gwei value', { rebalanceMaxGwei: '0,4' }, 'rebalanceMaxGwei'],
      ['more than 9 gwei decimals', { syncMaxGwei: '0.0000000001' }, 'syncMaxGwei'],
      ['a zero ceiling', { withdrawMaxGwei: '0' }, 'withdrawMaxGwei is 0'],
      ['an urgent ceiling below the floor', { withdrawUrgentMaxGwei: '0.2' }, 'withdrawUrgentMaxGwei'],
      ['a ramp that ends before it starts', { withdrawRampStartHours: 12 }, 'withdrawRampStartHours'],
      ['a zero amount share', { amountFeeBps: 0 }, 'amountFeeBps'],
      ['a malformed maxFee', { maxFee: '$50' }, 'maxFee'],
    ]
    for (const [name, overrides, field] of cases) {
      it(`emits nothing on ${name}, naming the field`, async () => {
        const result = await run(perform(5), [], overrides)
        expect(result.success).to.be.true
        expect(result.intents).to.have.lengthOf(0)
        const logs = JSON.stringify(result.logs)
        expect(logs).to.include('config-error')
        expect(logs).to.include(field)
      })
    }
  })
})

describe('W2 no-op Rebalance suppression (keepers#27)', () => {
  const mainnetInputs = {
    chainId: 1,
    executor: MAINNET_EXECUTOR,
    smartAccount: SMART_ACCOUNT,
    ...feeInputs,
  }
  const mainnetContext = (): Context => contextAt(NOW_MS, 1)

  // The incident payload is chain-verbatim; cross-check it against an
  // independent encoder (ethers + the vendored ABI) so the fixture cannot
  // drift from the contract's own encoding.
  it('pins the incident payload to perform(Rebalance=1)', () => {
    expect(REBALANCE_PAYLOAD).to.equal(perform(1))
    for (const block of INCIDENT_BLOCKS) {
      const checker = incidentMocks(block).find((m) => m.data === SEL.checker)
      expect(checker, `checker() mock for block ${block}`).to.not.be.undefined
      expect(checker!.value).to.include(REBALANCE_PAYLOAD.slice(2))
    }
  })

  // 26004768: only S2 unhealthy, not calm (spot 197958 vs 30-min TWAP 198043
  // ± 100; 60s TWAP 197939 outside). 26004793: all three unhealthy, none
  // calm. 26004820: S1 and S3 unhealthy (spot deviations -121 / +121 vs the
  // 30-min TWAP), neither calm. Every incident tick must emit zero intents
  // with one suppressed-noop-rebalance log line.
  const expectedDeviations: Record<number, string[]> = {
    26004768: ['spotDeviation=-85', 'shortTwapDeviation=-104'],
    26004793: ['spotDeviation=-140', 'spotDeviation=-142', 'spotDeviation=141'],
    26004820: ['spotDeviation=-121', 'spotDeviation=121'],
  }
  for (const block of INCIDENT_BLOCKS) {
    it(`suppresses the guaranteed-revert Rebalance at block ${block}`, async () => {
      const result = await runWithRawMocks(functionDir, mainnetContext(), mainnetInputs, incidentMocks(block))
      expect(result.success).to.be.true
      expect(result.intents).to.have.lengthOf(0)

      const logs = JSON.stringify(result.logs)
      expect(logs).to.include('suppressed-noop-rebalance')
      for (const deviation of expectedDeviations[block]) expect(logs).to.include(deviation)
    })
  }

  // Block 26004820 with S1 flipped to calm: spot tick and 60s TWAP moved
  // onto the 30-min TWAP (197986), so S1 is `calm ∧ ¬healthy` and the batch
  // is real work. S3 stays chain-verbatim ¬calm — only some strategies
  // being ¬calm must still relay. Synthetic overrides are encoded with
  // ethers (independent of the function), not re-derived from its code.
  const partialCalmMocks = (): RawMock[] =>
    incidentMocks(26004820)
      .map((m) => {
        // slot0/observe are read on S1's pool (P1), not on the strategy
        if (m.to === P1 && m.data === SEL.slot0) {
          // chain slot0 with the tick moved onto the 30-min TWAP (197986)
          return {
            ...m,
            value: CODER.encode(
              ['uint160', 'int24', 'uint16', 'uint16', 'uint16', 'uint8', 'bool'],
              ['0x4d4b1f41899f4fffbe8aee538641', 197986, 1009, 90, 90, 0, true]
            ),
          }
        }
        if (m.to === P1 && m.data === SEL.observe60) {
          // 60s mean tick exactly 197986: delta = 197986 * 60
          return {
            ...m,
            value: CODER.encode(
              ['int56[]', 'uint160[]'],
              [
                [0, 197986 * 60],
                [0, 0],
              ]
            ),
          }
        }
        return m
      })
      .concat(FEE_READS_26004820)

  it('relays byte-identical when an unhealthy strategy is calm (partial ¬calm)', async () => {
    const result = await runWithRawMocks(functionDir, mainnetContext(), mainnetInputs, partialCalmMocks(), ethPrice(1))
    const { op, fee } = onlyIntent(result)
    expect(op.calls[0].target.toLowerCase()).to.be.equal(MAINNET_EXECUTOR)
    expect(op.calls[0].data.toLowerCase()).to.be.equal(REBALANCE_PAYLOAD)
    expect(JSON.stringify(result.logs)).to.include('relay')

    // S1 and S3 selected; no priced batch in window (cursor == current == 2)
    expect(fee).to.equal(gweiCapUsd(REBALANCE_GAS_PER_STRATEGY * 2n, '0.4'))
  })

  it('raises the Rebalance ceiling for a withdrawal it masks near expiry', async () => {
    // Synthetic on top of the chain reads: batch 2 priced and owing a user,
    // 6h from expiry. checker() returns Rebalance first, so the shortfall
    // behind it can only settle if the Rebalance itself goes through.
    const mocks = partialCalmMocks().concat([
      call(ExitQueueIface, MAINNET_EXIT_QUEUE, 'currentBatchId', [], [3]),
      call(ExitQueueIface, MAINNET_EXIT_QUEUE, 'batchInfo', [2], [true, 10n ** 18n, 0, 0, NOW + 6 * HOUR - WINDOW]),
      call(ExitQueueIface, MAINNET_EXIT_QUEUE, 'unprocessedUsersCount', [2], [1]),
    ])
    const result = await runWithRawMocks(functionDir, mainnetContext(), mainnetInputs, mocks, ethPrice(1))
    expect(onlyIntent(result).fee).to.equal(gweiCapUsd(REBALANCE_GAS_PER_STRATEGY * 2n, '3'))
    expect(JSON.stringify(result.logs)).to.include('raised for the withdrawal it masks')
  })

  it('emits nothing on a read error (unavailable TWAP) instead of classifying', async () => {
    // Block 26004768 without the 60s observe response for S2's pool: an
    // observe failure is an unavailable TWAP on-chain (calm == false), but
    // off-chain it is indistinguishable from an RPC error — fail closed,
    // emit nothing.
    const mocks = incidentMocks(26004768).filter((m) => !(m.to === P2 && m.data === SEL.observe60))
    const result = await runWithRawMocks(functionDir, mainnetContext(), mainnetInputs, mocks)
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
    expect(JSON.stringify(result.logs)).to.include('read-error')
  })
})
