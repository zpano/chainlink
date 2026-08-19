package v2_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

	captest "github.com/smartcontractkit/capabilities/chain_capabilities/common/test"
	ts "github.com/smartcontractkit/capabilities/chain_capabilities/common/transmission_schedule"
	evmactions "github.com/smartcontractkit/capabilities/chain_capabilities/evm/actions"
	evmconfig "github.com/smartcontractkit/capabilities/chain_capabilities/evm/config"
	evmmonitoring "github.com/smartcontractkit/capabilities/chain_capabilities/evm/monitoring"
	commoncap "github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	ocrtypes "github.com/smartcontractkit/chainlink-common/pkg/capabilities/consensus/ocr3/types"
	evmpb "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm"
	evmserver "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm/server"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/settings/limits"
	commontypes "github.com/smartcontractkit/chainlink-common/pkg/types"
	evmtypes "github.com/smartcontractkit/chainlink-common/pkg/types/chains/evm"
	regmocks "github.com/smartcontractkit/chainlink-common/pkg/types/core/mocks"
	"github.com/smartcontractkit/chainlink-common/pkg/workflows/host"
	modulemocks "github.com/smartcontractkit/chainlink-common/pkg/workflows/wasm/host/mocks"
	forwarderbinding "github.com/smartcontractkit/chainlink-evm/gethwrappers/keystone/generated/forwarder"
	billing "github.com/smartcontractkit/chainlink-protos/billing/go"
	sdkpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"
	capmocks "github.com/smartcontractkit/chainlink/v2/core/capabilities/mocks"
	metmocks "github.com/smartcontractkit/chainlink/v2/core/services/workflows/metering/mocks"
	v2 "github.com/smartcontractkit/chainlink/v2/core/services/workflows/v2"
	"github.com/smartcontractkit/chainlink/v2/core/utils/matches"
)

const verticalSimulatedChainID = int64(1337)

type verticalSimulatedEVMService struct {
	// Promote irrelevant methods. Every EVMService method reached by the real
	// WriteReport/forwarder path is implemented explicitly below.
	commontypes.EVMService

	backend *backends.SimulatedBackend
	key     *ecdsa.PrivateKey
	chainID *big.Int
	from    common.Address

	mu                sync.Mutex
	feesByID          map[commontypes.IdempotencyKey]*big.Int
	lastReceipt       *gethtypes.Receipt
	lastBalanceBefore *big.Int
	lastBalanceAfter  *big.Int

	sequence    atomic.Int32
	submitOrder atomic.Int32
	feeOrder    atomic.Int32
}

func newVerticalSimulatedEVMService(t *testing.T) *verticalSimulatedEVMService {
	t.Helper()

	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(key.PublicKey)
	initialBalance := new(big.Int).Mul(big.NewInt(100), big.NewInt(1_000_000_000_000_000_000))
	backend := backends.NewSimulatedBackend(gethtypes.GenesisAlloc{
		from: {Balance: initialBalance},
	}, 30_000_000)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })

	return &verticalSimulatedEVMService{
		backend:  backend,
		key:      key,
		chainID:  big.NewInt(verticalSimulatedChainID),
		from:     from,
		feesByID: make(map[commontypes.IdempotencyKey]*big.Int),
	}
}

func (s *verticalSimulatedEVMService) newTransactor(t *testing.T) *bind.TransactOpts {
	t.Helper()
	auth, err := bind.NewKeyedTransactorWithChainID(s.key, s.chainID)
	require.NoError(t, err)
	auth.Context = t.Context()
	return auth
}

func (s *verticalSimulatedEVMService) mineBoundTransaction(t *testing.T, tx *gethtypes.Transaction) *gethtypes.Receipt {
	t.Helper()
	s.backend.Commit()
	receipt, err := s.backend.TransactionReceipt(t.Context(), tx.Hash())
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.Equal(t, uint64(1), receipt.Status)
	return receipt
}

func (s *verticalSimulatedEVMService) deployForwarder(t *testing.T) (common.Address, *forwarderbinding.KeystoneForwarder) {
	t.Helper()
	address, tx, instance, err := forwarderbinding.DeployKeystoneForwarder(s.newTransactor(t), s.backend)
	require.NoError(t, err)
	receipt := s.mineBoundTransaction(t, tx)
	require.Equal(t, address, receipt.ContractAddress)
	return address, instance
}

