#!/usr/bin/env bash
#
# Regenerate pkg/envelope/testdata/fixtures.json.
#
# Fixtures are produced by `cast abi-encode` (Foundry), i.e. by an independent
# Solidity ABI encoder rather than by the Go code under test. That is the whole
# point: the round-trip test would pass against itself no matter how wrong the
# layout was, so the golden bytes have to come from the other side of the
# boundary.
#
# `abi.encode(Envelope)` is a single dynamic tuple, which is exactly what
# `cast abi-encode "f((uint64,uint64,uint64,uint8,bytes))"` emits — leading
# 0x20 offset word included.
#
# Requires: foundry (cast), jq. Run from the repo root.
set -euo pipefail

command -v cast >/dev/null || { echo "cast not found — install Foundry" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq not found" >&2; exit 1; }

OUT="pkg/envelope/testdata/fixtures.json"
mkdir -p "$(dirname "$OUT")"

SEPOLIA=16015286601757825753
MAINNET=5009297550715157269
U64_MAX=18446744073709551615

# encode_params <solidity-arg-types> <values...>
encode_params() {
  local types="$1"; shift
  [ -z "$types" ] && { echo "0x"; return; }
  cast abi-encode "f($types)" "$@"
}

# fixture <name> <chainSelector> <sequence> <observedAt> <action> <params> <note>
fixture() {
  local name="$1" sel="$2" seq="$3" obs="$4" action="$5" params="$6" note="$7"
  local report
  report="$(cast abi-encode "f((uint64,uint64,uint64,uint8,bytes))" "($sel,$seq,$obs,$action,$params)")"
  jq -n \
    --arg name "$name" --arg note "$note" \
    --arg sel "$sel" --arg seq "$seq" --arg obs "$obs" \
    --argjson action "$action" \
    --arg params "$params" --arg report "$report" \
    '{name:$name, note:$note, chainSelector:$sel, sequence:$seq, observedAt:$obs,
      action:$action, params:$params, report:$report}'
}

{
  fixture "queue_price_batch" "$SEPOLIA" 1 1700000000 1 \
    "$(encode_params "uint256" 7)" \
    "W1 PriceBatch, batchId 7"

  fixture "queue_process_requests" "$SEPOLIA" 2 1700000060 2 \
    "$(encode_params "uint256,uint256,uint256" 42 0 5)" \
    "W1 ProcessRequests, batch 42, first 5 affordable requests (endIndex exclusive)"

  fixture "queue_advance_cursor" "$SEPOLIA" 3 1700000120 3 \
    "$(encode_params "uint256" 9)" \
    "W1 AdvanceCursor to batch 9"

  fixture "strategy_rebalance" "$SEPOLIA" 1 1700000000 1 "0x" \
    "W2 Rebalance — params empty, amounts recomputed on-chain"

  fixture "strategy_provide_exit_liquidity" "$SEPOLIA" 7 1700000180 6 "0x" \
    "W2 ProvideExitLiquidity — highest StrategyAction ordinal"

  fixture "strategy_sync_mainnet" "$MAINNET" 100 1700000240 5 "0x" \
    "W2 Sync on mainnet — non-Sepolia chain selector"

  fixture "edge_max_uint64" "$U64_MAX" "$U64_MAX" "$U64_MAX" 255 \
    "$(encode_params "uint256" 1)" \
    "Boundary: every uint64 field at max, action at uint8 max"

  fixture "edge_long_params" "$SEPOLIA" 4 1700000300 2 \
    "$(encode_params "uint256,uint256,uint256" "$U64_MAX" 0 "$U64_MAX")" \
    "Boundary: uint64-max batch id and index range still fit uint256 params"
} | jq -s '.' > "$OUT"

echo "wrote $(jq 'length' "$OUT") fixtures to $OUT"
