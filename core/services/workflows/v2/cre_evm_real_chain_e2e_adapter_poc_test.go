//go:build auditcre

package v2_test

func (*e2eEVMClientCapability) ChainSelector() uint64 {
	return e2eChainSelector
}

func (*e2eEVMClientCapability) Description() string {
	return "engine-to-real-EVM billing overspend validation adapter"
}
