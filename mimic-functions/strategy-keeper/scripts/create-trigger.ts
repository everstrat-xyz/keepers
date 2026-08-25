import { Client, CronTriggerConfig, EthersSigner } from '@mimicprotocol/sdk'
import { config } from 'dotenv'

// Fill in after `yarn mimic deploy` prints the function CID.
const FUNCTION_CID = 'YOUR_FUNCTION_CID_HERE'

const inputs = {
  chainId: Number(process.env.CHAIN_ID),
  executor: process.env.STRATEGY_EXECUTOR_ADDRESS!,
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
    description: `EverStrat strategy keeper (W2) — chain ${inputs.chainId}`,
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
