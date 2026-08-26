/**
 * Create the W2 cron trigger from `scripts/.env`.
 *
 * Requires SMART_ACCOUNT_ADDRESS: look it up in the Mimic Protocol App
 * (this chain) *before* creating the trigger. Pass that same address to
 * `StrategyKeeperExecutor.allowExecutorCaller()` or `perform()` reverts
 * `KeeperExecutorNoAllowedCallers`. Dry-run / prefill may use `0x0`; a live
 * trigger must not.
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
  console.log('Next: ADMIN calls StrategyKeeperExecutor.allowExecutorCaller() with')
  console.log('the same SMART_ACCOUNT_ADDRESS that is in this trigger — until then perform() reverts.')
}

main().catch((error) => {
  console.error('Error creating trigger:', error)
  process.exit(1)
})
