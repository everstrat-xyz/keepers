/**
 * Run the compiled W2 relay against the LIVE Mimic oracle and a real
 * deployment, without creating a trigger.
 *
 * W2 forwards `StrategyKeeperExecutor.checker()`'s execPayload verbatim, so
 * this answers the questions that matter before binding: does the oracle
 * serve the `checker()` call at all, does the payload it returns decode into
 * a `perform` call, and — on a tick with work — does the oracle price the
 * native token the fee cap converts through. `npm test` mocks all three.
 *
 *   cd mimic-functions/strategy-keeper && npm run build && npm run try-function
 *
 * It settles nothing: intents are printed, never sent.
 */
import { randomEvmAddress } from '@mimicprotocol/sdk'
import { runFunction } from '@mimicprotocol/test-ts'

import { inputs } from './inputs.js'

const FUNCTION_DIR = './build'
const ORACLE_URL = process.env.ORACLE_URL ?? 'https://api-protocol.mimic.fi/oracle'

async function main(): Promise<void> {
  const functionInputs = inputs()

  const context = {
    user: randomEvmAddress(),
    settlers: [{ address: randomEvmAddress(), chainId: functionInputs.chainId }],
    timestamp: Date.now(),
  }

  const result = await runFunction(FUNCTION_DIR, context, { inputs: functionInputs }, ORACLE_URL)
  console.log(JSON.stringify(result, null, 2))

  const logs: string[] = (result.logs ?? []).map((entry: unknown) => JSON.stringify(entry))
  // One log class per tick (src/function.ts): relay, suppressed-noop-rebalance,
  // read-error, config-error, or the idle `no upkeep` line.
  const line = logs.find((l) => l.includes('W2 '))

  console.log('\n--- summary ---')
  console.log(line ?? 'no decision logged — the tick aborted before reading checker()')
  // No intent with canExec=true means the relay dropped a payload it should
  // have forwarded; an intent with canExec=false would be worse.
  console.log(`intents emitted: ${result.intents?.length ?? 0}`)
}

main().catch((error) => {
  console.error('Error running function:', error)
  process.exit(1)
})
