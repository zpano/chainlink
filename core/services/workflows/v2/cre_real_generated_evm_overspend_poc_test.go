package v2_test

import (
	"context"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	captest "github.com/smartcontractkit/capabilities/chain_capabilities/common/test"
	ts "github.com/smartcontractkit/capabilities/chain_capabilities/common/transmission_schedule"
	evmactions "github.com/smartcontractkit/capabilities/chain_capabilities/evm/actions"
	evmconfig "github.com/smartcontractkit/capabilities/chain_capabilities/evm/config"
	evmmonitoring "github.com/smartcontractkit/capabilities/chain_capabilities/evm/monitoring"
	evmserver "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm/server"

	commoncap "github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	ocrtypes "github.com/smartcontractkit/chainlink-common/pkg/capabilities/consensus/ocr3/types"
	caperrors "github.com/smartcontractkit/chainlink-common/pkg/capabilities/errors"
	evmpb "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/settings/limits"
	commontypes "github.com/smartcontractkit/chainlink-common/pkg/types"
	evmtypes "github.com/smartcontractkit/chainlink-common/pkg/types/chains/evm"
	coretypes "github.com/smartcontractkit/chainlink-common/pkg/types/core"
	regmocks "github.com/smartcontractkit/chainlink-common/pkg/types/core/mocks"
	evmmocks "github.com/smartcontractkit/chainlink-common/pkg/types/mocks"
	"github.com/smartcontractkit/chainlink-common/pkg/workflows/host"
	modulemocks "github.com/smartcontractkit/chainlink-common/pkg/workflows/wasm/host/mocks"
	"github.com/smartcontractkit/chainlink-evm/gethwrappers/keystone/generated/forwarder"
	billing "github.com/smartcontractkit/chainlink-protos/billing/go"
	sdkpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"
	capmocks "github.com/smartcontractkit/chainlink/v2/core/capabilities/mocks"
	metmocks "github.com/smartcontractkit/chainlink/v2/core/services/workflows/metering/mocks"
	v2 "github.com/smartcontractkit/chainlink/v2/core/services/workflows/v2"
	"github.com/smartcontractkit/chainlink/v2/core/utils/matches"
)

// realEVMClientCapability supplies the lifecycle and trigger methods required by
// the generated EVM server wrapper while delegating all EVM action methods,
// including WriteReport, to the actual capabilities implementation.
type realEVMClientCapability struct {
	*evmactions.EVM
	chainSelector uint64
}

var _ evmserver.ClientCapability = (*realEVMClientCapability)(nil)

func (c *realEVMClientCapability) ChainSelector() uint64       { return c.chainSelector }
func (c *realEVMClientCapability) Start(context.Context) error { return nil }
func (c *realEVMClientCapability) Close() error                { return nil }
func (c *realEVMClientCapability) HealthReport() map[string]error {
	return map[string]error{"real-evm-capability": nil}
}
func (c *realEVMClientCapability) Name() string        { return "real-evm-capability" }
func (c *realEVMClientCapability) Description() string { return "real EVM capability PoC" }
func (c *realEVMClientCapability) Ready() error        { return nil }
func (c *realEVMClientCapability) Initialise(context.Context, coretypes.StandardCapabilitiesDependencies) error {
	return nil
}
func (c *realEVMClientCapability) RegisterLogTrigger(context.Context, string, commoncap.RequestMetadata, *evmpb.FilterLogTriggerRequest) (<-chan commoncap.TriggerAndId[*evmpb.Log], caperrors.Error) {
	return nil, nil
}
func (c *realEVMClientCapability) UnregisterLogTrigger(context.Context, string, commoncap.RequestMetadata, *evmpb.FilterLogTriggerRequest) caperrors.Error {
	return nil
}
func (c *realEVMClientCapability) AckEvent(context.Context, string, string, string) caperrors.Error {
	return nil
}

