package v2_test

import (
	"context"
	"fmt"
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

// TestPoC_EVMWriteReportOverspendAmplifiesToMaxTargets shows that the released
// per-workflow ChainWrite.TargetsLimit does not bound spending by the reserved
// credits. A workflow may use all ten allowed WriteReport calls. Because EVM
// advertises no GAS SpendTypes, each call reaches the capability with an empty
// SpendLimits list and settles its irreversible fee only after execution.
func TestPoC_EVMWriteReportOverspendAmplifiesToMaxTargets(t *testing.T) {
	const (
		callCount     = 10 // released PerWorkflow.ChainWrite.TargetsLimit
		reserved      = "1"
		actualGasCost = "2"
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
			RateCards: []*billing.RateCard{{
				ResourceType:    billing.ResourceType_RESOURCE_TYPE_COMPUTE,
				MeasurementUnit: billing.MeasurementUnit_MEASUREMENT_UNIT_MILLISECONDS,
				UnitsPerCredit:  "0",
			}},
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
	subscribedCh := make(chan []string, 1)
	finishedCh := make(chan string, 1)
	runErrCh := make(chan error, 1)

	cfg := defaultTestConfig(t, nil)
	cfg.Module = module
	cfg.CapRegistry = capreg
	cfg.BillingClient = billingClient
	cfg.Hooks = v2.LifecycleHooks{
		OnInitialized: func(err error) { initDoneCh <- err },
		OnSubscribedToTriggers: func(ids []string) { subscribedCh <- ids },
		OnExecutionFinished: func(id string, _ string) { finishedCh <- id },
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
	capreg.EXPECT().GetExecutable(matches.AnyContext, capabilityID).Return(capability, nil).Times(callCount)
	capreg.EXPECT().
		ConfigForCapability(mock.Anything, capabilityID, uint32(42)).
		Return(capabilities.CapabilityConfiguration{}, nil).
		Times(callCount)
	capability.EXPECT().
		Info(matches.AnyContext).
		Return(capabilities.CapabilityInfo{
			ID:         capabilityID,
			DON:        &capabilities.DON{ID: 42},
			SpendTypes: nil,
		}, nil).
		Times(callCount)
	capability.EXPECT().
		Execute(matches.AnyContext, mock.Anything).
		Run(func(_ context.Context, req capabilities.CapabilityRequest) {
			require.Equal(t, "WriteReport", req.Method)
			require.Empty(t, req.Metadata.SpendLimits)
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
		Times(callCount)

	module.EXPECT().
		Execute(matches.AnyContext, mock.Anything, mock.Anything).
		Run(func(ctx context.Context, _ *sdkpb.ExecuteRequest, executor host.ExecutionHelper) {
			for i := 1; i <= callCount; i++ {
				_, callErr := executor.CallCapability(ctx, &sdkpb.CapabilityRequest{
					Id:         capabilityID,
					Method:     "WriteReport",
					CallbackId: int32(i),
				})
				if callErr != nil {
					runErrCh <- fmt.Errorf("WriteReport %d failed: %w", i, callErr)
					return
				}
			}
			runErrCh <- nil
		}).
		Return(nil, nil).
		Once()

	require.NoError(t, engine.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	require.NoError(t, <-initDoneCh)
	require.Equal(t, []string{"id_0"}, <-subscribedCh)

	eventCh <- capabilities.TriggerResponse{Event: capabilities.TriggerEvent{
		TriggerType: "basic-trigger@1.0.0",
		ID:          "evm_write_report_max_targets_poc",
	}}
	require.NoError(t, <-runErrCh)

	select {
	case <-finishedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for workflow execution")
	}

	select {
	case receipt := <-receiptCh:
		require.Equal(t, "20", receipt.CreditsConsumed)
		t.Logf("max-target overspend reproduced: reserved=%s consumed=%s calls=%d", reserved, receipt.CreditsConsumed, callCount)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for metering receipt")
	}
}
