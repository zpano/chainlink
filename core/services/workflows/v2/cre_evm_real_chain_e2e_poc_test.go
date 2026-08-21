//go:build auditcre

package v2_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/abi/bind/backends"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	commontest "github.com/smartcontractkit/capabilities/chain_capabilities/common/test"
	ts "github.com/smartcontractkit/capabilities/chain_capabilities/common/transmission_schedule"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/actions"
	evmconfig "github.com/smartcontractkit/capabilities/chain_capabilities/evm/config"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/monitoring"
	"github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	ocrtypes "github.com/smartcontractkit/chainlink-common/pkg/capabilities/consensus/ocr3/types"
	caperrors "github.com/smartcontractkit/chainlink-common/pkg/capabilities/errors"
	evmcap "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm"
	evmserver "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm/server"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/settings/limits"
	regmocks "github.com/smartcontractkit/chainlink-common/pkg/types/core/mocks"
	cltypes "github.com/smartcontractkit/chainlink-common/pkg/types"
	evmtypes "github.com/smartcontractkit/chainlink-common/pkg/types/chains/evm"
	"github.com/smartcontractkit/chainlink-common/pkg/workflows/host"
	modulemocks "github.com/smartcontractkit/chainlink-common/pkg/workflows/wasm/host/mocks"
	billing "github.com/smartcontractkit/chainlink-protos/billing/go"
	workflowpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"
	forwarder "github.com/smartcontractkit/chainlink-evm/gethwrappers/keystone/generated/forwarder"
	reservemanager "github.com/smartcontractkit/chainlink-evm/gethwrappers/workflow/generated/reserve_manager"
	capmocks "github.com/smartcontractkit/chainlink/v2/core/capabilities/mocks"
	metmocks "github.com/smartcontractkit/chainlink/v2/core/services/workflows/metering/mocks"
	v2 "github.com/smartcontractkit/chainlink/v2/core/services/workflows/v2"
	"github.com/smartcontractkit/chainlink/v2/core/utils/matches"
)

const e2eChainSelector = uint64(12922642891491394802)

type e2eSimulatedChainEVMService struct {
	cltypes.UnimplementedEVMService

	backend          *backends.SimulatedBackend
	auth             bind.TransactOpts
	forwarderAddress common.Address
	forwarderABI     abi.ABI

	mu       sync.Mutex
	receipts map[string]*gethtypes.Receipt
	lastFee  *big.Int
}

func newE2ESimulatedChainEVMService(
	backend *backends.SimulatedBackend,
	auth *bind.TransactOpts,
	forwarderAddress common.Address,
) (*e2eSimulatedChainEVMService, error) {
	forwarderABI, err := forwarder.KeystoneForwarderMetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return &e2eSimulatedChainEVMService{
		backend:          backend,
		auth:             *auth,
		forwarderAddress: forwarderAddress,
		forwarderABI:     *forwarderABI,
		receipts:         make(map[string]*gethtypes.Receipt),
	}, nil
}

func e2ePositiveBlock(number *big.Int) *big.Int {
	if number == nil || number.Sign() < 0 {
		return nil
	}
	return number
}

func (s *e2eSimulatedChainEVMService) CallContract(ctx context.Context, req evmtypes.CallContractRequest) (*evmtypes.CallContractReply, error) {
	if req.Msg == nil {
		return nil, fmt.Errorf("call message is nil")
	}
	to := common.Address(req.Msg.To)
	data, err := s.backend.CallContract(ctx, ethereum.CallMsg{
		From: common.Address(req.Msg.From),
		To:   &to,
		Data: req.Msg.Data,
	}, e2ePositiveBlock(req.BlockNumber))
	if err != nil {
		return nil, err
	}
	return &evmtypes.CallContractReply{Data: data}, nil
}

