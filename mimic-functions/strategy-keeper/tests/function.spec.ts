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
 */
import { expect } from 'chai'
import { AbiCoder, Interface } from 'ethers'

import ExecutorAbi from '../abis/StrategyKeeperExecutor.json'

import { type Context, type EvmCallOperation, mockTuple, type RawMock, runWithRawMocks } from './helpers'
import {
  EXECUTOR as MAINNET_EXECUTOR,
  INCIDENT_BLOCKS,
  incidentMocks,
  P1,
  P2,
  REBALANCE_PAYLOAD,
  SEL,
} from './incident-fixtures'

const ExecutorIface = new Interface(ExecutorAbi)
const CODER = AbiCoder.defaultAbiCoder()

const functionDir = './build'
const chainId = 10

// Deterministic fixtures. Addresses double as read-target discriminators.
const EXECUTOR = '0x0000000000000000000000000000000000000100'
const SMART_ACCOUNT = '0x0000000000000000000000000000000000000400'

// checker() selector — StrategyKeeperExecutorUtils.encodeChecker()
const SEL_CHECKER = '0xcf5303cf'

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
  maxFee: '1',
}

const checkerMock = (canExec: boolean, execPayload: string): RawMock =>
  mockTuple(EXECUTOR, SEL_CHECKER, '(bool,bytes)', [[canExec, execPayload]])

describe('Strategy keeper (W2)', () => {
  it('relays the checker() execPayload verbatim', async () => {
    // perform(WithdrawShortfall=2) as the contract would build it
    const payload = ExecutorIface.encodeFunctionData('perform', [2])
    const result = await runWithRawMocks(functionDir, contextAt(Date.now()), inputs, [checkerMock(true, payload)])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(1)

    const op = result.intents[0].operations[0] as EvmCallOperation
    expect(op.opType).to.be.equal(2) // EvmCall
    expect(op.user).to.be.equal(SMART_ACCOUNT)
    expect(op.calls).to.have.lengthOf(1)
    expect(op.calls[0].target.toLowerCase()).to.be.equal(EXECUTOR)
    // byte-for-byte relay, not a re-encoding
    expect(op.calls[0].data.toLowerCase()).to.be.equal(payload.toLowerCase())
  })

  it('relays arbitrary bytes without touching them', async () => {
    // not a valid perform() call — the relay must not inspect or reformat it
    const payload = '0xdeadbeef' + 'ff'.repeat(67)
    const result = await runWithRawMocks(functionDir, contextAt(Date.now()), inputs, [checkerMock(true, payload)])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(1)

    const op = result.intents[0].operations[0] as EvmCallOperation
    expect(op.calls[0].data.toLowerCase()).to.be.equal(payload.toLowerCase())
  })

  it('emits nothing when canExec is false', async () => {
    const result = await runWithRawMocks(functionDir, contextAt(Date.now()), inputs, [checkerMock(false, '0x')])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })

  it('emits nothing when checker() reverts', async () => {
    // no mock for the checker() read at all → oracle has no response
    const result = await runWithRawMocks(functionDir, contextAt(Date.now()), inputs, [])
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(0)
  })
})

describe('W2 no-op Rebalance suppression (keepers#27)', () => {
  const mainnetInputs = {
    chainId: 1,
    executor: MAINNET_EXECUTOR,
    smartAccount: SMART_ACCOUNT,
    maxFee: '1',
  }
  const mainnetContext = (): Context => ({
    user: '0x756f45e3fa69347a9a973a725e3c98bc4db0b5a0',
    settlers: [{ address: '0x0000000000000000000000000000000000000500', chainId: 1 }],
    timestamp: Date.now(),
  })

  // The incident payload is chain-verbatim; cross-check it against an
  // independent encoder (ethers + the vendored ABI) so the fixture cannot
  // drift from the contract's own encoding.
  it('pins the incident payload to perform(Rebalance=1)', () => {
    expect(REBALANCE_PAYLOAD).to.equal(ExecutorIface.encodeFunctionData('perform', [1]))
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

  it('relays byte-identical when an unhealthy strategy is calm (partial ¬calm)', async () => {
    // Block 26004820 with S1 flipped to calm: spot tick and 60s TWAP moved
    // onto the 30-min TWAP (197986), so S1 is `calm ∧ ¬healthy` and the batch
    // is real work. S3 stays chain-verbatim ¬calm — only some strategies
    // being ¬calm must still relay. Synthetic overrides are encoded with
    // ethers (independent of the function), not re-derived from its code.
    const mocks = incidentMocks(26004820).map((m) => {
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

    const result = await runWithRawMocks(functionDir, mainnetContext(), mainnetInputs, mocks)
    expect(result.success).to.be.true
    expect(result.intents).to.have.lengthOf(1)

    const op = result.intents[0].operations[0] as EvmCallOperation
    expect(op.calls[0].target.toLowerCase()).to.be.equal(MAINNET_EXECUTOR)
    expect(op.calls[0].data.toLowerCase()).to.be.equal(REBALANCE_PAYLOAD)
    expect(JSON.stringify(result.logs)).to.include('relay')
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
