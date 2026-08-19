package actions

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
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/abi/bind/backends"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	evmcap "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	cltypes "github.com/smartcontractkit/chainlink-common/pkg/types"
	evmtypes "github.com/smartcontractkit/chainlink-common/pkg/types/chains/evm"
	forwarderbinding "github.com/smartcontractkit/chainlink-evm/gethwrappers/keystone/generated/forwarder"
	workflowpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"
	p2ptypes "github.com/smartcontractkit/libocr/ragep2p/types"

	ts "github.com/smartcontractkit/capabilities/chain_capabilities/common/transmission_schedule"
	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/internal/contracts"
)

const fullForwarderChainID = int64(1337)

type fullSimulatedEVMService struct {
	// Promote the methods that are irrelevant to this focused integration test.
	// Every method reached by production WriteReport/CREForwarderClient is
	// implemented explicitly below and therefore never dispatches through this
	// nil embedded interface.
	cltypes.EVMService

	backend *backends.SimulatedBackend
	key     *ecdsa.PrivateKey
	chainID *big.Int
	from    common.Address

	mu               sync.Mutex
	feesByID         map[string]*big.Int
	lastReceipt      *gethtypes.Receipt
	lastBalanceBefore *big.Int
	lastBalanceAfter  *big.Int
}

func newFullSimulatedEVMService(t *testing.T) *fullSimulatedEVMService {
	t.Helper()

	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(key.PublicKey)
	initialBalance := new(big.Int).Mul(big.NewInt(100), big.NewInt(1_000_000_000_000_000_000))
	backend := backends.NewSimulatedBackend(gethtypes.GenesisAlloc{
		from: {Balance: initialBalance},
	}, 30_000_000)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })

	return &fullSimulatedEVMService{
		backend:   backend,
		key:       key,
		chainID:   big.NewInt(fullForwarderChainID),
		from:      from,
		feesByID:  make(map[string]*big.Int),
	}
}

func (s *fullSimulatedEVMService) newTransactor(t *testing.T) *bind.TransactOpts {
	t.Helper()
	auth, err := bind.NewKeyedTransactorWithChainID(s.key, s.chainID)
	require.NoError(t, err)
	auth.Context = t.Context()
	return auth
}

func (s *fullSimulatedEVMService) mineBoundTransaction(t *testing.T, tx *gethtypes.Transaction) *gethtypes.Receipt {
	t.Helper()
	s.backend.Commit()
	receipt, err := s.backend.TransactionReceipt(t.Context(), tx.Hash())
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.Equal(t, uint64(1), receipt.Status)
	return receipt
}

func (s *fullSimulatedEVMService) deployForwarder(t *testing.T) (common.Address, *forwarderbinding.KeystoneForwarder) {
	t.Helper()
	address, tx, instance, err := forwarderbinding.DeployKeystoneForwarder(s.newTransactor(t), s.backend)
	require.NoError(t, err)
	receipt := s.mineBoundTransaction(t, tx)
	require.Equal(t, address, receipt.ContractAddress)
	return address, instance
}