func (s *e2eSimulatedChainEVMService) SubmitTransaction(ctx context.Context, req evmtypes.SubmitTransactionRequest) (*evmtypes.TransactionResult, error) {
	if common.Address(req.To) != s.forwarderAddress {
		return nil, fmt.Errorf("unexpected transaction destination %s", common.Address(req.To))
	}
	if req.GasConfig == nil || req.GasConfig.GasLimit == nil || *req.GasConfig.GasLimit == 0 {
		return nil, fmt.Errorf("missing positive gas limit")
	}

	opts := s.auth
	opts.Context = ctx
	opts.Nonce = nil
	opts.GasLimit = *req.GasConfig.GasLimit
	contract := bind.NewBoundContract(s.forwarderAddress, s.forwarderABI, s.backend, s.backend, s.backend)
	tx, err := contract.RawTransact(&opts, req.Data)
	if err != nil {
		return nil, err
	}
	s.backend.Commit()

	receipt, err := s.backend.TransactionReceipt(ctx, tx.Hash())
	if err != nil {
		return nil, err
	}
	idempotencyKey := tx.Hash().Hex()

	s.mu.Lock()
	s.receipts[idempotencyKey] = receipt
	s.lastFee = e2eTransactionFeeFromReceipt(receipt)
	s.mu.Unlock()

	status := evmtypes.TxReverted
	if receipt.Status == gethtypes.ReceiptStatusSuccessful {
		status = evmtypes.TxSuccess
	}
	return &evmtypes.TransactionResult{
		TxStatus:         status,
		TxHash:           evmtypes.Hash(tx.Hash()),
		TxIdempotencyKey: idempotencyKey,
	}, nil
}

func e2eTransactionFeeFromReceipt(receipt *gethtypes.Receipt) *big.Int {
	if receipt == nil || receipt.EffectiveGasPrice == nil {
		return new(big.Int)
	}
	return new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
}

func (s *e2eSimulatedChainEVMService) GetTransactionFee(_ context.Context, transactionID cltypes.IdempotencyKey) (*evmtypes.TransactionFee, error) {
	s.mu.Lock()
	receipt := s.receipts[transactionID]
	s.mu.Unlock()
	if receipt == nil {
		return nil, fmt.Errorf("unknown transaction id %q", transactionID)
	}
	return &evmtypes.TransactionFee{TransactionFee: e2eTransactionFeeFromReceipt(receipt)}, nil
}

func (s *e2eSimulatedChainEVMService) HeaderByNumber(ctx context.Context, req evmtypes.HeaderByNumberRequest) (*evmtypes.HeaderByNumberReply, error) {
	header, err := s.backend.HeaderByNumber(ctx, e2ePositiveBlock(req.Number))
	if err != nil {
		return nil, err
	}
	return &evmtypes.HeaderByNumberReply{Header: &evmtypes.Header{
		Timestamp:  header.Time,
		Hash:       evmtypes.Hash(header.Hash()),
		ParentHash: evmtypes.Hash(header.ParentHash),
		Number:     new(big.Int).Set(header.Number),
	}}, nil
}

func (s *e2eSimulatedChainEVMService) FilterLogs(ctx context.Context, req evmtypes.FilterLogsRequest) (*evmtypes.FilterLogsReply, error) {
	query := ethereum.FilterQuery{
		FromBlock: req.FilterQuery.FromBlock,
		ToBlock:   req.FilterQuery.ToBlock,
	}
	for _, address := range req.FilterQuery.Addresses {
		query.Addresses = append(query.Addresses, common.Address(address))
	}
	for _, topicSet := range req.FilterQuery.Topics {
		converted := make([]common.Hash, 0, len(topicSet))
		for _, topic := range topicSet {
			converted = append(converted, common.Hash(topic))
		}
		query.Topics = append(query.Topics, converted)
	}
	var zeroHash evmtypes.Hash
	if req.FilterQuery.BlockHash != zeroHash {
		blockHash := common.Hash(req.FilterQuery.BlockHash)
		query.BlockHash = &blockHash
	}

	logs, err := s.backend.FilterLogs(ctx, query)
	if err != nil {
		return nil, err
	}
	reply := &evmtypes.FilterLogsReply{Logs: make([]*evmtypes.Log, 0, len(logs))}
	for i := range logs {
		reply.Logs = append(reply.Logs, e2eConvertGethLog(logs[i]))
	}
	return reply, nil
}