func (s *verticalSimulatedEVMService) deployReceiver(t *testing.T) common.Address {
	t.Helper()
	binHex := strings.TrimSpace(os.Getenv("AUDIT_RECEIVER_BIN"))
	require.NotEmpty(t, binHex, "AUDIT_RECEIVER_BIN must contain compiled AuditReceiver creation bytecode")
	bytecode, err := hex.DecodeString(strings.TrimPrefix(binHex, "0x"))
	require.NoError(t, err)
	emptyABI, err := abi.JSON(strings.NewReader("[]"))
	require.NoError(t, err)
	address, tx, _, err := bind.DeployContract(s.newTransactor(t), emptyABI, bytecode, s.backend)
	require.NoError(t, err)
	receipt := s.mineBoundTransaction(t, tx)
	require.Equal(t, address, receipt.ContractAddress)
	return address
}

func verticalBlockNumber(number *big.Int) *big.Int {
	if number == nil || number.Sign() < 0 {
		return nil
	}
	return new(big.Int).Set(number)
}

func verticalCallMsg(msg *evmtypes.CallMsg) ethereum.CallMsg {
	to := common.Address(msg.To)
	return ethereum.CallMsg{
		From: common.Address(msg.From),
		To:   &to,
		Data: append([]byte(nil), msg.Data...),
	}
}

func verticalConvertLog(log gethtypes.Log) *evmtypes.Log {
	topics := make([]evmtypes.Hash, len(log.Topics))
	for i, topic := range log.Topics {
		topics[i] = evmtypes.Hash(topic)
	}
	var eventSig evmtypes.Hash
	if len(topics) > 0 {
		eventSig = topics[0]
	}
	return &evmtypes.Log{
		LogIndex:    uint32(log.Index),
		BlockHash:   evmtypes.Hash(log.BlockHash),
		BlockNumber: new(big.Int).SetUint64(log.BlockNumber),
		Topics:      topics,
		EventSig:    eventSig,
		Address:     evmtypes.Address(log.Address),
		TxHash:      evmtypes.Hash(log.TxHash),
		Data:        append([]byte(nil), log.Data...),
		Removed:     log.Removed,
	}
}

func verticalConvertReceipt(receipt *gethtypes.Receipt) *evmtypes.Receipt {
	logs := make([]*evmtypes.Log, len(receipt.Logs))
	for i, log := range receipt.Logs {
		logs[i] = verticalConvertLog(*log)
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
	}
}

func (s *verticalSimulatedEVMService) BalanceAt(ctx context.Context, request evmtypes.BalanceAtRequest) (*evmtypes.BalanceAtReply, error) {
	balance, err := s.backend.BalanceAt(ctx, common.Address(request.Address), verticalBlockNumber(request.BlockNumber))
	if err != nil {
		return nil, err
	}
	return &evmtypes.BalanceAtReply{Balance: balance}, nil
}

func (s *verticalSimulatedEVMService) CallContract(ctx context.Context, request evmtypes.CallContractRequest) (*evmtypes.CallContractReply, error) {
	if request.Msg == nil {
		return nil, errors.New("nil call message")
	}
	data, err := s.backend.CallContract(ctx, verticalCallMsg(request.Msg), verticalBlockNumber(request.BlockNumber))
	if err != nil {
		return nil, err
	}
	return &evmtypes.CallContractReply{Data: data}, nil
}

