import { Client, CronTriggerConfig, EthersSigner } from '@mimicprotocol/sdk'
import { config } from 'dotenv'

// Fill in after `yarn mimic deploy` prints the function CID.
const FUNCTION_CID = 'YOUR_FUNCTION_CID_HERE'

// Every key here is required by manifest.yaml — a missing one fails manifest
// validation at trigger creation, not at run time.
const inputs = {
  chainId: Number(process.env.CHAIN_ID),
  amm: process.env.AMM_ADDRESS!,
  executor: process.env.QUEUE_EXECUTOR_ADDRESS!,
  controller: process.env.CONTROLLER_ADDRESS!,
  exitQueue: process.env.EXIT_QUEUE_ADDRESS!,
  smartAccount: process.env.SMART_ACCOUNT_ADDRESS!,
  maxBatches: Number(process.env.MAX_BATCHES ?? 250),
  maxRequestsPerBatch: Number(process.env.MAX_REQUESTS_PER_BATCH ?? 50),
  maxFee: process.env.MAX_FEE ?? '1',
}

async function main(): Promise<void> {
  config({ path: './scripts/.env' })

  const client = new Client({
    signer: EthersSigner.fromPrivateKey(process.env.PRIVATE_KEY!),
  })

  const manifest = await client.functions.getManifest(FUNCTION_CID)

  await client.triggers.signAndCreate({
    functionCid: FUNCTION_CID,
    manifest: manifest,
    input: inputs,
    version: '1.0.0',
    description: `EverStrat queue keeper (W1) — chain ${inputs.chainId}`,
    config: {
      type: 'cron',
      schedule: process.env.CRON_SCHEDULE ?? '*/5 * * * *',
      delta: 0,
    } as CronTriggerConfig,
    executionFeeLimit: '0',
    minValidations: 1,
  })

  console.log('Successfully created trigger')
}

main().catch((error) => {
  console.error('Error creating trigger:', error)
  process.exit(1)
})