func e2eConvertGethLog(log gethtypes.Log) *evmtypes.Log {
	topics := make([]evmtypes.Hash, 0, len(log.Topics))
	for _, topic := range log.Topics {
		topics = append(topics, evmtypes.Hash(topic))
	}
	return &evmtypes.Log{
		LogIndex:    uint32(log.Index),
		BlockHash:   evmtypes.Hash(log.BlockHash),
		BlockNumber: new(big.Int).SetUint64(log.BlockNumber),
		Topics:      topics,
		Address:     evmtypes.Address(log.Address),
		TxHash:      evmtypes.Hash(log.TxHash),
		Data:        append([]byte(nil), log.Data...),
		Removed:     log.Removed,
	}
}

func (s *e2eSimulatedChainEVMService) GetTransactionReceipt(ctx context.Context, req evmtypes.GeTransactionReceiptRequest) (*evmtypes.Receipt, error) {
	receipt, err := s.backend.TransactionReceipt(ctx, common.Hash(req.Hash))
	if err != nil {
		return nil, err
	}
	logs := make([]*evmtypes.Log, 0, len(receipt.Logs))
	for _, log := range receipt.Logs {
		logs = append(logs, e2eConvertGethLog(*log))
	}
	return &evmtypes.Receipt{
		Status:            receipt.Status,
		Logs:              logs,
		TxHash:            evmtypes.Hash(receipt.TxHash),
		ContractAddress:   evmtypes.Address(receipt.ContractAddress),
		GasUsed:           receipt.GasUsed,
		BlockHash:         evmtypes.Hash(receipt.BlockHash),
		BlockNumber:       new(big.Int).Set(receipt.BlockNumber),
		TransactionIndex:  uint64(receipt.TransactionIndex),
		EffectiveGasPrice: new(big.Int).Set(receipt.EffectiveGasPrice),
	}, nil
}

func (s *e2eSimulatedChainEVMService) CalculateTransactionFee(_ context.Context, info evmtypes.ReceiptGasInfo) (*evmtypes.TransactionFee, error) {
	if info.EffectiveGasPrice == nil {
		return nil, fmt.Errorf("effective gas price is nil")
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(info.GasUsed), info.EffectiveGasPrice)
	if info.L1Fee != nil {
		fee.Add(fee, info.L1Fee)
	}
	return &evmtypes.TransactionFee{TransactionFee: fee}, nil
}

func (s *e2eSimulatedChainEVMService) recordedFee() *big.Int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastFee == nil {
		return nil
	}
	return new(big.Int).Set(s.lastFee)
}

type e2eEVMClientCapability struct {
	*actions.EVM
}

// actions.EVM implements the generated client surface except AckEvent, which
// is only relevant to LogTrigger. This adapter keeps the generated wrapper in
// the execution path without adding an unrelated trigger dependency.
func (*e2eEVMClientCapability) AckEvent(context.Context, string, string, string) caperrors.Error {
	return nil
}

type e2eExecutableWithInfo struct {
	capabilities.ExecutableCapability
	info              capabilities.CapabilityInfo
	seenSpendLimitsCh chan []capabilities.SpendLimit
}

func (e *e2eExecutableWithInfo) Info(context.Context) (capabilities.CapabilityInfo, error) {
	return e.info, nil
}

func (e *e2eExecutableWithInfo) Execute(ctx context.Context, request capabilities.CapabilityRequest) (capabilities.CapabilityResponse, error) {
	limitsCopy := append([]capabilities.SpendLimit(nil), request.Metadata.SpendLimits...)
	e.seenSpendLimitsCh <- limitsCopy
	return e.ExecutableCapability.Execute(ctx, request)
}

