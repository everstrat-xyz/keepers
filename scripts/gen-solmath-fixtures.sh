#!/usr/bin/env bash
#
# Regenerate pkg/solmath/testdata/fixtures.json.
#
# Reference values are evaluated by `chisel` — Foundry's Solidity REPL — so the
# expected results come from a real Solidity uint256 evaluator rather than from
# the Go code under test or from hand arithmetic. Truncating division and the
# strict `<` in isRelativelyLessThan are exactly the details a hand-computed
# fixture gets wrong.
#
# The expressions below are transcribed from src/libraries/Math.sol:
#   convertAssets(a, p)              = (a * p) / 1e18
#   isRelativelyLessThan(a, b, diff) = a * 1e18 < b * (1e18 - diff)
#
# Requires: foundry (chisel), jq. Run from the repo root.
set -euo pipefail

command -v chisel >/dev/null || { echo "chisel not found — install Foundry" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq not found" >&2; exit 1; }

OUT="pkg/solmath/testdata/fixtures.json"
mkdir -p "$(dirname "$OUT")"

# eval_sol <expression> -> decimal value (or true/false for bools)
eval_sol() {
  chisel eval "$1" | awk -F': ' '/Decimal:/ {print $2; found=1} END {if (!found) exit 1}' | tr -d ' '
}

eval_bool() {
  # chisel renders a bool as a "Value: true|false" line, with no Decimal line.
  chisel eval "$1" | awk '/Value:/ {print $NF; found=1} END {if (!found) exit 1}'
}

# Every operand is wrapped in uint256(). Without it Solidity constant-folds the
# literal expression with *rational* arithmetic, so truncating cases either fail
# to compile or silently return the exact value — which is precisely the
# behaviour these fixtures exist to distinguish from EVM integer division.
u() { printf 'uint256(%s)' "$1"; }

convert_case() {
  local amount="$1" price="$2"
  local got
  got="$(eval_sol "$(u "$amount") * $(u "$price") / $(u 1e18)")"
  jq -n --arg amount "$amount" --arg price "$price" --arg want "$got" \
    '{amount:$amount, price:$price, want:$want}'
}

relative_case() {
  local a="$1" b="$2" diff="$3"
  local got
  got="$(eval_bool "$(u "$a") * $(u 1e18) < $(u "$b") * ($(u 1e18) - $(u "$diff"))")"
  jq -n --arg a "$a" --arg b "$b" --arg difference "$diff" --argjson want "$got" \
    '{a:$a, b:$b, difference:$difference, want:$want}'
}

CONVERT="$( {
  convert_case "1000000000000000000" "1000000000000000000"   # 1 EVE at 1 ETH
  convert_case "7000000000000000000" "123456789012345678"    # truncating
  convert_case "1" "1"                                       # truncates to 0
  convert_case "999999999999999999" "999999999999999999"     # off-by-one wei
  convert_case "0" "1000000000000000000"
  convert_case "1000000000000000000" "0"
  convert_case "123456789012345678901234567890" "1500000000000000000"
} | jq -s '.' )"

# difference is a fraction of 1e18: 0 = exact, 5e16 = 5%, 1e18 = 100%.
RELATIVE="$( {
  relative_case "1000000000000000000" "1000000000000000000" "0"                    # equal, diff 0
  relative_case "999999999999999999" "1000000000000000000" "0"                     # one wei below
  relative_case "950000000000000000" "1000000000000000000" "50000000000000000"     # exactly at 5% tolerance
  relative_case "949999999999999999" "1000000000000000000" "50000000000000000"     # one wei past 5%
  relative_case "1100000000000000000" "1000000000000000000" "50000000000000000"    # price rose
  relative_case "0" "1000000000000000000" "1000000000000000000"                    # 100% tolerance
  relative_case "0" "0" "0"                                                        # both zero
  relative_case "1" "1000000000000000000" "999999999999999999"                     # extreme tolerance
} | jq -s '.' )"

jq -n --argjson convertAssets "$CONVERT" --argjson isRelativelyLessThan "$RELATIVE" \
  '{convertAssets:$convertAssets, isRelativelyLessThan:$isRelativelyLessThan}' > "$OUT"

echo "wrote $(jq '.convertAssets | length' "$OUT") convertAssets and $(jq '.isRelativelyLessThan | length' "$OUT") isRelativelyLessThan cases to $OUT"
