/**
 * W1 payload encoding for `QueueKeeperExecutor.perform(uint8 action, bytes params)`.
 *
 * Params are ABI-encoded exactly as the executor decodes them:
 *
 *     PriceBatch      abi.encode(uint256 batchId)
 *     ProcessRequests abi.encode(uint256 batchId, uint256 startIndex, uint256 endIndex)  // endIndex exclusive
 *     AdvanceCursor   abi.encode(batchId)
 *
 * # No amounts
 *
 * The params surface here is closed: the only expressible values are a batch id
 * and an index range. `encode` rejects any builder call that would smuggle an
 * ETH amount, NAV, or price into a payload — and `decode` rejects any blob
 * longer than the action's exact encoding, which is what an appended amount
 * word would look like on the wire.
 */

import { BigInt } from '@mimicprotocol/lib-ts'

import { ActionAdvanceCursor, ActionPriceBatch, ActionProcessRequests, actionString } from './decide'

function requireKnownAction(a: u8): void {
  if (a != ActionPriceBatch && a != ActionProcessRequests && a != ActionAdvanceCursor) {
    const msg: string = 'queue: unknown or non-actionable QueueAction: '
    throw new Error(msg.concat(u8(a).toString()))
  }
}

function requireStartIndexZero(startIndex: BigInt): void {
  if (startIndex != BigInt.zero()) {
    const msg: string = 'queue: startIndex must be 0 (executor only accepts an affordable-set prefix), got '
    throw new Error(msg.concat(startIndex.toString()))
  }
}

function requireNonEmptyRange(startIndex: BigInt, endIndex: BigInt): void {
  if (endIndex <= startIndex) {
    throw new Error('queue: endIndex must be greater than startIndex')
  }
}

/** 32-byte big-endian word, as EVM ABI encoding lays out a uint256. */
function word(n: BigInt): Uint8Array {
  if (n < BigInt.zero()) throw new Error('queue: cannot encode a negative uint256')
  // lib-ts's pow overflows past ~2^170, so build 2^256 as (2^128)^2.
  const p128 = BigInt.fromI32(2).pow(128 as u8)
  const two256 = p128.times(p128)
  if (n >= two256) throw new Error('queue: value does not fit in a uint256')
  let v = n
  const out = new Uint8Array(32)
  const ff = BigInt.fromI32(0xff)
  const base = BigInt.fromI32(256)
  for (let i = 31; i >= 0; i--) {
    out[i] = <u8>((v % base) & ff).toI32()
    v = v.div(base)
  }
  return out
}

function concat3(a: Uint8Array, b: Uint8Array, c: Uint8Array): Uint8Array {
  const out = new Uint8Array(a.length + b.length + c.length)
  let off = 0
  out.set(a, off)
  off += a.length
  out.set(b, off)
  off += b.length
  out.set(c, off)
  return out
}

/** Encodes `abi.encode(batchId)`. */
export function encodePriceBatchParams(batchId: BigInt): Uint8Array {
  return word(batchId)
}

/**
 * Encodes `abi.encode(batchId, startIndex, endIndex)` with endIndex exclusive,
 * matching `Controller.processRequests`.
 *
 * startIndex is fixed at 0 rather than taken as an argument: the executor
 * re-derives the affordable set from live state and reverts unless the claimed
 * range starts at 0. A run may claim a shorter prefix (endIndex below the
 * affordable count) but never an offset one.
 */
export function encodeProcessRequestsParams(batchId: BigInt, endIndex: BigInt): Uint8Array {
  requireNonEmptyRange(BigInt.zero(), endIndex)
  const zero = word(BigInt.zero())
  return concat3(word(batchId), zero, word(endIndex))
}

/**
 * Encodes `abi.encode(batchId)`, where batchId is the cursor position the run
 * expects the executor to reach. The executor reverts if it cannot advance at
 * least that far.
 */
export function encodeAdvanceCursorParams(batchId: BigInt): Uint8Array {
  return word(batchId)
}

/** The decoded, typed view of a payload. Note the absence of any value field. */
export class Params {
  action: u8
  batchId: BigInt
  startIndex: BigInt
  endIndex: BigInt // exclusive

  constructor(action: u8, batchId: BigInt, startIndex: BigInt, endIndex: BigInt) {
    this.action = action
    this.batchId = batchId
    this.startIndex = startIndex
    this.endIndex = endIndex
  }
}

function paramWordCount(a: u8): i32 {
  if (a == ActionPriceBatch || a == ActionAdvanceCursor) return 1
  if (a == ActionProcessRequests) return 3
  requireKnownAction(a)
  return 0
}

export function encode(action: u8, params: Params): Uint8Array {
  if (action == ActionPriceBatch) {
    return encodePriceBatchParams(params.batchId)
  }
  if (action == ActionProcessRequests) {
    requireStartIndexZero(params.startIndex)
    return encodeProcessRequestsParams(params.batchId, params.endIndex)
  }
  if (action == ActionAdvanceCursor) {
    return encodeAdvanceCursorParams(params.batchId)
  }
  requireKnownAction(action)
  return new Uint8Array(0)
}

/**
 * Parses params for an action and enforces the exact wire length.
 *
 * The length check is the "no smuggled amounts" guard: all three layouts are
 * static, so a correct blob is exactly 32 bytes per field. An appended amount
 * word — the mistake this module exists to make impossible — changes the
 * length and is rejected here rather than being silently ignored by
 * Solidity's abi.decode.
 */
export function decode(action: u8, params: Uint8Array): Params {
  const want = paramWordCount(action) * 32
  if (params.length != want) {
    const msg: string = 'queue: '
      .concat(actionString(action))
      .concat(' params length mismatch: want ')
      .concat(want.toString())
      .concat(' bytes, got ')
      .concat(params.length.toString())
    throw new Error(msg)
  }

  const readWord = (i: i32): BigInt => {
    let v = BigInt.zero()
    for (let b = 0; b < 32; b++) {
      v = v.times(BigInt.fromI32(256)).plus(BigInt.fromU8(params[i * 32 + b]))
    }
    return v
  }

  const out = new Params(action, readWord(0), BigInt.zero(), BigInt.zero())
  if (action == ActionProcessRequests) {
    out.startIndex = readWord(1)
    out.endIndex = readWord(2)
    requireStartIndexZero(out.startIndex)
    requireNonEmptyRange(out.startIndex, out.endIndex)
  }
  return out
}