// reportInjectingExecutable emulates the SDK's GenerateReport step. It builds
// report metadata from the exact execution metadata produced by the real engine,
// then delegates unchanged to the generated production EVM wrapper.
type reportInjectingExecutable struct {
	inner          commoncap.ExecutableCapability
	receiver       common.Address
	spendLimitsCh  chan []commoncap.SpendLimit
	generatedCalls atomic.Int32
}

var _ commoncap.ExecutableCapability = (*reportInjectingExecutable)(nil)

func (p *reportInjectingExecutable) Info(ctx context.Context) (commoncap.CapabilityInfo, error) {
	return p.inner.Info(ctx)
}
func (p *reportInjectingExecutable) RegisterToWorkflow(ctx context.Context, req commoncap.RegisterToWorkflowRequest) error {
	return p.inner.RegisterToWorkflow(ctx, req)
}
func (p *reportInjectingExecutable) UnregisterFromWorkflow(ctx context.Context, req commoncap.UnregisterFromWorkflowRequest) error {
	return p.inner.UnregisterFromWorkflow(ctx, req)
}
func (p *reportInjectingExecutable) Execute(ctx context.Context, req commoncap.CapabilityRequest) (commoncap.CapabilityResponse, error) {
	p.generatedCalls.Add(1)
	p.spendLimitsCh <- append([]commoncap.SpendLimit(nil), req.Metadata.SpendLimits...)

	reportMetadata := ocrtypes.Metadata{
		Version:          1,
		ExecutionID:      req.Metadata.WorkflowExecutionID,
		Timestamp:        1000,
		DONID:            req.Metadata.WorkflowDonID,
		DONConfigVersion: req.Metadata.WorkflowDonConfigVersion,
		WorkflowID:       req.Metadata.WorkflowID,
		WorkflowName:     req.Metadata.WorkflowName,
		WorkflowOwner:    req.Metadata.WorkflowOwner,
		ReportID:         "0001",
	}
	rawReport, err := reportMetadata.Encode()
	if err != nil {
		return commoncap.CapabilityResponse{}, fmt.Errorf("encode report metadata: %w", err)
	}

	payload, err := anypb.New(&evmpb.WriteReportRequest{
		Receiver: p.receiver.Bytes(),
		Report: &sdkpb.ReportResponse{
			RawReport:     rawReport,
			ReportContext: make([]byte, 96),
			Sigs: []*sdkpb.AttributedSignature{{
				Signature: make([]byte, 65),
			}},
		},
		GasConfig: &evmpb.GasConfig{GasLimit: 5_000_000},
	})
	if err != nil {
		return commoncap.CapabilityResponse{}, fmt.Errorf("wrap WriteReport request: %w", err)
	}
	req.Payload = payload
	return p.inner.Execute(ctx, req)
}

type transmissionInfoABI struct {
	GasLimit        *big.Int
	InvalidReceiver bool
	State           uint8
	Success         bool
	TransmissionId  [32]byte
	Transmitter     common.Address
}

func encodeTransmissionInfo(t *testing.T, info transmissionInfoABI) []byte {
	t.Helper()
	forwarderABI, err := forwarder.KeystoneForwarderMetaData.GetAbi()
	require.NoError(t, err)
	encoded, err := forwarderABI.Methods["getTransmissionInfo"].Outputs.Pack(info)
	require.NoError(t, err)
	return encoded
}

