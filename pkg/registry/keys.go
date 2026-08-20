// Package registry holds the canonical EverStrat Registry keys and role
// identifiers from the contracts' `Auth` library.
//
// The Registry is the single configured address a workflow needs: everything
// else (Controller, ExitQueue, AMM, StrategyManager) is resolved at runtime
// with `IRegistry.getContractByKey(bytes32)`. Resolving rather than configuring
// means a protocol redeploy that re-registers a contract does not silently
// leave the keepers pointed at a dead address.
//
// Keys are `keccak256(<name>)` and are hardcoded here so the WASM workflows do
// not carry a Keccak implementation just to derive ten constants. TestKeys
// recomputes every value from its source string and fails on any drift.
package registry

import "github.com/ethereum/go-ethereum/common"

// Contract keys — `Auth.CONTROLLER` and friends.
var (
	KeyController             = common.HexToHash("0x70546d1c92f8c2132ae23a23f5177aa8526356051c7510df99f50e012d221529")
	KeyAMM                    = common.HexToHash("0xedda99bbc2e81e192db34603b20c3ca6ffc475ec37193683bbf3d66d78db8a7c")
	KeyStrategyManager        = common.HexToHash("0x1893e1a169e79f2fe8aa327b1bceb2fede7a1b76a54824f95ea0e737720954ae")
	KeyExitQueue              = common.HexToHash("0x6a7c10ecf5ed4662e5ef8392907aa359123001b89182291fde3f91408f34221f")
	KeyOracle                 = common.HexToHash("0x352d05fe3946dbe49277552ba941e744d5a96d9c60bc1ba0ea5f1d3ae000f7c8")
	KeyEVE                    = common.HexToHash("0xaf94fe894bf0e22494392493fc7eb18a0ab98754fe785e74fd233f476b9c37c9")
	KeyConverter              = common.HexToHash("0x9b67f1cdf2c9ad23209b11e73d7a46eeae44f0340c10d14788faa1ace66c0898")
	KeyQueueKeeperExecutor    = common.HexToHash("0x66854862635421a5d930a231dd533764fb30f528b7f7dd0feb1d93fb2e4e25d2")
	KeyStrategyKeeperExecutor = common.HexToHash("0xf047ee91387866f3af2e0a5de8623ab6b2ac0a5898ed2f08abd4abb03324a85e")
	KeyWhitelist              = common.HexToHash("0x0af0c3ebe77999ca20698e1ff25f812bf82409a59d21ca15a41f39e0ce9f2500")
)

// Role identifiers — `Auth.ADMIN_ROLE` and friends.
//
// Workflows never hold a role themselves; the receiver contract does. These are
// here for the cutover checklist and for W4's freeze-watch assertions, e.g.
// confirming the manual multisig still holds KEEPER_ROLE as break-glass.
var (
	RoleAdmin    = common.HexToHash("0xa49807205ce4d355092ef5a8a18f56e8913cf4a201fbe287825b095693c21775")
	RoleSecurity = common.HexToHash("0x4698baa05b306e3e5e3fa66d29891e203a1418ef5bee962e2c9b109f129e8920")
	RoleKeeper   = common.HexToHash("0xfc8737ab85eb45125971625a9ebdb75cc78e01d5c1fa80c4c6e5203f47bc4fab")
)

// Preimages maps each identifier to the string it hashes, so tests can verify
// the constants and logs can render a readable name.
var Preimages = map[common.Hash]string{
	KeyController:             "CONTROLLER",
	KeyAMM:                    "AMM",
	KeyStrategyManager:        "STRATEGY_MANAGER",
	KeyExitQueue:              "EXIT_QUEUE",
	KeyOracle:                 "ORACLE",
	KeyEVE:                    "EVE",
	KeyConverter:              "CONVERTER",
	KeyQueueKeeperExecutor:    "QUEUE_KEEPER_EXECUTOR",
	KeyStrategyKeeperExecutor: "STRATEGY_KEEPER_EXECUTOR",
	KeyWhitelist:              "WHITELIST",

	RoleAdmin:    "ADMIN_ROLE",
	RoleSecurity: "SECURITY_ROLE",
	RoleKeeper:   "KEEPER_ROLE",
}

// Name renders a key or role for logs, falling back to hex for anything
// unknown.
func Name(h common.Hash) string {
	if s, ok := Preimages[h]; ok {
		return s
	}
	return h.Hex()
}