func (s *verticalSimulatedEVMService) FilterLogs(ctx context.Context, request evmtypes.FilterLogsRequest) (*evmtypes.FilterLogsReply, error) {
	query := ethereum.FilterQuery{
		FromBlock: verticalBlockNumber(request.FilterQuery.FromBlock),
		ToBlock:   verticalBlockNumber(request.FilterQuery.ToBlock),
	}
	if request.FilterQuery.BlockHash != (evmtypes.Hash{}) {
		hash := common.Hash(request.FilterQuery.BlockHash)
		query.BlockHash = &hash
	}
	query.Addresses = make([]common.Address, len(request.FilterQuery.Addresses))
	for i, address := range request.FilterQuery.Addresses {
		query.Addresses[i] = common.Address(address)
	}
	query.Topics = make([][]common.Hash, len(request.FilterQuery.Topics))
	for i, alternatives := range request.FilterQuery.Topics {
		query.Topics[i] = make([]common.Hash, len(alternatives))
		for j, topic := range alternatives {
			query.Topics[i][j] = common.Hash(topic)
		}
	}
	logs, err := s.backend.FilterLogs(ctx, query)
	if err != nil {
		return nil, err
	}
	converted := make([]*evmtypes.Log, len(logs))
	for i, log := range logs {
		converted[i] = verticalConvertLog(log)
	}
	return &evmtypes.FilterLogsReply{Logs: converted}, nil
}

func (s *verticalSimulatedEVMService) HeaderByNumber(ctx context.Context, request evmtypes.HeaderByNumberRequest) (*evmtypes.HeaderByNumberReply, error) {
	header, err := s.backend.HeaderByNumber(ctx, verticalBlockNumber(request.Number))
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

func (s *verticalSimulatedEVMService) EstimateGas(ctx context.Context, call *evmtypes.CallMsg) (uint64, error) {
	if call == nil {
		return 0, errors.New("nil call message")
	}
	return s.backend.EstimateGas(ctx, verticalCallMsg(call))
}

func (s *verticalSimulatedEVMService) GetTransactionReceipt(ctx context.Context, request evmtypes.GeTransactionReceiptRequest) (*evmtypes.Receipt, error) {
	receipt, err := s.backend.TransactionReceipt(ctx, common.Hash(request.Hash))
	if err != nil {
		return nil, err
	}
	return verticalConvertReceipt(receipt), nil
}

func (s *verticalSimulatedEVMService) SubmitTransaction(ctx context.Context, request evmtypes.SubmitTransactionRequest) (*evmtypes.TransactionResult, error) {
	gasLimit := uint64(5_000_000)
	if request.GasConfig != nil && request.GasConfig.GasLimit != nil {
		gasLimit = *request.GasConfig.GasLimit
	}

	nonce, err := s.backend.PendingNonceAt(ctx, s.from)
	if err != nil {
		return nil, fmt.Errorf("pending nonce: %w", err)
	}
	tipCap, err := s.backend.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("suggest gas tip cap: %w", err)
	}
	header, err := s.backend.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("latest header: %w", err)
	}
	if header.BaseFee == nil {
		return nil, errors.New("latest header has nil base fee")
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(header.BaseFee, big.NewInt(2)), tipCap)
	if request.GasConfig != nil && request.GasConfig.MaxGasPrice != nil && feeCap.Cmp(request.GasConfig.MaxGasPrice) > 0 {
		feeCap = new(big.Int).Set(request.GasConfig.MaxGasPrice)
	}

	before, err := s.backend.BalanceAt(ctx, s.from, nil)
	if err != nil {
		return nil, fmt.Errorf("balance before report: %w", err)
	}
	to := common.Address(request.To)
	tx := gethtypes.NewTx(&gethtypes.DynamicFeeTx{
		ChainID:   s.chainID,
		Nonce:     nonce,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       gasLimit,
		To:        &to,
		Value:     big.NewInt(0),
		Data:      append([]byte(nil), request.Data...),
	})
	signed, err := gethtypes.SignTx(tx, gethtypes.LatestSignerForChainID(s.chainID), s.key)
	if err != nil {
		return nil, fmt.Errorf("sign report transaction: %w", err)
	}
	if err := s.backend.SendTransaction(ctx, signed); err != nil {
		return nil, fmt.Errorf("send report transaction: %w", err)
	}
	s.backend.Commit()
	receipt, err := s.backend.TransactionReceipt(ctx, signed.Hash())
	if err != nil {
		return nil, fmt.Errorf("report receipt: %w", err)
	}
	if receipt.EffectiveGasPrice == nil {
		return nil, errors.New("report receipt has nil effective gas price")
	}
	after, err := s.backend.BalanceAt(ctx, s.from, nil)
	if err != nil {
		return nil, fmt.Errorf("balance after report: %w", err)
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	idempotencyKey := commontypes.IdempotencyKey(signed.Hash().Hex())

	s.mu.Lock()
	s.feesByID[idempotencyKey] = new(big.Int).Set(fee)
	s.lastReceipt = receipt
	s.lastBalanceBefore = before
	s.lastBalanceAfter = after
	s.mu.Unlock()
	s.submitOrder.Store(s.sequence.Add(1))

	status := evmtypes.TxSuccess
	if receipt.Status == 0 {
		status = evmtypes.TxReverted
	}
	return &evmtypes.TransactionResult{
		TxStatus:         status,
		TxHash:           evmtypes.Hash(signed.Hash()),
		TxIdempotencyKey: idempotencyKey,
	}, nil
}