func (s *fullSimulatedEVMService) deployAuditReceiver(t *testing.T) common.Address {
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

func fullBlockNumber(number *big.Int) *big.Int {
	if number == nil || number.Sign() < 0 {
		return nil
	}
	return new(big.Int).Set(number)
}

func fullCallMsg(msg *evmtypes.CallMsg) ethereum.CallMsg {
	to := common.Address(msg.To)
	return ethereum.CallMsg{
		From: common.Address(msg.From),
		To:   &to,
		Data: append([]byte(nil), msg.Data...),
	}
}

func fullConvertLog(log gethtypes.Log) *evmtypes.Log {
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

func fullConvertReceipt(receipt *gethtypes.Receipt) *evmtypes.Receipt {
	logs := make([]*evmtypes.Log, len(receipt.Logs))
	for i, log := range receipt.Logs {
		logs[i] = fullConvertLog(*log)
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

func (s *fullSimulatedEVMService) BalanceAt(ctx context.Context, request evmtypes.BalanceAtRequest) (*evmtypes.BalanceAtReply, error) {
	balance, err := s.backend.BalanceAt(ctx, common.Address(request.Address), fullBlockNumber(request.BlockNumber))
	if err != nil {
		return nil, err
	}
	return &evmtypes.BalanceAtReply{Balance: balance}, nil
}

func (s *fullSimulatedEVMService) CallContract(ctx context.Context, request evmtypes.CallContractRequest) (*evmtypes.CallContractReply, error) {
	if request.Msg == nil {
		return nil, errors.New("nil call message")
	}
	data, err := s.backend.CallContract(ctx, fullCallMsg(request.Msg), fullBlockNumber(request.BlockNumber))
	if err != nil {
		return nil, err
	}
	return &evmtypes.CallContractReply{Data: data}, nil
}

func (s *fullSimulatedEVMService) FilterLogs(ctx context.Context, request evmtypes.FilterLogsRequest) (*evmtypes.FilterLogsReply, error) {
	query := ethereum.FilterQuery{
		FromBlock: fullBlockNumber(request.FilterQuery.FromBlock),
		ToBlock:   fullBlockNumber(request.FilterQuery.ToBlock),
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
		converted[i] = fullConvertLog(log)
	}
	return &evmtypes.FilterLogsReply{Logs: converted}, nil
}

func (s *fullSimulatedEVMService) HeaderByNumber(ctx context.Context, request evmtypes.HeaderByNumberRequest) (*evmtypes.HeaderByNumberReply, error) {
	header, err := s.backend.HeaderByNumber(ctx, fullBlockNumber(request.Number))
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

func (s *fullSimulatedEVMService) EstimateGas(ctx context.Context, call *evmtypes.CallMsg) (uint64, error) {
	if call == nil {
		return 0, errors.New("nil call message")
	}
	return s.backend.EstimateGas(ctx, fullCallMsg(call))
}

func (s *fullSimulatedEVMService) GetTransactionReceipt(ctx context.Context, request evmtypes.GeTransactionReceiptRequest) (*evmtypes.Receipt, error) {
	receipt, err := s.backend.TransactionReceipt(ctx, common.Hash(request.Hash))
	if err != nil {
		return nil, err
	}
	return fullConvertReceipt(receipt), nil
}

func (s *fullSimulatedEVMService) SubmitTransaction(ctx context.Context, request evmtypes.SubmitTransactionRequest) (*evmtypes.TransactionResult, error) {
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
	idempotencyKey := signed.Hash().Hex()

	s.mu.Lock()
	s.feesByID[idempotencyKey] = new(big.Int).Set(fee)
	s.lastReceipt = receipt
	s.lastBalanceBefore = before
	s.lastBalanceAfter = after
	s.mu.Unlock()

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

func (s *fullSimulatedEVMService) GetTransactionFee(_ context.Context, transactionID cltypes.IdempotencyKey) (*evmtypes.TransactionFee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fee, ok := s.feesByID[transactionID]
	if !ok {
		return nil, fmt.Errorf("transaction fee not found for %s", transactionID)
	}
	return &evmtypes.TransactionFee{TransactionFee: new(big.Int).Set(fee)}, nil
}

func (s *fullSimulatedEVMService) CalculateTransactionFee(_ context.Context, info evmtypes.ReceiptGasInfo) (*evmtypes.TransactionFee, error) {
	if info.EffectiveGasPrice == nil {
		return nil, errors.New("nil effective gas price")
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(info.GasUsed), info.EffectiveGasPrice)
	if info.L1Fee != nil {
		fee.Add(fee, info.L1Fee)
	}
	return &evmtypes.TransactionFee{TransactionFee: fee}, nil
}

func fullSignReport(t *testing.T, report *workflowpb.ReportResponse, signerKeys []*ecdsa.PrivateKey) {
	t.Helper()
	require.GreaterOrEqual(t, len(signerKeys), 2)
	rawHash := crypto.Keccak256Hash(report.RawReport)
	completeHash := crypto.Keccak256Hash(rawHash.Bytes(), report.ReportContext)
	report.Sigs = make([]*workflowpb.AttributedSignature, 0, 2)
	for _, signerKey := range signerKeys[:2] {
		signature, err := crypto.Sign(completeHash.Bytes(), signerKey)
		require.NoError(t, err)
		require.Len(t, signature, 65)
		require.LessOrEqual(t, signature[64], byte(1))
		report.Sigs = append(report.Sigs, &workflowpb.AttributedSignature{Signature: signature})
	}
}

func TestPoC_RealKeystoneForwarderChargesGasWithoutSpendLimit(t *testing.T) {
	serviceBackend := newFullSimulatedEVMService(t)
	forwarderAddress, forwarderContract := serviceBackend.deployForwarder(t)
	receiverAddress := serviceBackend.deployAuditReceiver(t)

	signerKeys := make([]*ecdsa.PrivateKey, 4)
	signerAddresses := make([]common.Address, 4)
	for i := range signerKeys {
		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		signerKeys[i] = key
		signerAddresses[i] = crypto.PubkeyToAddress(key.PublicKey)
	}
	configTx, err := forwarderContract.SetConfig(
		serviceBackend.newTransactor(t),
		uint32(10),
		uint32(2),
		uint8(1),
		signerAddresses,
	)
	require.NoError(t, err)
	serviceBackend.mineBoundTransaction(t, configTx)

	fixture := newWriteReportTestFixture(t)
	fixture.receiver = receiverAddress
	fixture.request.Receiver = receiverAddress.Bytes()
	fixture.request.GasConfig = &evmcap.GasConfig{GasLimit: 1_500_000}
	fullSignReport(t, fixture.request.Report, signerKeys)
	require.Empty(t, fixture.metadata.SpendLimits,
		"the real-forwarder WriteReport call must enter without a GAS spend limit")

	lggr := logger.Test(t)
	forwarderClient, err := contracts.NewCREForwarderClient(
		serviceBackend,
		forwarderAddress,
		contracts.DefaultLookbackBlocks,
		lggr,
	)
	require.NoError(t, err)
	var peerID p2ptypes.PeerID
	peerID[0] = 1
	scheduler := ts.NewTransmissionScheduler(
		peerID,
		[]p2ptypes.PeerID{peerID},
		time.Millisecond,
		0,
		lggr,
	)
	capability := createMocksAndCapabilityWithScheduler(t, lggr, serviceBackend, forwarderClient, scheduler)
	capability.keystoneForwarderAddress = forwarderAddress

	result, capErr := capability.WriteReport(t.Context(), fixture.metadata, fixture.request)
	require.NoError(t, capErr)
	require.NotNil(t, result)
	require.NotNil(t, result.Response)
	require.Equal(t, evmcap.TxStatus_TX_STATUS_SUCCESS, result.Response.TxStatus)

	serviceBackend.mu.Lock()
	receipt := serviceBackend.lastReceipt
	before := new(big.Int).Set(serviceBackend.lastBalanceBefore)
	after := new(big.Int).Set(serviceBackend.lastBalanceAfter)
	serviceBackend.mu.Unlock()
	require.NotNil(t, receipt)
	require.Equal(t, uint64(1), receipt.Status)
	require.Positive(t, receipt.GasUsed)
	require.NotNil(t, receipt.EffectiveGasPrice)
	feeWei := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	require.Equal(t, feeWei, new(big.Int).Sub(before, after),
		"the transmitter balance loss must equal the mined real-forwarder transaction fee")

	storage, err := serviceBackend.backend.StorageAt(t.Context(), receiverAddress, common.Hash{}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), new(big.Int).SetBytes(storage).Int64(),
		"the real ERC-165 receiver must persist its onReport state change")

	transmissionInfo, err := forwarderContract.GetTransmissionInfo(
		&bind.CallOpts{Context: t.Context()},
		receiverAddress,
		fixture.transmissionID.WorkflowExecutionID,
		fixture.transmissionID.ReportID,
	)
	require.NoError(t, err)
	require.Equal(t, uint8(contracts.TransmissionStateSucceeded), transmissionInfo.State)
	require.True(t, transmissionInfo.Success)
	require.False(t, transmissionInfo.InvalidReceiver)
	require.NotEqual(t, common.Address{}, transmissionInfo.Transmitter)

	require.Len(t, result.ResponseMetadata.Metering, 1)
	metering := result.ResponseMetadata.Metering[0]
	require.Equal(t, "GAS.1", metering.SpendUnit)
	expectedMetered := new(big.Float).
		Quo(new(big.Float).SetInt(feeWei), big.NewFloat(1e18)).
		Text('f', -1)
	require.Equal(t, expectedMetered, metering.SpendValue)

	t.Logf(
		"real KeystoneForwarder spend reproduced: tx=%s gasUsed=%d effectiveGasPrice=%s feeWei=%s metered=%s receiverSlot0=1 transmissionState=%d",
		receipt.TxHash.Hex(),
		receipt.GasUsed,
		receipt.EffectiveGasPrice,
		feeWei,
		metering.SpendValue,
		transmissionInfo.State,
	)
}

var _ cltypes.EVMService = (*fullSimulatedEVMService)(nil)
var _ = capabilities.RequestMetadata{}
