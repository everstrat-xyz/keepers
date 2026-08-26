/**
 * W1 queue-keeper — Mimic function tests.
 *
 * Uses the raw-mock harness (tests/helpers.ts): @mimicprotocol/test-ts's mock
 * schema only admits primitive abiTypes, but W1's read layer is built on
 * batchInfo/requestInfo/queueUpkeepStatus tuples and address[] user lists. The
 * harness feeds oracle responses keyed by the same EIP-712 query hash the
 * runner computes, so the compiled WASM runs unmodified.
 */
import { expect } from 'chai'
import { AbiCoder, Interface, zeroPadValue } from 'ethers'

import ExecutorAbi from '../abis/QueueKeeperExecutor.json'

import { type Context, type EvmCallOperation, mockPrimitive, mockTuple, type RawMock, runWithRawMocks } from './helpers'

const ExecutorIface = new Interface(ExecutorAbi)
const Coder = new AbiCoder()

const functionDir = './build'
const chainId = 10

// Deterministic fixtures. Addresses double as read-target discriminators.
const EXECUTOR = '0x0000000000000000000000000000000000000100'
const EXIT_QUEUE = '0x0000000000000000000000000000000000000200'
const CONTROLLER = '0x0000000000000000000000000000000000000300'
const AMM = '0x0000000000000000000000000000000000000350'
const SMART_ACCOUNT = '0x0000000000000000000000000000000000000400'
const USER_A = '0x0000000000000000000000000000000000000aa1'
const USER_B = '0x0000000000000000000000000000000000000aa2'
const MIMIC_HELPER = '0x2ef71e27560874b932ef1cf9e95d340595a92f44'

// Selectors for every read the function can emit.
const SEL = {
  paused: '0x5c975abb',
  currentBatchId: '0x0a763da1',
  maxBatchProcessingTime: '0x89c5a797',
  batchInfo: '0x815bda47',
  unprocessedUsersCount: '0xc1a65c6f',
  unprocessedUsers: '0x2a6d3c18',
  requestInfo: '0xd4be074f',
  queueUpkeepStatus: '0xfed4315e',
  nextBatchIdToProcess: '0x092a786c',
  minBatchAge: '0x13ab3b2e',
  maxUsersPerUpkeep: '0x0ab6be7a',
  // environment.getNativeTokenBalance → balanceOf(controller) on the helper
  nativeBalance: '0xeffd663c',
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
  amm: AMM,
  executor: EXECUTOR,
  controller: CONTROLLER,
  exitQueue: EXIT_QUEUE,
  smartAccount: SMART_ACCOUNT,
  maxBatches: 250,
  maxRequestsPerBatch: 50,
  maxFee: '1',
}

function call(to: string, selector: string, args: unknown[] = []): string {
  return selector + (args.length ? Coder.encode(Array(args.length).fill('uint256'), args).slice(2) : '')
}

// The pause fan-out mirrors QueueKeeperExecutor._queueUpkeepStatus: executor,
// Controller, ExitQueue AND AMM.
const pausedReads = (executor: boolean, controller: boolean, exitQueue: boolean, amm = false): RawMock[] => [
  mockPrimitive(EXECUTOR, SEL.paused, executor, 'bool'),
  mockPrimitive(CONTROLLER, SEL.paused, controller, 'bool'),
  mockPrimitive(EXIT_QUEUE, SEL.paused, exitQueue, 'bool'),
  mockPrimitive(AMM, SEL.paused, amm, 'bool'),
]

const configReads = (o: {
  currentBatchId: string
  maxProcessing: string
  cursor: string
  minBatchAge: string
  maxUsers: string
  balance: string
}): RawMock[] => [
  mockPrimitive(EXIT_QUEUE, SEL.currentBatchId, o.currentBatchId, 'uint256'),
  mockPrimitive(EXIT_QUEUE, SEL.maxBatchProcessingTime, o.maxProcessing, 'uint256'),
  mockPrimitive(EXECUTOR, SEL.nextBatchIdToProcess, o.cursor, 'uint256'),
  mockPrimitive(EXECUTOR, SEL.minBatchAge, o.minBatchAge, 'uint256'),
  mockPrimitive(EXECUTOR, SEL.maxUsersPerUpkeep, o.maxUsers, 'uint256'),
  mockPrimitive(MIMIC_HELPER, SEL.nativeBalance, o.balance, 'uint256', [CONTROLLER]),
]

function batchInfoMock(batchId: string, b: [boolean, string, string, string, string]): RawMock {
  return mockTuple(EXIT_QUEUE, call(EXIT_QUEUE, SEL.batchInfo, [batchId]), '(bool,uint256,uint256,uint256,uint256)', [
    b,
  ])
}

