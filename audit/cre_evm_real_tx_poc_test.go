package actions

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi/bind/backends"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	evmcap "github.com/smartcontractkit/chainlink-common/pkg/capabilities/v2/chain-capabilities/evm"
	evmtypes "github.com/smartcontractkit/chainlink-common/pkg/types/chains/evm"
	workflowpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"

	"github.com/smartcontractkit/capabilities/chain_capabilities/evm/internal/contracts"
)

const (
	realReceiverCreationCodeHex = "600a600c600039600a6000f360005460010160005500"
	realTxIdempotencyKey        = "cre-real-simulated-evm-tx"
	simulatedChainID            = int64(1337)
)

// realTxForwarder is a deliberately thin adapter around an actual go-ethereum
// simulated chain. It replaces only the forwarder-client transport boundary;
// the production EVM WriteReport implementation, input validation, scheduling,
// receipt handling, fee conversion, and metering code all remain unmodified.
type realTxForwarder struct {
	backend *backends.SimulatedBackend
	key     *ecdsa.PrivateKey
	chainID *big.Int
	from    common.Address

	receiver common.Address
	gasLimit uint64
	submitted bool
	receipt   *gethtypes.Receipt
	feeWei    *big.Int
	beforeBal *big.Int
	afterBal  *big.Int
}

func newRealTxForwarder(t *testing.T) *realTxForwarder {
	t.Helper()

	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(key.PublicKey)
	initialBalance := new(big.Int).Mul(big.NewInt(10), big.NewInt(1_000_000_000_000_000_000))
	backend := backends.NewSimulatedBackend(gethtypes.GenesisAlloc{
		from: {Balance: initialBalance},
	}, 30_000_000)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })

	f := &realTxForwarder{
		backend: backend,
		key:     key,
		chainID: big.NewInt(simulatedChainID),
		from:    from,
	}

	creationCode, err := hex.DecodeString(realReceiverCreationCodeHex)
	require.NoError(t, err)
	deploymentReceipt, err := f.sendRawTransaction(t.Context(), nil, creationCode, 1_000_000)
	require.NoError(t, err)
	require.Equal(t, uint64(1), deploymentReceipt.Status)
	require.NotEqual(t, common.Address{}, deploymentReceipt.ContractAddress)
	f.receiver = deploymentReceipt.ContractAddress

	return f
}

func (f *realTxForwarder) sendRawTransaction(
	ctx context.Context,
	to *common.Address,
	data []byte,
	gasLimit uint64,
) (*gethtypes.Receipt, error) {
	nonce, err := f.backend.PendingNonceAt(ctx, f.from)
	if err != nil {
		return nil, fmt.Errorf("get pending nonce: %w", err)
	}
	tipCap, err := f.backend.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("suggest gas tip cap: %w", err)
	}
	header, err := f.backend.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("get latest header: %w", err)
	}
	if header.BaseFee == nil {
		return nil, fmt.Errorf("simulated chain returned nil base fee")
	}
	feeCap := new(big.Int).Add(
		new(big.Int).Mul(header.BaseFee, big.NewInt(2)),
		tipCap,
	)

	tx := gethtypes.NewTx(&gethtypes.DynamicFeeTx{
		ChainID:   f.chainID,
		Nonce:     nonce,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       gasLimit,
		To:        to,
		Value:     big.NewInt(0),
		Data:      data,
	})
	signed, err := gethtypes.SignTx(tx, gethtypes.LatestSignerForChainID(f.chainID), f.key)
	if err != nil {
		return nil, fmt.Errorf("sign transaction: %w", err)
	}
	if err := f.backend.SendTransaction(ctx, signed); err != nil {
		return nil, fmt.Errorf("send transaction: %w", err)
	}
	f.backend.Commit()
	receipt, err := f.backend.TransactionReceipt(ctx, signed.Hash())
	if err != nil {
		return nil, fmt.Errorf("get transaction receipt: %w", err)
	}
	return receipt, nil
}

func (f *realTxForwarder) GetTransmissionInfo(
	_ context.Context,
	_ contracts.TransmissionID,
) (contracts.TransmissionInfo, error) {
	if !f.submitted {
		return contracts.TransmissionInfo{
			State: contracts.TransmissionStateNotAttempted,
		}, nil
	}
	return contracts.TransmissionInfo{
		Success:  true,
		State:    contracts.TransmissionStateSucceeded,
		GasLimit: new(big.Int).SetUint64(f.gasLimit),
	}, nil
}

func (f *realTxForwarder) InvokeOnReport(
	ctx context.Context,
	receiver common.Address,
	_ *workflowpb.ReportResponse,
	gasConfig *evmcap.GasConfig,
) (*evmtypes.TransactionResult, error) {
	if receiver != f.receiver {
		return nil, fmt.Errorf("unexpected receiver: got %s want %s", receiver, f.receiver)
	}
	if gasConfig == nil || gasConfig.GasLimit == 0 {
		return nil, fmt.Errorf("missing positive gas limit")
	}

	beforeBal, err := f.backend.BalanceAt(ctx, f.from, nil)
	if err != nil {
		return nil, fmt.Errorf("get pre-transaction balance: %w", err)
	}
	to := receiver
	receipt, err := f.sendRawTransaction(ctx, &to, []byte{0x01}, gasConfig.GasLimit)
	if err != nil {
		return nil, err
	}
	if receipt.Status != 1 {
		return nil, fmt.Errorf("state-changing receiver transaction reverted")
	}
	if receipt.EffectiveGasPrice == nil {
		return nil, fmt.Errorf("receipt has nil effective gas price")
	}
	afterBal, err := f.backend.BalanceAt(ctx, f.from, nil)
	if err != nil {
		return nil, fmt.Errorf("get post-transaction balance: %w", err)
	}

	f.gasLimit = gasConfig.GasLimit
	f.receipt = receipt
	f.feeWei = new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	f.beforeBal = beforeBal
	f.afterBal = afterBal
	f.submitted = true

	return &evmtypes.TransactionResult{
		TxStatus:         evmtypes.TxSuccess,
		TxHash:           evmtypes.Hash(receipt.TxHash),
		TxIdempotencyKey: realTxIdempotencyKey,
	}, nil
}