func e2eSignReport(t *testing.T, rawReport, reportContext []byte, signerKeys ...*ecdsa.PrivateKey) []*workflowpb.AttributedSignature {
	t.Helper()
	innerHash := crypto.Keccak256Hash(rawReport)
	completeHash := crypto.Keccak256Hash(append(innerHash.Bytes(), reportContext...))
	signatures := make([]*workflowpb.AttributedSignature, 0, len(signerKeys))
	for _, signerKey := range signerKeys {
		signature, err := crypto.Sign(completeHash.Bytes(), signerKey)
		require.NoError(t, err)
		require.Len(t, signature, 65)
		signatures = append(signatures, &workflowpb.AttributedSignature{Signature: signature})
	}
	return signatures
}

// TestPoC_EngineRealEVMWriteReportSpendsBeyondReservation is the full public-
// component validation chain:
//
//   Workflow Engine -> Billing reservation -> generated EVM wrapper -> real
//   EVM WriteReport -> real KeystoneForwarder + valid DON signatures -> receiver
//   state change + transmitter gas payment -> Engine settlement -> Billing receipt.
//
// The only mocked boundary is the private Billing Platform RPC. Its responses
// and request are captured at the same interface used in production.
func TestPoC_EngineRealEVMWriteReportSpendsBeyondReservation(t *testing.T) {
	transmitterKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth, err := bind.NewKeyedTransactorWithChainID(transmitterKey, big.NewInt(1337))
	require.NoError(t, err)

	initialBalance := new(big.Int).Exp(big.NewInt(10), big.NewInt(25), nil)
	backend := backends.NewSimulatedBackend(gethtypes.GenesisAlloc{
		auth.From: {Balance: initialBalance},
	}, 30_000_000)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })

	forwarderAddress, _, forwarderContract, err := forwarder.DeployKeystoneForwarder(auth, backend)
	require.NoError(t, err)
	backend.Commit()

	const (
		donID         = uint32(10)
		configVersion = uint32(2)
		faults        = uint8(1)
	)
	signerKeys := make([]*ecdsa.PrivateKey, 0, 4)
	signerAddresses := make([]common.Address, 0, 4)
	for i := 0; i < 4; i++ {
		key, keyErr := crypto.GenerateKey()
		require.NoError(t, keyErr)
		signerKeys = append(signerKeys, key)
		signerAddresses = append(signerAddresses, crypto.PubkeyToAddress(key.PublicKey))
	}
	_, err = forwarderContract.SetConfig(auth, donID, configVersion, faults, signerAddresses)
	require.NoError(t, err)
	backend.Commit()

	reserveAddress, _, reserveManager, err := reservemanager.DeployReserveManager(auth, backend)
	require.NoError(t, err)
	backend.Commit()

	service, err := newE2ESimulatedChainEVMService(backend, auth, forwarderAddress)
	require.NoError(t, err)
	lggr := logger.Test(t)
	evmCapability, err := actions.NewEVM(
		evmconfig.Config{
			CREForwarderAddress:     forwarderAddress.Hex(),
			ReceiverGasMinimum:      1_000,
			ForwarderLookbackBlocks: 100,
		},
		service,
		lggr,
		commontest.NopBeholderProcessor{},
		monitoring.NewMessageBuilder(cltypes.ChainInfo{}, capabilities.CapabilityInfo{}, ""),
		nil,
		e2eChainSelector,
		limits.Factory{Logger: lggr},
		ts.TransmissionScheduler{},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, evmCapability.Close()) })

	capabilityID := fmt.Sprintf("evm:ChainSelector:%d@1.0.0", e2eChainSelector)
	generatedServer := evmserver.NewClientServer(&e2eEVMClientCapability{EVM: evmCapability})
	seenSpendLimitsCh := make(chan []capabilities.SpendLimit, 1)
	executable := &e2eExecutableWithInfo{
		ExecutableCapability: generatedServer,
		info: capabilities.CapabilityInfo{
			ID:             capabilityID,
			CapabilityType: capabilities.CapabilityTypeCombined,
			DON:            &capabilities.DON{ID: 42},
			SpendTypes:     nil,
		},
		seenSpendLimitsCh: seenSpendLimitsCh,
	}

	const reservedCredits = "0.000000000000000001" // one wei at one native token per credit
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
				e2eChainSelector: "1000000000000000000",
			},
		}, nil).
		Once()
	billingClient.EXPECT().
		ReserveCredits(mock.Anything, mock.Anything).
		Return(&billing.ReserveCreditsResponse{Success: true, Credits: reservedCredits}, nil).
		Once()

	receiptCh := make(chan *billing.SubmitWorkflowReceiptRequest, 1)
	billingClient.EXPECT().
		SubmitWorkflowReceipt(mock.Anything, mock.Anything).
		Run(func(_ context.Context, req *billing.SubmitWorkflowReceiptRequest) {
			receiptCh <- req
		}).
		Return(&emptypb.Empty{}, nil).
		Once()

	module := modulemocks.NewModuleV2(t)
	module.EXPECT().Start()
	module.EXPECT().Close()
	capreg := regmocks.NewCapabilitiesRegistry(t)
	capreg.EXPECT().LocalNode(matches.AnyContext).Return(newNode(t), nil)

	initDoneCh := make(chan error, 1)
	subscribedCh := make(chan []string, 1)
	finishedCh := make(chan string, 1)
	runErrCh := make(chan error, 1)
	executionIDCh := make(chan string, 1)

	cfg := defaultTestConfig(t, nil)
	cfg.Module = module
	cfg.CapRegistry = capreg
	cfg.BillingClient = billingClient
	cfg.Hooks = v2.LifecycleHooks{
		OnInitialized:            func(err error) { initDoneCh <- err },
		OnSubscribedToTriggers:   func(ids []string) { subscribedCh <- ids },
		OnExecutionFinished:      func(id string, _ string) { finishedCh <- id },
	}

	trigger := capmocks.NewTriggerCapability(t)
	eventCh := make(chan capabilities.TriggerResponse, 1)
	module.EXPECT().Execute(matches.AnyContext, mock.Anything, mock.Anything).Return(newTriggerSubs(1), nil).Once()
	capreg.EXPECT().GetTrigger(matches.AnyContext, "id_0").Return(trigger, nil).Once()
	trigger.EXPECT().RegisterTrigger(matches.AnyContext, mock.Anything).Return(eventCh, nil).Once()
	trigger.EXPECT().UnregisterTrigger(matches.AnyContext, mock.Anything).Return(nil).Once()
	trigger.EXPECT().AckEvent(matches.AnyContext, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	capreg.EXPECT().GetExecutable(matches.AnyContext, capabilityID).Return(executable, nil).Once()
	capreg.EXPECT().
		ConfigForCapability(mock.Anything, capabilityID, uint32(42)).
		Return(capabilities.CapabilityConfiguration{}, nil).
		Once()

	reportID := [2]byte{0x75, 0x75}
	uint256Type, err := abi.NewType("uint256", "", nil)
	require.NoError(t, err)
	payload, err := (abi.Arguments{{Type: uint256Type}, {Type: uint256Type}}).Pack(big.NewInt(789), big.NewInt(987))
	require.NoError(t, err)

	module.EXPECT().
		Execute(matches.AnyContext, mock.Anything, mock.Anything).
		Run(func(ctx context.Context, _ *workflowpb.ExecuteRequest, executor host.ExecutionHelper) {
			executionID := executor.GetWorkflowExecutionID()
			executionIDCh <- executionID
			reportMetadata := ocrtypes.Metadata{
				Version:          1,
				ExecutionID:      executionID,
				Timestamp:        1_000,
				DONID:            donID,
				DONConfigVersion: configVersion,
				WorkflowID:       cfg.WorkflowID,
				WorkflowName:     cfg.WorkflowName.Hex(),
				WorkflowOwner:    cfg.WorkflowOwner,
				ReportID:         hex.EncodeToString(reportID[:]),
			}
			encodedMetadata, encodeErr := reportMetadata.Encode()
			if encodeErr != nil {
				runErrCh <- encodeErr
				return
			}
			rawReport := append(append([]byte(nil), encodedMetadata...), payload...)
			reportContext := make([]byte, 96)
			writeRequest := &evmcap.WriteReportRequest{
				Receiver: reserveAddress.Bytes(),
				Report: &workflowpb.ReportResponse{
					RawReport:     rawReport,
					ReportContext: reportContext,
					Sigs:          e2eSignReport(t, rawReport, reportContext, signerKeys[0], signerKeys[1]),
				},
				GasConfig: &evmcap.GasConfig{GasLimit: 1_000_000},
			}
			wrapped, wrapErr := anypb.New(writeRequest)
			if wrapErr != nil {
				runErrCh <- wrapErr
				return
			}
			_, callErr := executor.CallCapability(ctx, &workflowpb.CapabilityRequest{
				Id:         capabilityID,
				Method:     "WriteReport",
				CallbackId: 1,
				Payload:    wrapped,
			})
			runErrCh <- callErr
		}).
		Return(nil, nil).
		Once()

	engine, err := v2.NewEngine(cfg)
	require.NoError(t, err)
	require.NoError(t, engine.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	require.NoError(t, <-initDoneCh)
	require.Equal(t, []string{"id_0"}, <-subscribedCh)

	balanceBefore, err := backend.BalanceAt(t.Context(), auth.From, nil)
	require.NoError(t, err)
	eventCh <- capabilities.TriggerResponse{Event: capabilities.TriggerEvent{
		TriggerType: "basic-trigger@1.0.0",
		ID:          "engine_real_evm_overspend_poc",
	}}
	require.NoError(t, <-runErrCh)

	select {
	case <-finishedCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for workflow execution")
	}
	balanceAfter, err := backend.BalanceAt(t.Context(), auth.From, nil)
	require.NoError(t, err)

	seenLimits := <-seenSpendLimitsCh
	require.Empty(t, seenLimits, "generated EVM wrapper must receive no pre-execution GAS spend limit")

	minted, err := reserveManager.LastTotalMinted(&bind.CallOpts{Context: t.Context()})
	require.NoError(t, err)
	reserve, err := reserveManager.LastTotalReserve(&bind.CallOpts{Context: t.Context()})
	require.NoError(t, err)
	require.Equal(t, int64(789), minted.Int64())
	require.Equal(t, int64(987), reserve.Int64())

	executionIDHex := <-executionIDCh
	executionIDBytes, err := hex.DecodeString(executionIDHex)
	require.NoError(t, err)
	require.Len(t, executionIDBytes, 32)
	var executionID [32]byte
	copy(executionID[:], executionIDBytes)
	transmission, err := forwarderContract.GetTransmissionInfo(
		&bind.CallOpts{Context: t.Context()}, reserveAddress, executionID, reportID,
	)
	require.NoError(t, err)
	require.Equal(t, uint8(1), transmission.State)
	require.True(t, transmission.Success)
	require.Equal(t, auth.From, transmission.Transmitter)

	paid := new(big.Int).Sub(balanceBefore, balanceAfter)
	recordedFee := service.recordedFee()
	require.NotNil(t, recordedFee)
	require.Positive(t, paid.Sign())
	require.Zero(t, paid.Cmp(recordedFee))
	require.Greater(t, paid.Cmp(big.NewInt(1)), 0)

	select {
	case receipt := <-receiptCh:
		consumed, ok := new(big.Rat).SetString(receipt.CreditsConsumed)
		require.True(t, ok, "invalid consumed credits %q", receipt.CreditsConsumed)
		reservedAmount, ok := new(big.Rat).SetString(reservedCredits)
		require.True(t, ok)
		require.Greater(t, consumed.Cmp(reservedAmount), 0,
			"post-facto consumed credits %s must exceed reservation %s",
			receipt.CreditsConsumed, reservedCredits)
		t.Logf("engine-to-chain overspend reproduced: reserved=%s consumed=%s feeWei=%s receiverState=(%s,%s) workflowStatus=success",
			reservedCredits, receipt.CreditsConsumed, paid.String(), minted.String(), reserve.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for billing receipt")
	}
}
