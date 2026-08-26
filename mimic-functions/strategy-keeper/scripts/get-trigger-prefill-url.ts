/**
 * Print a Mimic Protocol App URL with the W2 trigger form pre-filled.
 *
 * Useful when the trigger is created by hand in the app (the runbook's default)
 * rather than by `create-trigger.ts`: the inputs come from the same
 * `scripts/.env`, so the form cannot disagree with the manifest.
 *
 * `smartAccount` is left as the zero address on purpose — it is assigned when
 * the trigger is created. Bind the real one afterwards with
 * `StrategyKeeperExecutor.allowExecutorCaller()`, per docs/MIMIC_CUTOVER.md §1.4.
 */
import { getTriggerPrefillUrl } from '@mimicprotocol/sdk'

import { cronConfig, endDateNotice, inputs, required } from './inputs.js'

async function main(): Promise<void> {
  const functionInputs = inputs()

  const prefillUrl = getTriggerPrefillUrl({
    functionCid: required('FUNCTION_CID'),
    input: functionInputs,
    config: cronConfig(),
    description: `EverStrat strategy keeper (W2) — chain ${functionInputs.chainId}`,
    version: '1.0.0',
  })

  console.log(`Trigger prefill URL: ${prefillUrl}`)
  console.log(endDateNotice())
}

main().catch((error) => {
  console.error('Error getting trigger prefill URL:', error)
  process.exit(1)
})
