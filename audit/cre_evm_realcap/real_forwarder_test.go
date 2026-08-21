package realcap_test

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/abi/bind/backends"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
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
	forwarder "github.com/smartcontractkit/chainlink-evm/gethwrappers/keystone/generated/forwarder"
	reservemanager "github.com/smartcontractkit/chainlink-evm/gethwrappers/workflow/generated/reserve_manager"
	workflowpb "github.com/smartcontractkit/chainlink-protos/cre/go/sdk"
)

func repeatByte32(b byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = b
	}
	return out
}

func signForwarderReport(t *testing.T, rawReport, reportContext []byte, privateKeys ...[]byte) []*workflowpb.AttributedSignature {
	t.Helper()

	innerHash := crypto.Keccak256Hash(rawReport)
	completeHash := crypto.Keccak256Hash(append(innerHash.Bytes(), reportContext...))
	signatures := make([]*workflowpb.AttributedSignature, 0, len(privateKeys))
	for _, encodedKey := range privateKeys {
		privateKey, err := crypto.ToECDSA(encodedKey)
		require.NoError(t, err)
		signature, err := crypto.Sign(completeHash.Bytes(), privateKey)
		require.NoError(t, err)
		require.Len(t, signature, 65)
		// crypto.Sign returns r || s || recovery-id (0/1), exactly the format
		// expected by KeystoneForwarder, which adds 27 before ecrecover.
		signatures = append(signatures, &workflowpb.AttributedSignature{Signature: signature})
	}
	return signatures
}

// TestRealKeystoneForwarderWriteReportPaysGasBeyondSpendLimit removes the last
// contract-level shortcut from the simulated-chain PoC. It deploys the real
// KeystoneForwarder, configures a real DON signer set, supplies F+1 valid
// secp256k1 signatures, changes receiver state, and verifies that the
// transmitter pays a receipt-derived fee above the supplied GAS spend limit.
func TestRealKeystoneForwarderWriteReportPaysGasBeyondSpendLimit(t *testing.T) {
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

	// KeystoneForwarder requires more than 3F configured signers and exactly
	// F+1 report signatures. Generate four independent signers and later sign
	// with the first two.
	signerAddresses := make([]common.Address, 0, 4)
	signerKeys := make([][]byte, 0, 4)
	for i := 0; i < 4; i++ {
		key, keyErr := crypto.GenerateKey()
		require.NoError(t, keyErr)
		signerAddresses = append(signerAddresses, crypto.PubkeyToAddress(key.PublicKey))
		signerKeys = append(signerKeys, crypto.FromECDSA(key))
	}
	_, err = forwarderContract.SetConfig(auth, donID, configVersion, faults, signerAddresses)
	require.NoError(t, err)
	backend.Commit()

	reserveAddress, _, reserveManager, err := reservemanager.DeployReserveManager(auth, backend)
	require.NoError(t, err)
	backend.Commit()

	service, err := newSimulatedChainEVMService(backend, auth, forwarderAddress)
	require.NoError(t, err)
	lggr := logger.Test(t)

	evmCapability, err := actions.NewEVM(
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
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, evmCapability.Close()) })

	executionID := repeatByte32(0x71)
	reportID := [2]byte{0x75, 0x75}
	reportMetadata := ocrtypes.Metadata{
		Version:          1,
		ExecutionID:      hex.EncodeToString(executionID[:]),
		Timestamp:        1_000,
		DONID:            donID,
		DONConfigVersion: configVersion,
		WorkflowID:       strings.Repeat("72", 32),
		WorkflowName:     strings.Repeat("73", 10),
		WorkflowOwner:    strings.Repeat("74", 20),
		ReportID:         hex.EncodeToString(reportID[:]),
	}
	encodedMetadata, err := reportMetadata.Encode()
	require.NoError(t, err)

	uint256Type, err := abi.NewType("uint256", "", nil)
	require.NoError(t, err)
	payload, err := (abi.Arguments{{Type: uint256Type}, {Type: uint256Type}}).Pack(big.NewInt(789), big.NewInt(987))
	require.NoError(t, err)
	rawReport := append(append([]byte(nil), encodedMetadata...), payload...)
	reportContext := make([]byte, 96)
	signatures := signForwarderReport(t, rawReport, reportContext, signerKeys[0], signerKeys[1])

	spendLimit := "0.000000000000000001" // one wei, intentionally below the real transaction fee
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
	response, err := evmCapability.WriteReport(t.Context(), metadata, &evmcap.WriteReportRequest{
		Receiver: reserveAddress.Bytes(),
		Report: &workflowpb.ReportResponse{
			RawReport:     rawReport,
			ReportContext: reportContext,
			Sigs:          signatures,
		},
		GasConfig: &evmcap.GasConfig{GasLimit: 1_000_000},
	})
	require.NoError(t, err)
	balanceAfter, err := backend.BalanceAt(t.Context(), auth.From, nil)
	require.NoError(t, err)

	minted, err := reserveManager.LastTotalMinted(&bind.CallOpts{Context: t.Context()})
	require.NoError(t, err)
	reserve, err := reserveManager.LastTotalReserve(&bind.CallOpts{Context: t.Context()})
	require.NoError(t, err)
	require.Equal(t, int64(789), minted.Int64())
	require.Equal(t, int64(987), reserve.Int64())

	transmission, err := forwarderContract.GetTransmissionInfo(
		&bind.CallOpts{Context: t.Context()}, reserveAddress, executionID, reportID,
	)
	require.NoError(t, err)
	require.Equal(t, uint8(1), transmission.State, "real forwarder must record a successful transmission")
	require.True(t, transmission.Success)
	require.Equal(t, auth.From, transmission.Transmitter)

	require.Len(t, response.ResponseMetadata.Metering, 1)
	metering := response.ResponseMetadata.Metering[0]
	actualSpend, ok := new(big.Rat).SetString(metering.SpendValue)
	require.True(t, ok)
	allowedSpend, ok := new(big.Rat).SetString(spendLimit)
	require.True(t, ok)
	require.Greater(t, actualSpend.Cmp(allowedSpend), 0)

	paid := new(big.Int).Sub(balanceBefore, balanceAfter)
	recordedFee := service.recordedFee()
	require.NotNil(t, recordedFee)
	require.Positive(t, paid.Sign())
	require.Zero(t, paid.Cmp(recordedFee), "transmitter balance loss must equal the receipt-derived fee")
	require.Greater(t, paid.Cmp(big.NewInt(1)), 0)

	t.Logf("real-forwarder overspend reproduced: limit=%s native actual=%s native feeWei=%s receiverState=(%s,%s) transmitter=%s",
		spendLimit, metering.SpendValue, paid.String(), minted.String(), reserve.String(), auth.From.Hex())
}
