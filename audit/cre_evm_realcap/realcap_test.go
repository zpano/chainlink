package realcap_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	commontest "github.com/smartcontractkit/capabilities/chain_capabilities/common/test"
	ts "github.com/smartcontractkit/capabilities/chain_capabilities/common/transmission_schedule"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/actions"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/config"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/monitoring"
	"github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	ocrtypes "github.com/smartcontractkit/chainlink-common/pkg/capabilities/consensus/ocr3/types"
	evmcap "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm"
	evmserver "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm/server"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/settings/limits"
	"github.com/smartcontractkit/chainlink-common/pkg/types"
	evmtypes "github.com/smartcontractkit/chainlink-common/pkg/types/chains/evm"
	"github.com/smartcontractkit/chainlink-evm/gethwrappers/keystone/generated/forwarder"
	workflowpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"
)

const chainSelector = uint64(12922642891491394802)

// infoOnlyCapability deliberately relies on the generated ClientCapability
// interface embedding for methods that Infos does not call. This lets the test
// exercise the exact generated EVM wrapper Info() implementation.
type infoOnlyCapability struct {
	evmserver.ClientCapability
}

func (*infoOnlyCapability) ChainSelector() uint64 { return chainSelector }
func (*infoOnlyCapability) Description() string   { return "real EVM wrapper metering probe" }

type recordingEVMService struct {
	types.UnimplementedEVMService

	mu               sync.Mutex
	events           []string
	transmissionRead int
	notAttemptedData []byte
	succeededData    []byte
	txHash           evmtypes.Hash
	feeWei           *big.Int
}

func newRecordingEVMService(t *testing.T) *recordingEVMService {
	t.Helper()

	forwarderABI, err := forwarder.KeystoneForwarderMetaData.GetAbi()
	require.NoError(t, err)

	notAttemptedData, err := forwarderABI.Methods["getTransmissionInfo"].Outputs.Pack(
		forwarder.IRouterTransmissionInfo{
			State:    0,
			GasLimit: big.NewInt(0),
		},
	)
	require.NoError(t, err)

	txHash := evmtypes.Hash(common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	succeededData, err := forwarderABI.Methods["getTransmissionInfo"].Outputs.Pack(
		forwarder.IRouterTransmissionInfo{
			State:       1,
			Transmitter: common.HexToAddress("0x2222222222222222222222222222222222222222"),
			Success:     true,
			GasLimit:    big.NewInt(300_000),
		},
	)
	require.NoError(t, err)

	return &recordingEVMService{
		notAttemptedData: notAttemptedData,
		succeededData:    succeededData,
		txHash:           txHash,
		feeWei:           new(big.Int).Mul(big.NewInt(2), big.NewInt(1_000_000_000_000_000_000)),
	}
}

func (s *recordingEVMService) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *recordingEVMService) snapshotEvents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *recordingEVMService) CallContract(_ context.Context, _ evmtypes.CallContractRequest) (*evmtypes.CallContractReply, error) {
	s.mu.Lock()
	s.transmissionRead++
	read := s.transmissionRead
	s.events = append(s.events, fmt.Sprintf("transmission-read-%d", read))
	s.mu.Unlock()

	if read == 1 {
		return &evmtypes.CallContractReply{Data: s.notAttemptedData}, nil
	}
	return &evmtypes.CallContractReply{Data: s.succeededData}, nil
}

func (s *recordingEVMService) SubmitTransaction(_ context.Context, req evmtypes.SubmitTransactionRequest) (*evmtypes.TransactionResult, error) {
	s.record("submit-transaction")
	if req.GasConfig == nil || req.GasConfig.GasLimit == nil || *req.GasConfig.GasLimit == 0 {
		return nil, fmt.Errorf("real WriteReport did not pass a positive transaction gas limit")
	}
	return &evmtypes.TransactionResult{
		TxStatus:         evmtypes.TxSuccess,
		TxHash:           s.txHash,
		TxIdempotencyKey: "real-capability-overspend-poc",
	}, nil
}

func (s *recordingEVMService) GetTransactionFee(_ context.Context, transactionID types.IdempotencyKey) (*evmtypes.TransactionFee, error) {
	s.record("get-actual-transaction-fee")
	if transactionID != "real-capability-overspend-poc" {
		return nil, fmt.Errorf("unexpected transaction id %q", transactionID)
	}
	return &evmtypes.TransactionFee{TransactionFee: new(big.Int).Set(s.feeWei)}, nil
}

func (s *recordingEVMService) HeaderByNumber(_ context.Context, _ evmtypes.HeaderByNumberRequest) (*evmtypes.HeaderByNumberReply, error) {
	s.record("read-latest-header")
	return &evmtypes.HeaderByNumberReply{Header: &evmtypes.Header{Number: big.NewInt(100)}}, nil
}

func (s *recordingEVMService) FilterLogs(_ context.Context, _ evmtypes.FilterLogsRequest) (*evmtypes.FilterLogsReply, error) {
	s.record("read-report-processed-log")
	data := make([]byte, 32)
	data[31] = 1
	return &evmtypes.FilterLogsReply{Logs: []*evmtypes.Log{{
		TxHash:      s.txHash,
		BlockNumber: big.NewInt(100),
		Data:        data,
	}}}, nil
}

