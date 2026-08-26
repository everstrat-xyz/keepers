/**
 * Create the W2 cron trigger from `scripts/.env`.
 *
 * Requires SMART_ACCOUNT_ADDRESS: unlike the dry-run scripts, this is the call
 * that binds a real signer, and that same address is what ADMIN must pass to
 * `StrategyKeeperExecutor.allowExecutorCaller()` before `perform()` stops
 * reverting `KeeperExecutorNoAllowedCallers`.
 */
import { Client, EthersSigner } from '@mimicprotocol/sdk'

import { cronConfig, endDateNotice, inputs, required } from './inputs.js'

async function main(): Promise<void> {
  const functionInputs = inputs(true)
  const functionCid = required('FUNCTION_CID')

  const client = new Client({
    signer: EthersSigner.fromPrivateKey(required('PRIVATE_KEY')),
  })

  const manifest = await client.functions.getManifest(functionCid)

  await client.triggers.signAndCreate({
    functionCid,
    manifest: manifest,
    input: functionInputs,
    version: '1.0.0',
    description: `EverStrat strategy keeper (W2) — chain ${functionInputs.chainId}`,
    config: cronConfig(),
    executionFeeLimit: '0',
    minValidations: 1,
  })

  console.log('Successfully created trigger')
  console.log(endDateNotice())
  console.log('Next: read the assigned smart account from the task page, then ADMIN calls')
  console.log('StrategyKeeperExecutor.allowExecutorCaller(<smart account>) — until then perform() reverts.')
}

main().catch((error) => {
  console.error('Error creating trigger:', error)
  process.exit(1)
})
