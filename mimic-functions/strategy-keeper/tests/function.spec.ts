/**
 * W2 strategy-keeper — Mimic function tests.
 *
 * The function is a pure relay: whatever `checker()` returns as execPayload
 * must arrive in the intent byte-for-byte, and nothing else may be emitted.
 * These tests pin both properties with adversarial payloads (arbitrary
 * selector, dynamic bytes padding) plus the no-work and error paths.
 */
import { expect } from 'chai'
import { Interface } from 'ethers'

import ExecutorAbi from '../abis/StrategyKeeperExecutor.json'

import { type Context, type EvmCallOperation, mockTuple, type RawMock, runWithRawMocks } from './helpers'

const ExecutorIface = new Interface(ExecutorAbi)

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
    // perform(Harvest=2) as the contract would build it
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
