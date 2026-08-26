/**
 * Run the compiled W1 function against the LIVE Mimic oracle and a real
 * deployment, without creating a trigger.
 *
 * This is the only thing in the repo that exercises the read path for real.
 * `npm test` keys its mocks by the same query hash the function computes, from
 * the same understanding that wrote the function — so it cannot catch a query
 * shape the oracle rejects, or a return the generated wrapper decodes wrongly.
 * Run this against the testnet deployment before binding a trigger.
 *
 *   cd mimic-functions/queue-keeper && npm run build && npm run try-function
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
    // Milliseconds, as the runner supplies them. readState divides by 1000 —
    // handing it seconds here would silently rewind every age check by ~55
    // years and make every batch look brand new.
    timestamp: Date.now(),
  }

  const result = await runFunction(FUNCTION_DIR, context, { inputs: functionInputs }, ORACLE_URL)
  console.log(JSON.stringify(result, null, 2))

  // The decision line and the cross-check are the point of the exercise; the
  // rest of the output is noise when you are checking one deployment.
  const logs: string[] = (result.logs ?? []).map((entry: unknown) => JSON.stringify(entry))
  const decision = logs.find((l) => l.includes('W1 queue-keeper:'))
  const crossCheck = logs.find((l) => l.includes('W1 cross-check:') || l.includes('unexplained'))

  console.log('\n--- summary ---')
  console.log(decision ?? 'no decision logged — the tick aborted before deciding')
  if (crossCheck) console.log(crossCheck)
  if (logs.some((l) => l.includes('divergence=bug'))) {
    console.log('\nDIVERGENCE=BUG: the off-chain model disagrees with queueUpkeepStatus() in a way')
    console.log('the scan window does not explain. Do not bind a trigger until this is understood.')
  }
}

main().catch((error) => {
  console.error('Error running function:', error)
  process.exit(1)
})
