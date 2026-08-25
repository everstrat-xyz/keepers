import {
  EmptyRangeError,
  ParamsLengthError,
  StartIndexNotZeroError,
  UnknownActionError,
  decode,
  encode,
  encodeAdvanceCursorParams,
  encodePriceBatchParams,
  encodeProcessRequestsParams,
} from "./params";
import { Action, actionString } from "./decide";

const WORD = (n: bigint | number) =>
  BigInt(n).toString(16).padStart(64, "0");

const toHex = (b: Uint8Array) =>
  "0x" + Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");

const fromHex = (s: string) =>
  Uint8Array.from(s.replace(/^0x/, "").match(/.{2}/g)!.map((h) => parseInt(h, 16)));

// Ordinals must match IQueueKeeperExecutor.QueueAction — reordering the
// Solidity enum without updating these would silently retarget every payload.
describe("action ordinals", () => {
  it("matches the Solidity enum", () => {
    expect(Action.None).toBe(0);
    expect(Action.PriceBatch).toBe(1);
    expect(Action.ProcessRequests).toBe(2);
    expect(Action.AdvanceCursor).toBe(3);
  });

  it("names every action", () => {
    expect(actionString(Action.None)).toBe("None");
    expect(actionString(Action.PriceBatch)).toBe("PriceBatch");
    expect(actionString(Action.ProcessRequests)).toBe("ProcessRequests");
    expect(actionString(Action.AdvanceCursor)).toBe("AdvanceCursor");
    expect(actionString(4 as Action)).toBe("Action(4)");
  });
});

describe("encode", () => {
  // Golden bytes from `cast abi-encode "f(uint256)" 7` and
  // `cast abi-encode "f(uint256,uint256,uint256)" 42 0 5`.
  it("PriceBatch matches Solidity", () => {
    expect(toHex(encodePriceBatchParams(7n))).toBe("0x" + WORD(7));
  });

  it("AdvanceCursor matches Solidity", () => {
    expect(toHex(encodeAdvanceCursorParams(9n))).toBe("0x" + WORD(9));
  });

  it("ProcessRequests matches Solidity", () => {
    expect(toHex(encodeProcessRequestsParams(42n, 5n))).toBe(
      "0x" + WORD(42) + WORD(0) + WORD(5),
    );
  });

  it("rejects an empty ProcessRequests range", () => {
    expect(() => encodeProcessRequestsParams(1n, 0n)).toThrow(EmptyRangeError);
  });

  it("rejects a non-zero startIndex", () => {
    expect(() =>
      encode(Action.ProcessRequests, {
        action: Action.ProcessRequests,
        batchId: 42n,
        startIndex: 1n,
        endIndex: 5n,
      }),
    ).toThrow(StartIndexNotZeroError);
  });

  it("rejects a non-actionable action", () => {
    expect(() =>
      encode(Action.None as Action, { action: Action.None, batchId: 1n, startIndex: 0n, endIndex: 0n }),
    ).toThrow(UnknownActionError);
  });

  it("rejects a negative batch id", () => {
    expect(() => encodePriceBatchParams(-1n)).toThrow(/negative/);
  });
});

describe("decode", () => {
  it("round-trips PriceBatch", () => {
    const p = decode(Action.PriceBatch, encodePriceBatchParams(7n));
    expect(p.batchId).toBe(7n);
  });

  it("round-trips ProcessRequests", () => {
    const p = decode(Action.ProcessRequests, encodeProcessRequestsParams(42n, 5n));
    expect(p).toMatchObject({ action: Action.ProcessRequests, batchId: 42n, startIndex: 0n, endIndex: 5n });
  });

  it("round-trips AdvanceCursor", () => {
    const p = decode(Action.AdvanceCursor, encodeAdvanceCursorParams(9n));
    expect(p.batchId).toBe(9n);
  });

  // The hard-constraint test: a payload must never carry an authoritative ETH
  // amount. The layouts are static, so an amount can only ride along as an
  // extra word — which Solidity's abi.decode would silently ignore and which
  // the length check rejects.
  it.each([
    ["appended to PriceBatch", Action.PriceBatch, WORD(7)],
    ["appended to AdvanceCursor", Action.AdvanceCursor, WORD(9)],
    [
      "appended to ProcessRequests",
      Action.ProcessRequests,
      WORD(42) + WORD(0) + WORD(5),
    ],
    ["prepended to PriceBatch, shifting every field", Action.PriceBatch, null],
  ])("rejects a smuggled amount %s", (_name, action, suffix) => {
    const ONE_ETH = WORD(10n ** 18n);
    const blob =
      suffix === null
        ? ONE_ETH + WORD(7) // prepended
        : suffix + ONE_ETH; // appended
    expect(() => decode(action, fromHex(blob))).toThrow(ParamsLengthError);
  });

  it("rejects a truncated blob", () => {
    const valid = encodePriceBatchParams(7n);
    expect(() => decode(Action.PriceBatch, valid.slice(0, 16))).toThrow(ParamsLengthError);
  });

  it("rejects an empty blob", () => {
    expect(() => decode(Action.PriceBatch, new Uint8Array(0))).toThrow(ParamsLengthError);
  });

  it("rejects a non-actionable action", () => {
    expect(() => decode(Action.None, encodePriceBatchParams(7n))).toThrow(UnknownActionError);
  });

  it("rejects a non-zero startIndex on the wire", () => {
    expect(() =>
      decode(Action.ProcessRequests, fromHex(WORD(42) + WORD(1) + WORD(5))),
    ).toThrow(StartIndexNotZeroError);
  });
});

// Whatever decide proposes must survive encode — the builder and the wire
// layout cannot be allowed to drift.
describe("decide output encodes", () => {
  it("every action's decision params are exactly encodable", () => {
    const cases: Array<[Action, bigint, bigint]> = [
      [Action.PriceBatch, 3n, 0n],
      [Action.ProcessRequests, 3n, 2n],
      [Action.AdvanceCursor, 9n, 0n],
    ];
    for (const [action, batchId, endIndex] of cases) {
      const params = encode(action, { action, batchId, startIndex: 0n, endIndex });
      expect(decode(action, params)).toMatchObject({ batchId, endIndex });
    }
  });
});
