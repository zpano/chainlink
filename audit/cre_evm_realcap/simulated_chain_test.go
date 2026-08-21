package realcap_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/abi/bind/backends"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	commontest "github.com/smartcontractkit/capabilities/chain_capabilities/common/test"
	ts "github.com/smartcontractkit/capabilities/chain_capabilities/common/transmission_schedule"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/actions"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/config"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/monitoring"
	"github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	ocrtypes "github.com/smartcontractkit/chainlink-common/pkg/capabilities/consensus/ocr3/types"
	evmcap "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/settings/limits"
	cltypes "github.com/smartcontractkit/chainlink-common/pkg/types"
	evmtypes "github.com/smartcontractkit/chainlink-common/pkg/types/chains/evm"
	mockforwarder "github.com/smartcontractkit/chainlink-evm/gethwrappers/keystone/generated/mock_forwarder"
	reservemanager "github.com/smartcontractkit/chainlink-evm/gethwrappers/workflow/generated/reserve_manager"
	workflowpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"
)

type simulatedChainEVMService struct {
	cltypes.UnimplementedEVMService

	backend          *backends.SimulatedBackend
	auth             bind.TransactOpts
	forwarderAddress common.Address
	forwarderABI     abi.ABI

	mu       sync.Mutex
	receipts map[string]*types.Receipt
	lastFee  *big.Int
}

func newSimulatedChainEVMService(
	backend *backends.SimulatedBackend,
	auth *bind.TransactOpts,
	forwarderAddress common.Address,
) (*simulatedChainEVMService, error) {
	forwarderABI, err := mockforwarder.MockKeystoneForwarderMetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return &simulatedChainEVMService{
		backend:          backend,
		auth:             *auth,
		forwarderAddress: forwarderAddress,
		forwarderABI:     *forwarderABI,
		receipts:         make(map[string]*types.Receipt),
	}, nil
}

func positiveBlock(number *big.Int) *big.Int {
	if number == nil || number.Sign() < 0 {
		return nil
	}
	return number
}

func (s *simulatedChainEVMService) CallContract(ctx context.Context, req evmtypes.CallContractRequest) (*evmtypes.CallContractReply, error) {
	if req.Msg == nil {
		return nil, fmt.Errorf("call message is nil")
	}
	to := common.Address(req.Msg.To)
	data, err := s.backend.CallContract(ctx, ethereum.CallMsg{
		From: common.Address(req.Msg.From),
		To:   &to,
		Data: req.Msg.Data,
	}, positiveBlock(req.BlockNumber))
	if err != nil {
		return nil, err
	}
	return &evmtypes.CallContractReply{Data: data}, nil
}

func (s *simulatedChainEVMService) SubmitTransaction(ctx context.Context, req evmtypes.SubmitTransactionRequest) (*evmtypes.TransactionResult, error) {
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
	s.lastFee = transactionFeeFromReceipt(receipt)
	s.mu.Unlock()

	status := evmtypes.TxReverted
	if receipt.Status == types.ReceiptStatusSuccessful {
		status = evmtypes.TxSuccess
	}
	return &evmtypes.TransactionResult{
		TxStatus:         status,
		TxHash:           evmtypes.Hash(tx.Hash()),
		TxIdempotencyKey: idempotencyKey,
	}, nil
}

func transactionFeeFromReceipt(receipt *types.Receipt) *big.Int {
	if receipt == nil || receipt.EffectiveGasPrice == nil {
		return new(big.Int)
	}
	return new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
}

func (s *simulatedChainEVMService) GetTransactionFee(_ context.Context, transactionID cltypes.IdempotencyKey) (*evmtypes.TransactionFee, error) {
	s.mu.Lock()
	receipt := s.receipts[transactionID]
	s.mu.Unlock()
	if receipt == nil {
		return nil, fmt.Errorf("unknown transaction id %q", transactionID)
	}
	return &evmtypes.TransactionFee{TransactionFee: transactionFeeFromReceipt(receipt)}, nil
}

func (s *simulatedChainEVMService) HeaderByNumber(ctx context.Context, req evmtypes.HeaderByNumberRequest) (*evmtypes.HeaderByNumberReply, error) {
	header, err := s.backend.HeaderByNumber(ctx, positiveBlock(req.Number))
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

func (s *simulatedChainEVMService) FilterLogs(ctx context.Context, req evmtypes.FilterLogsRequest) (*evmtypes.FilterLogsReply, error) {
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
		reply.Logs = append(reply.Logs, convertGethLog(logs[i]))
	}
	return reply, nil
}