func (s *verticalSimulatedEVMService) GetTransactionFee(_ context.Context, transactionID commontypes.IdempotencyKey) (*evmtypes.TransactionFee, error) {
	s.feeOrder.Store(s.sequence.Add(1))
	s.mu.Lock()
	defer s.mu.Unlock()
	fee, ok := s.feesByID[transactionID]
	if !ok {
		return nil, fmt.Errorf("transaction fee not found for %s", transactionID)
	}
	return &evmtypes.TransactionFee{TransactionFee: new(big.Int).Set(fee)}, nil
}

func (s *verticalSimulatedEVMService) CalculateTransactionFee(_ context.Context, info evmtypes.ReceiptGasInfo) (*evmtypes.TransactionFee, error) {
	if info.EffectiveGasPrice == nil {
		return nil, errors.New("nil effective gas price")
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(info.GasUsed), info.EffectiveGasPrice)
	if info.L1Fee != nil {
		fee.Add(fee, info.L1Fee)
	}
	return &evmtypes.TransactionFee{TransactionFee: fee}, nil
}

type fullVerticalReportExecutable struct {
	t               *testing.T
	inner           commoncap.ExecutableCapability
	service         *verticalSimulatedEVMService
	forwarder       *forwarderbinding.KeystoneForwarder
	receiver        common.Address
	signerKeys      []*ecdsa.PrivateKey
	signerAddresses []common.Address
	spendLimitsCh   chan []commoncap.SpendLimit
	executionIDCh   chan [32]byte
	generatedCalls  atomic.Int32
	configureOnce   sync.Once
	configureErr    error
}

var _ commoncap.ExecutableCapability = (*fullVerticalReportExecutable)(nil)

func (p *fullVerticalReportExecutable) Info(ctx context.Context) (commoncap.CapabilityInfo, error) {
	return p.inner.Info(ctx)
}

func (p *fullVerticalReportExecutable) RegisterToWorkflow(ctx context.Context, req commoncap.RegisterToWorkflowRequest) error {
	return p.inner.RegisterToWorkflow(ctx, req)
}

func (p *fullVerticalReportExecutable) UnregisterFromWorkflow(ctx context.Context, req commoncap.UnregisterFromWorkflowRequest) error {
	return p.inner.UnregisterFromWorkflow(ctx, req)
}

