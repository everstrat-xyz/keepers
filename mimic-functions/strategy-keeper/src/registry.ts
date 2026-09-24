import { Address, Bytes, ChainId, Result } from '@mimicprotocol/lib-ts'

import { IRegistry } from './types/IRegistry'
import { StrategyKeeperExecutor } from './types/StrategyKeeperExecutor'

// Auth.* registry keys, keccak256 of the key name. The registry has no named
// getters; the executor resolves every collaborator through these, so W2 must
// resolve the same contracts the same way.
export const STRATEGY_MANAGER_KEY = '0x1893e1a169e79f2fe8aa327b1bceb2fede7a1b76a54824f95ea0e737720954ae'
export const EXIT_QUEUE_KEY = '0x6a7c10ecf5ed4662e5ef8392907aa359123001b89182291fde3f91408f34221f'
export const QUEUE_KEEPER_EXECUTOR_KEY = '0x66854862635421a5d930a231dd533764fb30f528b7f7dd0feb1d93fb2e4e25d2'

/** The executor's registry, as the executor itself reads it. */
export function executorRegistry(executor: StrategyKeeperExecutor, chainId: ChainId): Result<IRegistry, string> {
  const registryResult = executor.registry()
  if (registryResult.isError) return Result.err<IRegistry, string>('registry(): ' + registryResult.error)
  return Result.ok<IRegistry, string>(new IRegistry(registryResult.unwrap(), chainId))
}

export function resolveKey(registry: IRegistry, key: string, name: string): Result<Address, string> {
  const result = registry.getContractByKey(Bytes.fromHexString(key))
  if (result.isError) return Result.err<Address, string>('getContractByKey(' + name + '): ' + result.error)
  return Result.ok<Address, string>(result.unwrap())
}
