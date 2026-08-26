/**
 * Raw-mock harness for the queue-keeper Mimic function.
 *
 * `runFunction` from @mimicprotocol/test-ts rejects tuple/array abiTypes in its
 * mock schema, but W1's read layer lives on `batchInfo`, `requestInfo`,
 * `unprocessedUsers` and `queueUpkeepStatus` — all tuples or arrays. The WASM
 * side never sees the mock schema: it emits an EvmCallQuery, the runner hashes
 * it, and looks the hash up in `context.oracleResponses`.
 *
 * This helper mirrors test-ts's own `runFunction` (same context shape, same
 * `OracleSigner.getQueryHash` keys, same ABI-encode step for the response
 * value) but takes pre-encoded hex responses, so any return shape can be
 * mocked. Primitives are still accepted via (value, abiType) pairs.
 */
import { runExecution } from '@mimicprotocol/runner-node'
import { EthersSigner, type EvmCallOperation, type Intent, OpType, OracleSigner } from '@mimicprotocol/sdk'
import { AbiCoder, ethers, Wallet } from 'ethers'
import * as http from 'http'
import type { AddressInfo } from 'net'
import * as path from 'path'

const SIGNER = new OracleSigner(EthersSigner.fromPrivateKey(Wallet.createRandom().privateKey))
const CODER = AbiCoder.defaultAbiCoder()

export interface Context {
  user: string
  settlers: { address: string; chainId: number }[]
  timestamp: number
}

/** One mocked read: target + calldata → raw ABI-encoded return value (hex). */
export interface RawMock {
  to: string
  data: string
  value: string
}

export function mockPrimitive(
  to: string,
  data: string,
  value: unknown,
  abiType: string,
  params: string[] = []
): RawMock {
  const calldata = data + (params.length ? CODER.encode(Array(params.length).fill('address'), params).slice(2) : '')
  return { to, data: calldata, value: CODER.encode([abiType], [value]) }
}

export function mockTuple(to: string, data: string, abiType: string, values: unknown[]): RawMock {
  return { to, data, value: CODER.encode([abiType], values) }
}

export interface RunResult {
  success: boolean
  intents: Intent[]
  logs: string[]
}

export async function runWithRawMocks(
  functionDir: string,
  context: Context,
  inputs: Record<string, unknown>,
  mocks: RawMock[],
  debug = false
): Promise<RunResult> {
  const oracleResponses: Record<string, unknown[]> = {}
  const mockKeys = new Map<string, string>()
  for (const m of mocks) {
    const params = { to: m.to, chainId: inputs.chainId, timestamp: context.timestamp, data: m.data }
    const hash = SIGNER.getQueryHash(params, 'EvmCallQuery')
    mockKeys.set(hash, `${m.to} ${m.data}`)
    oracleResponses[hash] = [
      { result: { value: m.value }, query: { params, name: 'EvmCallQuery', hash }, signature: '' },
    ]
  }

  const fullContext = {
    timestamp: context.timestamp,
    consensusThreshold: 1,
    user: context.user,
    settlers: context.settlers,
    triggerSig: '0x',
    triggerPayload: { type: 0, data: '0x' },
    oracleResponses,
  }

  let oracleUrl = ''
  let server: http.Server | null = null
  const captured: { to: string; data: string }[] = []
  if (debug) {
    server = http.createServer((req, res) => {
      let body = ''
      req.on('data', (c: Buffer) => (body += c.toString()))
      req.on('end', () => {
        captured.push(JSON.parse(body))
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end('{}')
      })
    })
    await new Promise<void>((r) => (server as http.Server).listen(0, '127.0.0.1', () => r()))
    oracleUrl = `http://127.0.0.1:${(server!.address() as AddressInfo).port}`
  }

  const functionPath = path.join(functionDir, 'function.wasm')
  const result = await runExecution(functionPath, JSON.stringify(inputs), JSON.stringify(fullContext), oracleUrl)
  if (debug) {
    server?.close()
    if (captured.length) {
      console.error('UNMOCKED READS (wasm asked the oracle for these):')
      for (const c of captured) console.error(`  to=${c.to} data=${c.data}`)
    }
  }

  const intents = (JSON.parse(result.intentsJson) as RunnerIntentJson[]).map((intent) => ({
    ...intent,
    operations: intent.operations.map((op) => ({ ...op, chainId: Number(op.chainId) })),
  }))
  return { success: result.success, intents, logs: JSON.parse(result.logsJson) }
}

/** The runner's raw JSON intent shape, before chainId is normalized. */
interface RunnerIntentJson {
  operations: { chainId: number | string; [k: string]: unknown }[]
  [k: string]: unknown
}

export { ethers, OpType }
export type { EvmCallOperation }
