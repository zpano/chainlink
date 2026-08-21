package v2_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	regmocks "github.com/smartcontractkit/chainlink-common/pkg/types/core/mocks"
	"github.com/smartcontractkit/chainlink-common/pkg/workflows/host"
	modulemocks "github.com/smartcontractkit/chainlink-common/pkg/workflows/wasm/host/mocks"
	billing "github.com/smartcontractkit/chainlink-protos/billing/go"
	sdkpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"
	capmocks "github.com/smartcontractkit/chainlink/v2/core/capabilities/mocks"
	metmocks "github.com/smartcontractkit/chainlink/v2/core/services/workflows/metering/mocks"
	v2 "github.com/smartcontractkit/chainlink/v2/core/services/workflows/v2"
	"github.com/smartcontractkit/chainlink/v2/core/utils/matches"
)

// TestPoC_EVMWriteReportCanSpendBeyondReservation isolates the stronger form
// of the candidate: no concurrency edge case is required.
//
// The generated EVM capability wrapper reports an empty CapabilityInfo.SpendTypes
// list. The engine therefore cannot construct a GAS spend limit and invokes
// WriteReport with an empty SpendLimits slice. WriteReport submits the on-chain
// transaction first and only reports the actual gas fee afterwards. Settle then
// records the post-facto spend even when it exceeds the amount reserved before
// execution.
func TestPoC_EVMWriteReportCanSpendBeyondReservation(t *testing.T) {
	const (
		reserved      = "1"
		actualGasCost = "2" // ETH-equivalent native units in this deterministic test
		chainSelector = uint64(12922642891491394802)
	)
	capabilityID := fmt.Sprintf("evm:ChainSelector:%d@1.0.0", chainSelector)
	gasSpendUnit := fmt.Sprintf("GAS.%d", chainSelector)

	module := modulemocks.NewModuleV2(t)
	module.EXPECT().Start()
	module.EXPECT().Close()

	capreg := regmocks.NewCapabilitiesRegistry(t)
	capreg.EXPECT().LocalNode(matches.AnyContext).Return(newNode(t), nil)

	billingClient := metmocks.NewBillingClient(t)
	billingClient.EXPECT().
		GetWorkflowExecutionRates(mock.Anything, mock.Anything).
		Return(&billing.GetWorkflowExecutionRatesResponse{
			// Keep standard WASM compute metering at zero so this test isolates GAS.
			RateCards: []*billing.RateCard{{
				ResourceType:    billing.ResourceType_RESOURCE_TYPE_COMPUTE,
				MeasurementUnit: billing.MeasurementUnit_MEASUREMENT_UNIT_MILLISECONDS,
				UnitsPerCredit:  "0",
			}},
			// One whole native token equals one credit, so a reported fee of 2
			// native tokens deterministically settles as two credits.
			GasTokensPerCredit: map[uint64]string{
				chainSelector: "1000000000000000000",
			},
		}, nil).
		Once()
	billingClient.EXPECT().
		ReserveCredits(mock.Anything, mock.Anything).
		Return(&billing.ReserveCreditsResponse{Success: true, Credits: reserved}, nil).
		Once()

	receiptCh := make(chan *billing.SubmitWorkflowReceiptRequest, 1)
	billingClient.EXPECT().
		SubmitWorkflowReceipt(mock.Anything, mock.Anything).
		Run(func(_ context.Context, req *billing.SubmitWorkflowReceiptRequest) {
			receiptCh <- req
		}).
		Return(&emptypb.Empty{}, nil).
		Once()

	initDoneCh := make(chan error, 1)
	subscribedToTriggersCh := make(chan []string, 1)
	executionFinishedCh := make(chan string, 1)
	runErrCh := make(chan error, 1)

	cfg := defaultTestConfig(t, nil)
	cfg.Module = module
	cfg.CapRegistry = capreg
	cfg.BillingClient = billingClient
	cfg.Hooks = v2.LifecycleHooks{
		OnInitialized: func(err error) { initDoneCh <- err },
		OnSubscribedToTriggers: func(triggerIDs []string) {
			subscribedToTriggersCh <- triggerIDs
		},
		OnExecutionFinished: func(executionID string, _ string) {
			executionFinishedCh <- executionID
		},
	}

	engine, err := v2.NewEngine(cfg)
	require.NoError(t, err)

	trigger := capmocks.NewTriggerCapability(t)
	eventCh := make(chan capabilities.TriggerResponse, 1)
	module.EXPECT().Execute(matches.AnyContext, mock.Anything, mock.Anything).Return(newTriggerSubs(1), nil).Once()
	capreg.EXPECT().GetTrigger(matches.AnyContext, "id_0").Return(trigger, nil).Once()
	trigger.EXPECT().RegisterTrigger(matches.AnyContext, mock.Anything).Return(eventCh, nil).Once()
	trigger.EXPECT().UnregisterTrigger(matches.AnyContext, mock.Anything).Return(nil).Once()
	trigger.EXPECT().AckEvent(matches.AnyContext, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	capability := capmocks.NewExecutableCapability(t)
	capreg.EXPECT().GetExecutable(matches.AnyContext, capabilityID).Return(capability, nil).Once()
	capreg.EXPECT().
		ConfigForCapability(mock.Anything, capabilityID, uint32(42)).
		Return(capabilities.CapabilityConfiguration{}, nil).
		Once()
	capability.EXPECT().
		Info(matches.AnyContext).
		Return(capabilities.CapabilityInfo{
			ID:  capabilityID,
			DON: &capabilities.DON{ID: 42},
			// This intentionally matches the generated EVM capability Info():
			// no SpendTypes are advertised.
			SpendTypes: nil,
		}, nil).
		Once()
	capability.EXPECT().
		Execute(matches.AnyContext, mock.Anything).
		Run(func(_ context.Context, req capabilities.CapabilityRequest) {
			require.Equal(t, "WriteReport", req.Method)
			require.Empty(t, req.Metadata.SpendLimits,
				"EVM WriteReport is invoked without a pre-execution GAS spend limit")
		}).
		Return(capabilities.CapabilityResponse{
			Metadata: capabilities.ResponseMetadata{
				Metering: []capabilities.MeteringNodeDetail{{
					Peer2PeerID: "local",
					SpendUnit:   gasSpendUnit,
					SpendValue:  actualGasCost,
				}},
			},
		}, nil).
		Once()

	module.EXPECT().
		Execute(matches.AnyContext, mock.Anything, mock.Anything).
		Run(func(ctx context.Context, _ *sdkpb.ExecuteRequest, executor host.ExecutionHelper) {
			_, callErr := executor.CallCapability(ctx, &sdkpb.CapabilityRequest{
				Id:         capabilityID,
				Method:     "WriteReport",
				CallbackId: 1,
			})
			runErrCh <- callErr
		}).
		Return(nil, nil).
		Once()

	require.NoError(t, engine.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	require.NoError(t, <-initDoneCh)
	require.Equal(t, []string{"id_0"}, <-subscribedToTriggersCh)

	eventCh <- capabilities.TriggerResponse{Event: capabilities.TriggerEvent{
		TriggerType: "basic-trigger@1.0.0",
		ID:          "evm_write_report_overspend_poc",
	}}

	require.NoError(t, <-runErrCh)
	select {
	case <-executionFinishedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for workflow execution")
	}

	select {
	case receipt := <-receiptCh:
		consumed, ok := new(big.Rat).SetString(receipt.CreditsConsumed)
		require.True(t, ok, "invalid consumed credit amount %q", receipt.CreditsConsumed)
		reservedAmount, ok := new(big.Rat).SetString(reserved)
		require.True(t, ok, "invalid reserved credit amount %q", reserved)
		require.Greater(t, consumed.Cmp(reservedAmount), 0,
			"post-facto GAS spend %s must exceed the %s-credit reservation",
			receipt.CreditsConsumed, reserved)
		require.Equal(t, actualGasCost, receipt.CreditsConsumed)
		t.Logf("EVM WriteReport overspend reproduced: reserved=%s consumed=%s", reserved, receipt.CreditsConsumed)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for metering receipt")
	}
}
