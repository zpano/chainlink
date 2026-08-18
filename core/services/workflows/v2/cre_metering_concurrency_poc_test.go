package v2_test

import (
	"context"
	"fmt"
	"math/big"
	"sync"
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

// TestPoC_MeteringFailsOpenAtSharedCapabilityConcurrencyLimit reproduces the
// production wiring where the WASM host and ExecutionHelper share the same
// CapabilityConcurrency limiter. The host keeps one slot for every pending
// async call, and ExecutionHelper takes a second slot for the same call.
//
// With the released default limit of 30, 15 pending calls fill the pool. The
// fifteenth inner call therefore observes Available()==0. Deduct returns
// ErrNoOpenCalls, but ExecutionHelper only logs that error and invokes the
// capability with an empty SpendLimits slice.
//
// The test reserves 14 credits and makes every capability consume one credit.
// All 15 capability calls succeed, and the submitted receipt reports more than
// the 14 credits that were reserved. Resource consumption has already happened
// before this post-facto receipt is submitted.
func TestPoC_MeteringFailsOpenAtSharedCapabilityConcurrencyLimit(t *testing.T) {
	const (
		callCount    = 15
		reserved     = "14"
		capabilityID = "metered-capability"
		spendUnit    = "RESOURCE_TYPE_COMPUTE"
	)

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
				MeasurementUnit: billing.MeasurementUnit_MEASUREMENT_UNIT_COST,
				UnitsPerCredit:  "1",
			}},
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
		OnInitialized: func(err error) {
			initDoneCh <- err
		},
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
	capreg.EXPECT().GetExecutable(matches.AnyContext, capabilityID).Return(capability, nil).Times(callCount)
	capreg.EXPECT().
		ConfigForCapability(mock.Anything, mock.Anything, mock.Anything).
		Return(capabilities.CapabilityConfiguration{}, nil).
		Times(callCount)
	capability.EXPECT().
		Info(matches.AnyContext).
		Return(capabilities.CapabilityInfo{
			ID:  capabilityID,
			DON: &capabilities.DON{ID: 42},
			SpendTypes: []capabilities.CapabilitySpendType{
				capabilities.CapabilitySpendType(spendUnit),
			},
		}, nil).
		Times(callCount)

	enteredCh := make(chan []capabilities.SpendLimit, callCount)
	releaseCh := make(chan struct{})
	capability.EXPECT().
		Execute(matches.AnyContext, mock.Anything).
		Run(func(_ context.Context, req capabilities.CapabilityRequest) {
			limitsCopy := append([]capabilities.SpendLimit(nil), req.Metadata.SpendLimits...)
			enteredCh <- limitsCopy
			<-releaseCh
		}).
		Return(capabilities.CapabilityResponse{
			Metadata: capabilities.ResponseMetadata{
				Metering: []capabilities.MeteringNodeDetail{{
					Peer2PeerID: "local",
					SpendUnit:   spendUnit,
					SpendValue:  "1",
				}},
			},
		}, nil).
		Times(callCount)

	module.EXPECT().
		Execute(matches.AnyContext, mock.Anything, mock.Anything).
		Run(func(ctx context.Context, _ *sdkpb.ExecuteRequest, executor host.ExecutionHelper) {
			var runErr error
			setRunErr := func(err error) {
				if runErr == nil {
					runErr = err
				}
			}

			// Reproduce the first acquisition made by host.callCapAsync. Production
			// passes this exact limiter into ModuleConfig.PendingCallsLimiter.
			outerFrees := make([]func(), 0, callCount)
			for i := 0; i < callCount; i++ {
				free, waitErr := cfg.LocalLimiters.CapabilityConcurrency.Wait(ctx, 1)
				if waitErr != nil {
					setRunErr(fmt.Errorf("host-side limiter acquisition %d failed: %w", i, waitErr))
					break
				}
				outerFrees = append(outerFrees, free)
			}
			defer func() {
				for i := len(outerFrees) - 1; i >= 0; i-- {
					outerFrees[i]()
				}
			}()

			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
			defer release()

			callErrCh := make(chan error, callCount)
			launched := 0
			for i := 0; i < callCount; i++ {
				callbackID := int32(i + 1)
				launched++
				go func() {
					_, callErr := executor.CallCapability(ctx, &sdkpb.CapabilityRequest{
						Id:         capabilityID,
						Method:     "execute",
						CallbackId: callbackID,
					})
					callErrCh <- callErr
				}()

				var gotLimits []capabilities.SpendLimit
				select {
				case gotLimits = <-enteredCh:
				case <-time.After(5 * time.Second):
					setRunErr(fmt.Errorf("capability call %d did not enter Execute", i+1))
					continue
				}

				if i < callCount-1 {
					if len(gotLimits) != 1 || gotLimits[0].Limit != "1.0000000000" {
						setRunErr(fmt.Errorf("call %d: expected one-credit spend limit, got %#v", i+1, gotLimits))
					}
				} else if len(gotLimits) != 0 {
					setRunErr(fmt.Errorf("call %d: expected empty spend limits after ErrNoOpenCalls, got %#v", i+1, gotLimits))
				}
			}

			release()
			for i := 0; i < launched; i++ {
				select {
				case callErr := <-callErrCh:
					if callErr != nil {
						setRunErr(fmt.Errorf("capability call failed instead of failing open: %w", callErr))
					}
				case <-time.After(5 * time.Second):
					setRunErr(fmt.Errorf("timed out waiting for capability call %d", i+1))
				}
			}

			runErrCh <- runErr
		}).
		Return(nil, nil).
		Once()

	require.NoError(t, engine.Start(t.Context()))
	t.Cleanup(func() {
		require.NoError(t, engine.Close())
	})
	require.NoError(t, <-initDoneCh)
	require.Equal(t, []string{"id_0"}, <-subscribedToTriggersCh)

	eventCh <- capabilities.TriggerResponse{Event: capabilities.TriggerEvent{
		TriggerType: "basic-trigger@1.0.0",
		ID:          "metering_shared_limiter_poc",
	}}

	require.NoError(t, <-runErrCh)
	<-executionFinishedCh

	select {
	case receipt := <-receiptCh:
		consumedCredits, ok := new(big.Rat).SetString(receipt.CreditsConsumed)
		require.True(t, ok, "invalid consumed credit amount %q", receipt.CreditsConsumed)
		reservedCredits, ok := new(big.Rat).SetString(reserved)
		require.True(t, ok, "invalid reserved credit amount %q", reserved)
		require.Greater(t, consumedCredits.Cmp(reservedCredits), 0,
			"actual irreversible spend %s must exceed the %s-credit reservation",
			receipt.CreditsConsumed, reserved)
		t.Logf("metering fail-open reproduced: reserved=%s consumed=%s", reserved, receipt.CreditsConsumed)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for metering receipt")
	}
}