func (p *fullVerticalReportExecutable) Execute(ctx context.Context, req commoncap.CapabilityRequest) (commoncap.CapabilityResponse, error) {
	p.generatedCalls.Add(1)
	p.spendLimitsCh <- append([]commoncap.SpendLimit(nil), req.Metadata.SpendLimits...)

	p.configureOnce.Do(func() {
		tx, err := p.forwarder.SetConfig(
			p.service.newTransactor(p.t),
			req.Metadata.WorkflowDonID,
			req.Metadata.WorkflowDonConfigVersion,
			uint8(1),
			p.signerAddresses,
		)
		if err != nil {
			p.configureErr = fmt.Errorf("configure real forwarder: %w", err)
			return
		}
		p.service.mineBoundTransaction(p.t, tx)
	})
	if p.configureErr != nil {
		return commoncap.CapabilityResponse{}, p.configureErr
	}

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
	reportContext := make([]byte, 96)
	report := &sdkpb.ReportResponse{
		RawReport:     rawReport,
		ReportContext: reportContext,
	}
	rawHash := crypto.Keccak256Hash(report.RawReport)
	completeHash := crypto.Keccak256Hash(rawHash.Bytes(), report.ReportContext)
	for _, signerKey := range p.signerKeys[:2] {
		signature, signErr := crypto.Sign(completeHash.Bytes(), signerKey)
		if signErr != nil {
			return commoncap.CapabilityResponse{}, fmt.Errorf("sign report: %w", signErr)
		}
		report.Sigs = append(report.Sigs, &sdkpb.AttributedSignature{Signature: signature})
	}

	executionBytes, err := hex.DecodeString(strings.TrimPrefix(req.Metadata.WorkflowExecutionID, "0x"))
	if err != nil {
		return commoncap.CapabilityResponse{}, fmt.Errorf("decode workflow execution ID: %w", err)
	}
	if len(executionBytes) != 32 {
		return commoncap.CapabilityResponse{}, fmt.Errorf("workflow execution ID has %d bytes, want 32", len(executionBytes))
	}
	var executionID [32]byte
	copy(executionID[:], executionBytes)
	p.executionIDCh <- executionID

	payload, err := anypb.New(&evmpb.WriteReportRequest{
		Receiver:  p.receiver.Bytes(),
		Report:    report,
		GasConfig: &evmpb.GasConfig{GasLimit: 1_500_000},
	})
	if err != nil {
		return commoncap.CapabilityResponse{}, fmt.Errorf("wrap WriteReport request: %w", err)
	}
	req.Payload = payload
	return p.inner.Execute(ctx, req)
}