func (f *realTxForwarder) GetReportProcessedEvents(
	_ context.Context,
	_ common.Address,
	_ [32]byte,
	_ [2]byte,
) ([]*evmtypes.Log, error) {
	if !f.submitted || f.receipt == nil {
		return nil, fmt.Errorf("report transaction has not been submitted")
	}
	return []*evmtypes.Log{{
		TxHash:      evmtypes.Hash(f.receipt.TxHash),
		Data:        successLogData(),
		BlockNumber: new(big.Int).Set(f.receipt.BlockNumber),
	}}, nil
}

func (f *realTxForwarder) capabilityReceipt() *evmtypes.Receipt {
	return &evmtypes.Receipt{
		Status:            f.receipt.Status,
		TxHash:            evmtypes.Hash(f.receipt.TxHash),
		GasUsed:           f.receipt.GasUsed,
		BlockHash:         evmtypes.Hash(f.receipt.BlockHash),
		BlockNumber:       new(big.Int).Set(f.receipt.BlockNumber),
		TransactionIndex:  uint64(f.receipt.TransactionIndex),
		EffectiveGasPrice: new(big.Int).Set(f.receipt.EffectiveGasPrice),
	}
}

func TestPoC_RealEVMWriteReportChargesIrreversibleGasWithoutSpendLimit(t *testing.T) {
	forwarder := newRealTxForwarder(t)
	service := InitMocks(t)
	service.EVM.forwarderClient = forwarder

	fixture := newWriteReportTestFixture(t)
	fixture.receiver = forwarder.receiver
	fixture.request.Receiver = forwarder.receiver.Bytes()
	fixture.request.GasConfig = &evmcap.GasConfig{GasLimit: 500_000}
	require.Empty(t, fixture.metadata.SpendLimits,
		"the production-shaped WriteReport request must enter without a GAS spend limit")

	service.EvmService.EXPECT().
		GetTransactionFee(mock.Anything, realTxIdempotencyKey).
		RunAndReturn(func(context.Context, string) (*evmtypes.TransactionFee, error) {
			require.NotNil(t, forwarder.feeWei)
			return &evmtypes.TransactionFee{TransactionFee: new(big.Int).Set(forwarder.feeWei)}, nil
		}).
		Once()
	service.EvmService.EXPECT().
		GetTransactionReceipt(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, req evmtypes.GeTransactionReceiptRequest) (*evmtypes.Receipt, error) {
			require.True(t, forwarder.submitted)
			require.Equal(t, evmtypes.Hash(forwarder.receipt.TxHash), req.Hash)
			return forwarder.capabilityReceipt(), nil
		}).
		Once()
	service.EvmService.EXPECT().
		CalculateTransactionFee(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, info evmtypes.ReceiptGasInfo) (*evmtypes.TransactionFee, error) {
			calculated := new(big.Int).Mul(new(big.Int).SetUint64(info.GasUsed), info.EffectiveGasPrice)
			require.Equal(t, forwarder.feeWei, calculated)
			return &evmtypes.TransactionFee{TransactionFee: calculated}, nil
		}).
		Once()

	result, err := service.WriteReport(t.Context(), fixture.metadata, fixture.request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Response)
	require.Equal(t, evmcap.TxStatus_TX_STATUS_SUCCESS, result.Response.TxStatus)
	require.True(t, forwarder.submitted)
	require.NotNil(t, forwarder.receipt)
	require.Positive(t, forwarder.receipt.GasUsed)
	require.Positive(t, forwarder.feeWei.Sign())

	balanceDelta := new(big.Int).Sub(forwarder.beforeBal, forwarder.afterBal)
	require.Equal(t, forwarder.feeWei, balanceDelta,
		"the transmitter account must irreversibly lose the exact mined transaction fee")

	storage, err := forwarder.backend.StorageAt(t.Context(), forwarder.receiver, common.Hash{}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), new(big.Int).SetBytes(storage).Int64(),
		"the receiver's persistent state must change before billing settlement")

	require.Len(t, result.ResponseMetadata.Metering, 1)
	metering := result.ResponseMetadata.Metering[0]
	require.Equal(t, "GAS.1", metering.SpendUnit)
	expectedFeeInNativeToken := new(big.Float).
		Quo(new(big.Float).SetInt(forwarder.feeWei), big.NewFloat(1e18)).
		Text('f', -1)
	require.Equal(t, expectedFeeInNativeToken, metering.SpendValue,
		"post-execution metering must equal the actual fee mined on the simulated chain")

	t.Logf(
		"real EVM spend reproduced: tx=%s gasUsed=%d effectiveGasPrice=%s feeWei=%s metered=%s receiverSlot0=1",
		forwarder.receipt.TxHash.Hex(),
		forwarder.receipt.GasUsed,
		forwarder.receipt.EffectiveGasPrice,
		forwarder.feeWei,
		metering.SpendValue,
	)
}
