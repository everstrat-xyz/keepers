/**
 * W1 payload encoding for `QueueKeeperExecutor.perform(uint8 action, bytes params)`.
 *
 * Params are ABI-encoded exactly as the executor decodes them:
 *
 *     PriceBatch      abi.encode(uint256 batchId)
 *     ProcessRequests abi.encode(uint256 batchId, uint256 startIndex, uint256 endIndex)  // endIndex exclusive
 *     AdvanceCursor   abi.encode(uint256 batchId)
 *
 * # No amounts
 *
 * The params surface here is closed: the only expressible values are a batch id
 * and an index range. `encode` rejects any builder call that would smuggle an
 * ETH amount, NAV, or price into a payload — and `decode` rejects any blob
 * longer than the action's exact encoding, which is what an appended amount
 * word would look like on the wire.
 */

import { Action } from "./decide";

export class UnknownActionError extends Error {
  constructor(action: number) {
    super(`queue: unknown or non-actionable QueueAction: ${action}`);
  }
}

export class EmptyRangeError extends Error {
  constructor() {
    super("queue: endIndex must be greater than startIndex");
  }
}

export class StartIndexNotZeroError extends Error {
  constructor(got: bigint) {
    super(`queue: startIndex must be 0 (executor only accepts an affordable-set prefix), got ${got}`);
  }
}

export class ParamsLengthError extends Error {
  constructor(action: Action, want: number, got: number) {
    super(`queue: ${actionString(action)} params length mismatch: want ${want} bytes, got ${got}`);
  }
}

function actionString(a: Action): string {
  return Action[a] ?? `Action(${a as number})`;
}

/** 32-byte big-endian word, as EVM ABI encoding lays out a uint256. */
function word(n: bigint): Uint8Array {
  if (n < 0n) throw new Error("queue: cannot encode a negative uint256");
  // BigInt.asUintN guards the 256-bit range the same way Solidity's uint256 does.
  const masked = BigInt.asUintN(256, n);
  const out = new Uint8Array(32);
  let v = masked;
  for (let i = 31; i >= 0; i--) {
    out[i] = Number(v & 0xffn);
    v >>= 8n;
  }
  return out;
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const len = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(len);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

/** Encodes `abi.encode(batchId)`. */
export function encodePriceBatchParams(batchId: bigint): Uint8Array {
  return word(batchId);
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
export function encodeProcessRequestsParams(batchId: bigint, endIndex: bigint): Uint8Array {
  if (endIndex === 0n) throw new EmptyRangeError();
  return concat(word(batchId), word(0n), word(endIndex));
}

/**
 * Encodes `abi.encode(batchId)`, where batchId is the cursor position the run
 * expects the executor to reach. The executor reverts if it cannot advance at
 * least that far.
 */
export function encodeAdvanceCursorParams(batchId: bigint): Uint8Array {
  return word(batchId);
}

/** The decoded, typed view of a payload. Note the absence of any value field. */
export interface Params {
  action: Action;
  batchId: bigint;
  startIndex: bigint;
  endIndex: bigint; // exclusive
}

function paramWordCount(a: Action): number {
  switch (a) {
    case Action.PriceBatch:
    case Action.AdvanceCursor:
      return 1;
    case Action.ProcessRequests:
      return 3;
    default:
      throw new UnknownActionError(a);
  }
}

export function encode(action: Action, params: Params): Uint8Array {
  switch (action) {
    case Action.PriceBatch:
      return encodePriceBatchParams(params.batchId);
    case Action.ProcessRequests:
      if (params.startIndex !== 0n) throw new StartIndexNotZeroError(params.startIndex);
      return encodeProcessRequestsParams(params.batchId, params.endIndex);
    case Action.AdvanceCursor:
      return encodeAdvanceCursorParams(params.batchId);
    default:
      throw new UnknownActionError(action);
  }
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
export function decode(action: Action, params: Uint8Array): Params {
  const want = paramWordCount(action) * 32;
  if (params.length !== want) {
    throw new ParamsLengthError(action, want, params.length);
  }

  const readWord = (i: number): bigint => {
    let v = 0n;
    for (let b = 0; b < 32; b++) v = (v << 8n) | BigInt(params[i * 32 + b]);
    return v;
  };

  const out: Params = { action, batchId: readWord(0), startIndex: 0n, endIndex: 0n };
  if (action === Action.ProcessRequests) {
    out.startIndex = readWord(1);
    out.endIndex = readWord(2);
    if (out.startIndex !== 0n) throw new StartIndexNotZeroError(out.startIndex);
    if (out.endIndex <= out.startIndex) throw new EmptyRangeError();
  }
  return out;
}