// TestPoC_RealGeneratedEVMWriteReportExceedsReservation crosses the actual
// current Workflow Engine with the official generated EVM wrapper and the real
// EVM WriteReport action. Only the RPC/txmgr and private Billing service
// boundaries are mocked.
func TestPoC_RealGeneratedEVMWriteReportExceedsReservation(t *testing.T) {
	const (
		reserved      = "1"
		chainSelector = uint64(12922642891491394802)
		capabilityID  = "evm:ChainSelector:12922642891491394802@1.0.0"
	)
	feeWei := new(big.Int).Mul(big.NewInt(2), big.NewInt(1e18))
	forwarderAddress := common.HexToAddress("0x1000000000000000000000000000000000000001")
	receiverAddress := common.HexToAddress("0x2000000000000000000000000000000000000002")
	txHash := evmtypes.Hash{0xaa, 0xbb, 0xcc}

	lggr := logger.Test(t)
	evmService := evmmocks.NewEVMService(t)
	realEVM, capErr := evmactions.NewEVM(
		evmconfig.Config{
			CREForwarderAddress: forwarderAddress.Hex(),
			ReceiverGasMinimum:  1_000,
		},
		evmService,
		lggr,
		captest.NopBeholderProcessor{},
		evmmonitoring.NewMessageBuilder(commontypes.ChainInfo{}, commoncap.CapabilityInfo{}, ""),
		nil,
		chainSelector,
		limits.Factory{Logger: lggr},
		ts.TransmissionScheduler{},
	)
	require.NoError(t, capErr)
	t.Cleanup(func() { require.NoError(t, realEVM.Close()) })

	client := &realEVMClientCapability{EVM: realEVM, chainSelector: chainSelector}
	generated := evmserver.NewClientServer(client)
	info, err := generated.Info(t.Context())
	require.NoError(t, err)
	require.True(t, info.IsLocal)
	require.Empty(t, info.SpendTypes, "official generated EVM wrapper still advertises no GAS spend type")

	spendLimitsCh := make(chan []commoncap.SpendLimit, 1)
	probe := &reportInjectingExecutable{
		inner:         generated,
		receiver:      receiverAddress,
		spendLimitsCh: spendLimitsCh,
	}

	var sequence atomic.Int32
	var submitOrder atomic.Int32
	var feeOrder atomic.Int32
	var receiptOrder atomic.Int32

	notAttempted := encodeTransmissionInfo(t, transmissionInfoABI{
		GasLimit: big.NewInt(0),
		State:    0,
	})
	succeeded := encodeTransmissionInfo(t, transmissionInfoABI{
		GasLimit:       big.NewInt(4_500_000),
		State:          1,
		Success:        true,
		TransmissionId: [32]byte{0x42},
		Transmitter:    common.HexToAddress("0x3000000000000000000000000000000000000003"),
	})
	var transmissionReads atomic.Int32
	evmService.EXPECT().
		CallContract(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ evmtypes.CallContractRequest) (*evmtypes.CallContractReply, error) {
			if transmissionReads.Add(1) == 1 {
				return &evmtypes.CallContractReply{Data: notAttempted}, nil
			}
			return &evmtypes.CallContractReply{Data: succeeded}, nil
		}).
		Times(2)

	evmService.EXPECT().
		SubmitTransaction(mock.Anything, mock.MatchedBy(func(req evmtypes.SubmitTransactionRequest) bool {
			return req.To == forwarderAddress && req.GasConfig != nil && req.GasConfig.GasLimit != nil && *req.GasConfig.GasLimit == 5_000_000
		})).
		RunAndReturn(func(_ context.Context, _ evmtypes.SubmitTransactionRequest) (*evmtypes.TransactionResult, error) {
			submitOrder.Store(sequence.Add(1))
			return &evmtypes.TransactionResult{
				TxHash:           txHash,
				TxStatus:         evmtypes.TxSuccess,
				TxIdempotencyKey: "real-generated-evm-poc",
			}, nil
		}).
		Once()

	evmService.EXPECT().
		GetTransactionFee(mock.Anything, "real-generated-evm-poc").
		RunAndReturn(func(context.Context, string) (*evmtypes.TransactionFee, error) {
			feeOrder.Store(sequence.Add(1))
			return &evmtypes.TransactionFee{TransactionFee: new(big.Int).Set(feeWei)}, nil
		}).
		Once()

	evmService.EXPECT().
		HeaderByNumber(mock.Anything, mock.Anything).
		Return(&evmtypes.HeaderByNumberReply{Header: &evmtypes.Header{Number: big.NewInt(100)}}, nil).
		Once()

	successEventData := make([]byte, 32)
	successEventData[31] = 1
	evmService.EXPECT().
		FilterLogs(mock.Anything, mock.Anything).
		Return(&evmtypes.FilterLogsReply{Logs: []*evmtypes.Log{{
			TxHash:      txHash,
			BlockNumber: big.NewInt(100),
			Data:        successEventData,
		}}}, nil).
		Once()

	receipt := &evmtypes.Receipt{
		Status:            1,
		TxHash:            txHash,
		GasUsed:           1_000_000,
		EffectiveGasPrice: big.NewInt(2_000_000_000_000),
	}
	evmService.EXPECT().
		GetTransactionReceipt(mock.Anything, evmtypes.GeTransactionReceiptRequest{Hash: txHash, IsExternal: false}).
		Return(receipt, nil).
		Once()
	evmService.EXPECT().
		CalculateTransactionFee(mock.Anything, mock.Anything).
		Return(&evmtypes.TransactionFee{TransactionFee: new(big.Int).Set(feeWei)}, nil).
		Once()

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
			GasTokensPerCredit: map[uint64]string{chainSelector: "1000000000000000000"},
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
			receiptOrder.Store(sequence.Add(1))
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
		OnInitialized:          func(err error) { initDoneCh <- err },
		OnSubscribedToTriggers: func(ids []string) { subscribedCh <- ids },
		OnExecutionFinished:    func(id string, _ string) { finishedCh <- id },
	}

	engine, err := v2.NewEngine(cfg)
	require.NoError(t, err)

	trigger := capmocks.NewTriggerCapability(t)
	eventCh := make(chan commoncap.TriggerResponse, 1)
	module.EXPECT().Execute(matches.AnyContext, mock.Anything, mock.Anything).Return(newTriggerSubs(1), nil).Once()
	capreg.EXPECT().GetTrigger(matches.AnyContext, "id_0").Return(trigger, nil).Once()
	trigger.EXPECT().RegisterTrigger(matches.AnyContext, mock.Anything).Return(eventCh, nil).Once()
	trigger.EXPECT().UnregisterTrigger(matches.AnyContext, mock.Anything).Return(nil).Once()
	trigger.EXPECT().AckEvent(matches.AnyContext, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	capreg.EXPECT().GetExecutable(matches.AnyContext, capabilityID).Return(probe, nil).Once()
	capreg.EXPECT().
		ConfigForCapability(mock.Anything, capabilityID, mock.Anything).
		Return(commoncap.CapabilityConfiguration{}, nil).
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
	require.Equal(t, []string{"id_0"}, <-subscribedCh)

	eventCh <- commoncap.TriggerResponse{Event: commoncap.TriggerEvent{
		TriggerType: "basic-trigger@1.0.0",
		ID:          "real_generated_evm_overspend_poc",
	}}

	require.NoError(t, <-runErrCh)
	select {
	case <-finishedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for workflow execution")
	}

	select {
	case limitsSeen := <-spendLimitsCh:
		require.Empty(t, limitsSeen, "real generated wrapper was invoked without a GAS spend limit")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for generated wrapper invocation")
	}

	select {
	case billingReceipt := <-receiptCh:
		require.Equal(t, "2", billingReceipt.CreditsConsumed)
		require.Equal(t, int32(1), probe.generatedCalls.Load())
		require.Positive(t, submitOrder.Load())
		require.Greater(t, feeOrder.Load(), submitOrder.Load(), "actual fee must be learned after transaction submission")
		require.Greater(t, receiptOrder.Load(), feeOrder.Load(), "billing receipt must be emitted after irreversible fee is known")
		t.Logf("real generated EVM path reproduced: reserved=%s consumed=%s order submit=%d fee=%d receipt=%d",
			reserved, billingReceipt.CreditsConsumed, submitOrder.Load(), feeOrder.Load(), receiptOrder.Load())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for billing receipt")
	}
}
