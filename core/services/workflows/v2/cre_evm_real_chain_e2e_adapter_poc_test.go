//go:build auditcre

package v2_test

import (
	"context"

	"github.com/smartcontractkit/chainlink-common/pkg/types/core"
)

func (*e2eEVMClientCapability) ChainSelector() uint64 {
	return e2eChainSelector
}

func (*e2eEVMClientCapability) Description() string {
	return "engine-to-real-EVM billing overspend validation adapter"
}

// The production EVM plugin implements these lifecycle methods on its outer
// capabilityGRPCService. The audit adapter embeds the real actions.EVM and adds
// only the outer service surface required by the generated ClientServer, so the
// generated wrapper remains in the execution path without booting an unrelated
// LOOP process or LogTrigger service.
func (*e2eEVMClientCapability) Start(context.Context) error {
	return nil
}

func (*e2eEVMClientCapability) HealthReport() map[string]error {
	return map[string]error{"audit-e2e-evm": nil}
}

func (*e2eEVMClientCapability) Name() string {
	return "audit-e2e-evm"
}

func (*e2eEVMClientCapability) Ready() error {
	return nil
}

func (*e2eEVMClientCapability) Initialise(context.Context, core.StandardCapabilitiesDependencies) error {
	return nil
}