function unprocessedCountMock(batchId: string, count: string): RawMock {
  return mockPrimitive(EXIT_QUEUE, call(EXIT_QUEUE, SEL.unprocessedUsersCount, [batchId]), count, 'uint256')
}

function unprocessedUsersMock(batchId: string, end: string, users: string[]): RawMock {
  const data = SEL.unprocessedUsers + Coder.encode(['uint256', 'uint256', 'uint256'], [batchId, '0', end]).slice(2)
  // the wrapper decodes the response as address[]; the runner's abi_decode
  // then yields the JSON form the wrapper JSON.parses
  return mockTuple(EXIT_QUEUE, data, 'address[]', [users])
}

function requestInfoMock(batchId: string, user: string, r: [boolean, boolean, string, string, string]): RawMock {
  const data = SEL.requestInfo + Coder.encode(['uint256', 'address'], [batchId, user]).slice(2)
  return mockTuple(EXIT_QUEUE, data, '(bool,bool,uint256,uint256,uint256)', [r])
}

function queueUpkeepStatusMock(action: number, batchId: string, count: string): RawMock {
  return mockTuple(EXECUTOR, SEL.queueUpkeepStatus, '(uint8,uint256,uint256)', [[action, batchId, count]])
}

const word = (n: number | string): string => zeroPadValue('0x' + BigInt(n).toString(16).padStart(2, '0'), 32)