// TestPoC_FullVerticalRealEVMWriteReportExceedsReservation crosses every
// open-source production boundary in one execution:
//
// Workflow Engine -> generated EVM wrapper -> production WriteReport action ->
// real CREForwarderClient -> deployed KeystoneForwarder -> signed report ->
// deployed ERC-165 receiver -> mined receipt -> actual gas fee -> Engine Settle
// -> Billing receipt.
//
// Only the private Billing Platform server is mocked. The test proves that a
// successful one-microcredit reservation does not cap the real, irreversible
// transmitter gas spend and does not prevent successful workflow completion.
func TestPoC_FullVerticalRealEVMWriteReportExceedsReservation(t *testing.T) {
	const (
		reserved      = "0.000001"
		chainSelector = uint64(12922642891491394802)
		capabilityID  = "evm:ChainSelector:12922642891491394802@1.0.0"
	)

	service := newVerticalSimulatedEVMService(t)
	forwarderAddress, forwarderContract := service.deployForwarder(t)
	receiverAddress := service.deployReceiver(t)

	signerKeys := make([]*ecdsa.PrivateKey, 4)
	signerAddresses := make([]common.Address, 4)
	for i := range signerKeys {
		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		signerKeys[i] = key
		signerAddresses[i] = crypto.PubkeyToAddress(key.PublicKey)
	}

	lggr := logger.Test(t)
	realEVM, err := evmactions.NewEVM(
		evmconfig.Config{
			CREForwarderAddress: forwarderAddress.Hex(),
			ReceiverGasMinimum:  1_000,
		},
		service,
		lggr,
		captest.NopBeholderProcessor{},
		evmmonitoring.NewMessageBuilder(commontypes.ChainInfo{}, commoncap.CapabilityInfo{}, ""),
		nil,
		chainSelector,
		limits.Factory{Logger: lggr},
		ts.TransmissionScheduler{},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, realEVM.Close()) })

	client := &realEVMClientCapability{EVM: realEVM, chainSelector: chainSelector}
	generated := evmserver.NewClientServer(client)
	info, err := generated.Info(t.Context())
	require.NoError(t, err)
	require.True(t, info.IsLocal)
	require.Empty(t, info.SpendTypes, "generated EVM wrapper must advertise no GAS spend type")

	spendLimitsCh := make(chan []commoncap.SpendLimit, 1)
	executionIDCh := make(chan [32]byte, 1)
	probe := &fullVerticalReportExecutable{
		t:               t,
		inner:           generated,
		service:         service,
		forwarder:       forwarderContract,
		receiver:        receiverAddress,
		signerKeys:      signerKeys,
		signerAddresses: signerAddresses,
		spendLimitsCh:   spendLimitsCh,
		executionIDCh:   executionIDCh,
	}

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

	billingReceiptCh := make(chan *billing.SubmitWorkflowReceiptRequest, 1)
	var billingReceiptOrder atomic.Int32
	billingClient.EXPECT().
		SubmitWorkflowReceipt(mock.Anything, mock.Anything).
		Run(func(_ context.Context, req *billing.SubmitWorkflowReceiptRequest) {
			billingReceiptOrder.Store(service.sequence.Add(1))
			billingReceiptCh <- req
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
		OnInitialized:          func(initErr error) { initDoneCh <- initErr },
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
		ID:          "full_vertical_real_evm_overspend_poc",
	}}

	require.NoError(t, <-runErrCh)
	select {
	case <-finishedCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for workflow execution")
	}

	select {
	case limitsSeen := <-spendLimitsCh:
		require.Empty(t, limitsSeen, "real generated wrapper was invoked without a GAS spend limit")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for generated wrapper invocation")
	}

	var executionID [32]byte
	select {
	case executionID = <-executionIDCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for report execution ID")
	}

	var billingReceipt *billing.SubmitWorkflowReceiptRequest
	select {
	case billingReceipt = <-billingReceiptCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for billing receipt")
	}

	consumed, ok := new(big.Rat).SetString(billingReceipt.CreditsConsumed)
	require.True(t, ok, "invalid consumed credit amount %q", billingReceipt.CreditsConsumed)
	reservedAmount, ok := new(big.Rat).SetString(reserved)
	require.True(t, ok, "invalid reserved credit amount %q", reserved)
	require.Greater(t, consumed.Cmp(reservedAmount), 0,
		"actual mined spend %s must exceed reservation %s", billingReceipt.CreditsConsumed, reserved)

	service.mu.Lock()
	receipt := service.lastReceipt
	before := new(big.Int).Set(service.lastBalanceBefore)
	after := new(big.Int).Set(service.lastBalanceAfter)
	service.mu.Unlock()
	require.NotNil(t, receipt)
	require.Equal(t, uint64(1), receipt.Status)
	require.Positive(t, receipt.GasUsed)
	require.NotNil(t, receipt.EffectiveGasPrice)
	feeWei := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	require.Equal(t, feeWei, new(big.Int).Sub(before, after),
		"transmitter balance loss must equal the mined report transaction fee")

	storage, err := service.backend.StorageAt(t.Context(), receiverAddress, common.Hash{}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), new(big.Int).SetBytes(storage).Int64(),
		"receiver state must change before the over-reservation billing receipt")

	transmissionInfo, err := forwarderContract.GetTransmissionInfo(
		&bind.CallOpts{Context: t.Context()},
		receiverAddress,
		executionID,
		[2]byte{0x00, 0x01},
	)
	require.NoError(t, err)
	require.Equal(t, uint8(1), transmissionInfo.State)
	require.True(t, transmissionInfo.Success)
	require.False(t, transmissionInfo.InvalidReceiver)
	require.NotEqual(t, common.Address{}, transmissionInfo.Transmitter)

	require.Equal(t, int32(1), probe.generatedCalls.Load())
	require.Positive(t, service.submitOrder.Load())
	require.Greater(t, service.feeOrder.Load(), service.submitOrder.Load(),
		"actual fee must be learned after the real transaction is mined")
	require.Greater(t, billingReceiptOrder.Load(), service.feeOrder.Load(),
		"billing receipt must be emitted after irreversible gas spend is known")

	t.Logf(
		"full vertical EVM overspend reproduced: reserved=%s consumed=%s tx=%s gasUsed=%d effectiveGasPrice=%s feeWei=%s receiverSlot0=1 transmissionState=%d order submit=%d fee=%d billing=%d",
		reserved,
		billingReceipt.CreditsConsumed,
		receipt.TxHash.Hex(),
		receipt.GasUsed,
		receipt.EffectiveGasPrice,
		feeWei,
		transmissionInfo.State,
		service.submitOrder.Load(),
		service.feeOrder.Load(),
		billingReceiptOrder.Load(),
	)
}

var _ commontypes.EVMService = (*verticalSimulatedEVMService)(nil)