func convertGethLog(log types.Log) *evmtypes.Log {
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

func (s *simulatedChainEVMService) GetTransactionReceipt(ctx context.Context, req evmtypes.GeTransactionReceiptRequest) (*evmtypes.Receipt, error) {
	receipt, err := s.backend.TransactionReceipt(ctx, common.Hash(req.Hash))
	if err != nil {
		return nil, err
	}
	logs := make([]*evmtypes.Log, 0, len(receipt.Logs))
	for _, log := range receipt.Logs {
		logs = append(logs, convertGethLog(*log))
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

func (s *simulatedChainEVMService) CalculateTransactionFee(_ context.Context, info evmtypes.ReceiptGasInfo) (*evmtypes.TransactionFee, error) {
	if info.EffectiveGasPrice == nil {
		return nil, fmt.Errorf("effective gas price is nil")
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(info.GasUsed), info.EffectiveGasPrice)
	if info.L1Fee != nil {
		fee.Add(fee, info.L1Fee)
	}
	return &evmtypes.TransactionFee{TransactionFee: fee}, nil
}

func (s *simulatedChainEVMService) recordedFee() *big.Int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastFee == nil {
		return nil
	}
	return new(big.Int).Set(s.lastFee)
}

func TestRealEVMWriteReportChangesOnchainStateAndPaysGasBeyondSpendLimit(t *testing.T) {
	privateKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	initialBalance := new(big.Int).Exp(big.NewInt(10), big.NewInt(25), nil)
	backend := backends.NewSimulatedBackend(types.GenesisAlloc{
		auth.From: {Balance: initialBalance},
	}, 30_000_000)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })

	forwarderAddress, _, _, err := mockforwarder.DeployMockKeystoneForwarder(auth, backend)
	require.NoError(t, err)
	backend.Commit()

	reserveAddress, _, reserveManager, err := reservemanager.DeployReserveManager(auth, backend)
	require.NoError(t, err)
	backend.Commit()

	service, err := newSimulatedChainEVMService(backend, auth, forwarderAddress)
	require.NoError(t, err)
	lggr := logger.Test(t)

	evmCapability, capErr := actions.NewEVM(
		config.Config{
			CREForwarderAddress:    forwarderAddress.Hex(),
			ReceiverGasMinimum:     1_000,
			ForwarderLookbackBlocks: 100,
		},
		service,
		lggr,
		commontest.NopBeholderProcessor{},
		monitoring.NewMessageBuilder(cltypes.ChainInfo{}, capabilities.CapabilityInfo{}, ""),
		nil,
		chainSelector,
		limits.Factory{Logger: lggr},
		ts.TransmissionScheduler{},
	)
	require.NoError(t, capErr)
	t.Cleanup(func() { require.NoError(t, evmCapability.Close()) })

	reportMetadata := ocrtypes.Metadata{
		Version:          1,
		ExecutionID:      strings.Repeat("61", 32),
		Timestamp:        1_000,
		DONID:            10,
		DONConfigVersion: 2,
		WorkflowID:       strings.Repeat("62", 32),
		WorkflowName:     strings.Repeat("63", 10),
		WorkflowOwner:    strings.Repeat("64", 20),
		ReportID:         strings.Repeat("65", 2),
	}
	encodedMetadata, err := reportMetadata.Encode()
	require.NoError(t, err)
	uint256Type, err := abi.NewType("uint256", "", nil)
	require.NoError(t, err)
	payload, err := (abi.Arguments{{Type: uint256Type}, {Type: uint256Type}}).Pack(big.NewInt(123), big.NewInt(456))
	require.NoError(t, err)
	rawReport := append(append([]byte(nil), encodedMetadata...), payload...)

	spendLimit := "0.000000000000000001" // one wei, intentionally below the real tx fee
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
			Limit:     spendLimit,
		}},
	}

	balanceBefore, err := backend.BalanceAt(t.Context(), auth.From, nil)
	require.NoError(t, err)
	response, writeErr := evmCapability.WriteReport(t.Context(), metadata, &evmcap.WriteReportRequest{
		Receiver: reserveAddress.Bytes(),
		Report: &workflowpb.ReportResponse{
			RawReport:     rawReport,
			ReportContext: []byte{},
			Sigs: []*workflowpb.AttributedSignature{{
				Signature: []byte{1, 2, 3, 4},
			}},
		},
		GasConfig: &evmcap.GasConfig{GasLimit: 1_000_000},
	})
	require.NoError(t, writeErr)
	balanceAfter, err := backend.BalanceAt(t.Context(), auth.From, nil)
	require.NoError(t, err)

	minted, err := reserveManager.LastTotalMinted(&bind.CallOpts{Context: t.Context()})
	require.NoError(t, err)
	reserve, err := reserveManager.LastTotalReserve(&bind.CallOpts{Context: t.Context()})
	require.NoError(t, err)
	require.Equal(t, int64(123), minted.Int64())
	require.Equal(t, int64(456), reserve.Int64())

	require.Len(t, response.ResponseMetadata.Metering, 1)
	metering := response.ResponseMetadata.Metering[0]
	actualSpend, ok := new(big.Rat).SetString(metering.SpendValue)
	require.True(t, ok)
	allowedSpend, ok := new(big.Rat).SetString(spendLimit)
	require.True(t, ok)
	require.Greater(t, actualSpend.Cmp(allowedSpend), 0,
		"actual on-chain fee must exceed the supplied GAS spend limit")

	paid := new(big.Int).Sub(balanceBefore, balanceAfter)
	recordedFee := service.recordedFee()
	require.NotNil(t, recordedFee)
	require.Positive(t, paid.Sign())
	require.Equal(t, 0, paid.Cmp(recordedFee),
		"the transmitter balance loss must equal the receipt-derived fee")
	require.Greater(t, paid.Cmp(big.NewInt(1)), 0,
		"the transmitter paid more than the one-wei spend limit")

	t.Logf("simulated-chain overspend reproduced: limit=%s native actual=%s native feeWei=%s receiverState=(%s,%s)",
		spendLimit, metering.SpendValue, paid.String(), minted.String(), reserve.String())
}