describe('Queue keeper (W1)', () => {
  it('emits nothing when the executor is paused', async () => {
    const context = contextAt(Date.now())
    const result = await runWithRawMocks(functionDir, context, inputs, [...pausedReads(true, false, false)])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })

  it('emits nothing when the controller is paused', async () => {
    const context = contextAt(Date.now())
    const result = await runWithRawMocks(functionDir, context, inputs, [...pausedReads(false, true, false)])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })

  it('emits nothing when the exit queue is paused', async () => {
    const context = contextAt(Date.now())
    const result = await runWithRawMocks(functionDir, context, inputs, [...pausedReads(false, false, true)])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })

  // Regression: the AMM is in _queueUpkeepStatus's pause fan-out, and skipping
  // it here is not a wasted tick. Controller.priceBatch is whenNotPaused on the
  // Controller only and AMM.eveBasePriceInETH() is an ungated view, so a
  // PriceBatch proposed during an AMM-only pause would land — pricing a batch
  // the on-chain view refuses to recommend.
  it('emits nothing when only the AMM is paused', async () => {
    const context = contextAt(Date.now())
    const result = await runWithRawMocks(functionDir, context, inputs, [...pausedReads(false, false, false, true)])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })

  it('prices the current batch once it is old enough', async () => {
    const nowSec = Math.floor(Date.now() / 1000)
    const twoDays = 2 * 86400
    const context = contextAt(Date.now())

    const mocks: RawMock[] = [
      ...pausedReads(false, false, false),
      ...configReads({
        currentBatchId: '3',
        maxProcessing: '259200',
        cursor: '2',
        minBatchAge: '86400',
        maxUsers: '20',
        balance: '1000000000000000000',
      }),
      // batch 2: priced long ago, empty → skippable
      batchInfoMock('2', [true, '0', '0', String(nowSec - 30 * 86400), String(nowSec - 30 * 86400)]),
      unprocessedCountMock('2', '0'),
      // batch 3 (current): unpriced, 5 unprocessed, created 2 days ago
      batchInfoMock('3', [true, '0', '0', String(nowSec - twoDays), '0']),
      unprocessedCountMock('3', '5'),
      // phase-2 walk loads the users of the first non-skippable batch; they
      // are too expensive for the balance, so decide falls through to pricing
      unprocessedUsersMock('3', '5', [USER_A, USER_B]),
      requestInfoMock('3', USER_A, [
        false,
        false,
        '2000000000000000000000',
        '5000000000000000000',
        '50000000000000000',
      ]),
      requestInfoMock('3', USER_B, [
        false,
        false,
        '2000000000000000000000',
        '5000000000000000000',
        '50000000000000000',
      ]),
      // cross-check view agrees there is nothing at the cursor
      queueUpkeepStatusMock(1, '3', '0'),
    ]

    const result = await runWithRawMocks(functionDir, context, inputs, mocks)
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(1)

    const op = result.intents[0].operations[0] as EvmCallOperation
    expect(op.opType).to.be.equal(2) // EvmCall
    expect(op.user).to.be.equal(SMART_ACCOUNT)
    expect(op.calls).to.have.lengthOf(1)
    expect(op.calls[0].target).to.be.equal(EXECUTOR)
    // perform(PriceBatch=1, abi.encode(uint256(3)))
    const expected = ExecutorIface.encodeFunctionData('perform', [1, word(3)])
    expect(op.calls[0].data.toLowerCase()).to.be.equal(expected.toLowerCase())
  })

  it('processes an affordable prefix of the oldest unpriced-scan batch', async () => {
    const nowSec = Math.floor(Date.now() / 1000)
    const day = 86400
    const context = contextAt(Date.now())

    const price = '2000000000000000000000' // 2000e18 EVE/ETH
    const reqCost = (tokens: string): string => (BigInt(tokens) * BigInt(price)) / BigInt(10 ** 18)

    const mocks: RawMock[] = [
      ...pausedReads(false, false, false),
      ...configReads({
        currentBatchId: '3',
        maxProcessing: '259200',
        cursor: '1',
        minBatchAge: '86400',
        maxUsers: '20',
        // exactly covers user A but not A+B
        balance: reqCost('1000000000000000000').toString(),
      }),
      // batch 1: priced recently, 2 unprocessed, in window
      batchInfoMock('1', [true, price, '3000000000000000000', String(nowSec - day), String(nowSec - day)]),
      unprocessedCountMock('1', '2'),
      unprocessedUsersMock('1', '2', [USER_A, USER_B]),
      requestInfoMock('1', USER_A, [false, false, price, '1000000000000000000', '50000000000000000']),
      requestInfoMock('1', USER_B, [false, false, price, '1000000000000000000', '50000000000000000']),
      // batch 2: skippable filler between cursor and current
      batchInfoMock('2', [true, '0', '0', String(nowSec - 30 * day), String(nowSec - 30 * day)]),
      unprocessedCountMock('2', '0'),
      // batch 3 (current): young, so PriceBatch is not chosen this tick
      batchInfoMock('3', [true, '0', '0', String(nowSec), '0']),
      unprocessedCountMock('3', '0'),
      queueUpkeepStatusMock(2, '1', '1'),
    ]

    const result = await runWithRawMocks(functionDir, context, inputs, mocks)
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(1)

    const op = result.intents[0].operations[0] as EvmCallOperation
    // perform(ProcessRequests=2, abi-words(batchId=1, start=0, end=1))
    const params = word(1) + word(0).slice(2) + word(1).slice(2)
    const expected = ExecutorIface.encodeFunctionData('perform', [2, params])
    expect(op.calls[0].data.toLowerCase()).to.be.equal(expected.toLowerCase())
  })

  it('advances the cursor past dead batches', async () => {
    const nowSec = Math.floor(Date.now() / 1000)
    const day = 86400
    const context = contextAt(Date.now())

    const dead = (id: string, ageDays: number): RawMock[] => [
      batchInfoMock(id, [true, '0', '0', String(nowSec - ageDays * day), String(nowSec - ageDays * day)]),
      unprocessedCountMock(id, '0'),
    ]

    const mocks: RawMock[] = [
      ...pausedReads(false, false, false),
      ...configReads({
        currentBatchId: '3',
        maxProcessing: '259200',
        cursor: '1',
        minBatchAge: '86400',
        maxUsers: '20',
        balance: '0',
      }),
      ...dead('1', 30),
      ...dead('2', 30),
      // current batch young and empty — nothing to price
      batchInfoMock('3', [true, '0', '0', String(nowSec), '0']),
      unprocessedCountMock('3', '0'),
      queueUpkeepStatusMock(3, '3', '0'),
    ]

    const result = await runWithRawMocks(functionDir, context, inputs, mocks)
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(1)

    const op = result.intents[0].operations[0] as EvmCallOperation
    // perform(AdvanceCursor=3, abi.encode(uint256(3)))
    const expected = ExecutorIface.encodeFunctionData('perform', [3, word(3)])
    expect(op.calls[0].data.toLowerCase()).to.be.equal(expected.toLowerCase())
  })

  it('refuses PriceBatch when the scan was truncated', async () => {
    const nowSec = Math.floor(Date.now() / 1000)
    const day = 86400
    const context = contextAt(Date.now())

    const dead = (id: string, ageDays: number): RawMock[] => [
      batchInfoMock(id, [true, '0', '0', String(nowSec - ageDays * day), String(nowSec - ageDays * day)]),
      unprocessedCountMock(id, '0'),
    ]

    // cursor 0, current 30, maxBatches 250 — but the input caps at 250 so no
    // truncation here; instead drive truncation via many dead batches with a
    // tiny maxBatches.
    const truncatedInputs = { ...inputs, maxBatches: 2 }
    const mocks: RawMock[] = [
      ...pausedReads(false, false, false),
      ...configReads({
        currentBatchId: '5',
        maxProcessing: '259200',
        cursor: '1',
        minBatchAge: '86400',
        maxUsers: '20',
        balance: '1000000000000000000',
      }),
      ...dead('1', 30),
      ...dead('2', 30),
      // scan stops after maxBatches=2 → batches 3..5 unseen, truncation recorded
      queueUpkeepStatusMock(3, '3', '0'),
    ]

    const result = await runWithRawMocks(functionDir, context, truncatedInputs, mocks)
    expect(result.success).to.be.true
    // PriceBatch is off the table — the current batch was never read. The
    // bounded cursor walk is still safe: batches 1–2 are dead, batch 3 is
    // unread (conservatively not skippable), so the claim lands there.
    expect(result.intents).to.have.lengthOf(1)
    const op = result.intents[0].operations[0] as EvmCallOperation
    const expected = ExecutorIface.encodeFunctionData('perform', [3, word(3)])
    expect(op.calls[0].data.toLowerCase()).to.be.equal(expected.toLowerCase())
  })

  it('emits nothing when there is no work', async () => {
    const nowSec = Math.floor(Date.now() / 1000)
    const context = contextAt(Date.now())

    const mocks: RawMock[] = [
      ...pausedReads(false, false, false),
      ...configReads({
        currentBatchId: '1',
        maxProcessing: '259200',
        cursor: '1',
        minBatchAge: '86400',
        maxUsers: '20',
        balance: '1000000000000000000',
      }),
      // current batch: young and empty
      batchInfoMock('1', [true, '0', '0', String(nowSec), '0']),
      unprocessedCountMock('1', '0'),
      queueUpkeepStatusMock(0, '0', '0'),
    ]

    const result = await runWithRawMocks(functionDir, context, inputs, mocks)
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })

  // The cross-check against queueUpkeepStatus is shadow mode's exit criterion
  // ("zero unexplained divergences"), so the classification gets its own specs
  // rather than being exercised silently by the scenarios above.
  describe('divergence cross-check', () => {
    const price = '2000000000000000000000' // 2000e18 EVE/ETH
    const reqCost = (tokens: string): bigint => (BigInt(tokens) * BigInt(price)) / BigInt(10 ** 18)

    // Batch 1 holds two equally-priced requests and the Controller balance
    // covers exactly one, so decide always claims a one-request prefix. Only
    // the on-chain view's answer changes between the cases below.
    const oneAffordableOfTwo = (status: RawMock): RawMock[] => {
      const nowSec = Math.floor(Date.now() / 1000)
      const day = 86400
      return [
        ...pausedReads(false, false, false),
        ...configReads({
          currentBatchId: '2',
          maxProcessing: '259200',
          cursor: '1',
          minBatchAge: '86400',
          maxUsers: '20',
          balance: reqCost('1000000000000000000').toString(),
        }),
        batchInfoMock('1', [true, price, '2000000000000000000', String(nowSec - day), String(nowSec - day)]),
        unprocessedCountMock('1', '2'),
        unprocessedUsersMock('1', '2', [USER_A, USER_B]),
        requestInfoMock('1', USER_A, [false, false, price, '1000000000000000000', '50000000000000000']),
        requestInfoMock('1', USER_B, [false, false, price, '1000000000000000000', '50000000000000000']),
        batchInfoMock('2', [true, '0', '0', String(nowSec), '0']),
        unprocessedCountMock('2', '0'),
        status,
      ]
    }

    const runAgainstView = async (action: number, batchId: string, count: string): Promise<string> => {
      const result = await runWithRawMocks(
        functionDir,
        contextAt(Date.now()),
        inputs,
        oneAffordableOfTwo(queueUpkeepStatusMock(action, batchId, count))
      )
      expect(result.success).to.be.true
      // Classification never suppresses the decision — the intent goes out
      // either way, and the class is what ops reads.
      expect(result.intents).to.have.lengthOf(1)
      return JSON.stringify(result.logs)
    }

    it('reports match when the view names the same batch and prefix', async () => {
      expect(await runAgainstView(2, '1', '1')).to.include('divergence=match')
    })

    it('reports intended-improvement when it claims a shorter prefix than the view', async () => {
      const logs = await runAgainstView(2, '1', '2')
      expect(logs).to.include('divergence=intended-improvement')
    })

    it('reports bug when the view cannot support the claimed prefix', async () => {
      const logs = await runAgainstView(2, '1', '0')
      expect(logs).to.include('divergence=bug')
      expect(logs).to.include('W1 divergence from on-chain view is unexplained')
    })
  })
})