func (s *recordingEVMService) GetTransactionReceipt(_ context.Context, req evmtypes.GeTransactionReceiptRequest) (*evmtypes.Receipt, error) {
	s.record("read-transaction-receipt")
	if req.Hash != s.txHash {
		return nil, fmt.Errorf("unexpected receipt hash")
	}
	return &evmtypes.Receipt{
		Status:            1,
		TxHash:            s.txHash,
		GasUsed:           2,
		BlockNumber:       big.NewInt(100),
		EffectiveGasPrice: big.NewInt(1_000_000_000_000_000_000),
	}, nil
}

func (s *recordingEVMService) CalculateTransactionFee(_ context.Context, info evmtypes.ReceiptGasInfo) (*evmtypes.TransactionFee, error) {
	s.record("calculate-receipt-fee")
	if info.EffectiveGasPrice == nil {
		return nil, fmt.Errorf("effective gas price is nil")
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(info.GasUsed), info.EffectiveGasPrice)
	return &evmtypes.TransactionFee{TransactionFee: fee}, nil
}

func indexOf(events []string, target string) int {
	for i, event := range events {
		if event == target {
			return i
		}
	}
	return -1
}

func TestRealGeneratedEVMWrapperAdvertisesNoGasSpendType(t *testing.T) {
	server := evmserver.NewClientServer(&infoOnlyCapability{})
	infos, err := server.Infos(t.Context())
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.Equal(t, "evm:ChainSelector:12922642891491394802@1.0.0", infos[0].ID)
	require.Empty(t, infos[0].SpendTypes,
		"the real generated EVM wrapper still gives the workflow engine no GAS spend type to reserve against")
}

func TestRealEVMWriteReportSpendsBeyondSuppliedGasLimitAfterSubmission(t *testing.T) {
	service := newRecordingEVMService(t)
	lggr := logger.Test(t)

	evmCapability, err := actions.NewEVM(
		config.Config{
			CREForwarderAddress:    "0x1111111111111111111111111111111111111111",
			ReceiverGasMinimum:     1_000,
			ForwarderLookbackBlocks: 100,
		},
		service,
		lggr,
		commontest.NopBeholderProcessor{},
		monitoring.NewMessageBuilder(types.ChainInfo{}, capabilities.CapabilityInfo{}, ""),
		nil,
		chainSelector,
		limits.Factory{Logger: lggr},
		ts.TransmissionScheduler{},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, evmCapability.Close()) })

	reportMetadata := ocrtypes.Metadata{
		Version:          1,
		ExecutionID:      strings.Repeat("11", 32),
		Timestamp:        1_000,
		DONID:            10,
		DONConfigVersion: 2,
		WorkflowID:       strings.Repeat("22", 32),
		WorkflowName:     strings.Repeat("33", 10),
		WorkflowOwner:    strings.Repeat("44", 20),
		ReportID:         strings.Repeat("55", 2),
	}
	rawReport, err := reportMetadata.Encode()
	require.NoError(t, err)

	metadata := capabilities.RequestMetadata{
		WorkflowID:               reportMetadata.WorkflowID,
		WorkflowOwner:            reportMetadata.WorkflowOwner,
		WorkflowName:             reportMetadata.WorkflowName,
		WorkflowDonID:            reportMetadata.DONID,
		WorkflowDonConfigVersion: reportMetadata.DONConfigVersion,
		WorkflowExecutionID:      reportMetadata.ExecutionID,
		ExecutionTimestamp:       time.Unix(1_800_000_000, 0),
		SpendLimits: []capabilities.SpendLimit{{
			SpendType: capabilities.CapabilitySpendType(fmt.Sprintf("GAS.%d", chainSelector)),
			Limit:     "1",
		}},
	}

	response, capErr := evmCapability.WriteReport(t.Context(), metadata, &evmcap.WriteReportRequest{
		Receiver: common.HexToAddress("0x3333333333333333333333333333333333333333").Bytes(),
		Report: &workflowpb.ReportResponse{
			RawReport:     rawReport,
			ReportContext: []byte{},
			Sigs: []*workflowpb.AttributedSignature{{
				Signature: []byte{1, 2, 3, 4},
			}},
		},
		GasConfig: &evmcap.GasConfig{GasLimit: 500_000},
	})
	require.NoError(t, capErr)
	require.NotNil(t, response)
	require.Len(t, response.ResponseMetadata.Metering, 1)

	metering := response.ResponseMetadata.Metering[0]
	require.Equal(t, fmt.Sprintf("GAS.%d", chainSelector), metering.SpendUnit)
	require.Equal(t, "2", metering.SpendValue)

	events := service.snapshotEvents()
	submitIndex := indexOf(events, "submit-transaction")
	feeIndex := indexOf(events, "get-actual-transaction-fee")
	require.NotEqual(t, -1, submitIndex, "the real capability must reach SubmitTransaction")
	require.NotEqual(t, -1, feeIndex, "the real capability must retrieve the actual fee")
	require.Less(t, submitIndex, feeIndex,
		"the irreversible transaction is submitted before its actual fee is learned")

	require.Equal(t, 1, len(metadata.SpendLimits))
	require.Equal(t, "1", metadata.SpendLimits[0].Limit)
	t.Logf("real EVM capability overspend reproduced: supplied GAS limit=%s actual metering=%s events=%v",
		metadata.SpendLimits[0].Limit, metering.SpendValue, events)
}
