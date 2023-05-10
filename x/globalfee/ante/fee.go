package ante

import (
	"errors"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	paramtypes "github.com/cosmos/cosmos-sdk/x/params/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	tmstrings "github.com/tendermint/tendermint/libs/strings"

	"github.com/cosmos/gaia/v9/x/globalfee"
	"github.com/cosmos/gaia/v9/x/globalfee/types"
)

// FeeWithBypassDecorator checks if the transaction's fee is at least as large
// as the local validator's minimum gasFee (defined in validator config) and global fee, and the fee denom should be in the global fees' denoms.
//
// If fee is too low, decorator returns error and tx is rejected from mempool.
// Note this only applies when ctx.CheckTx = true. If fee is high enough or not
// CheckTx, then call next AnteHandler.
//
// CONTRACT: Tx must implement FeeTx to use FeeDecorator
// If the tx msg type is one of the bypass msg types, the tx is valid even if the min fee is lower than normally required.
// If the bypass tx still carries fees, the fee denom should be the same as global fee required.

var _ sdk.AnteDecorator = FeeDecorator{}

type FeeDecorator struct {
	BypassMinFeeMsgTypes            []string
	GlobalMinFee                    globalfee.ParamSource
	StakingSubspace                 paramtypes.Subspace
	MaxTotalBypassMinFeeMsgGasUsage uint64
}

func NewFeeDecorator(bypassMsgTypes []string, globalfeeSubspace, stakingSubspace paramtypes.Subspace, maxTotalBypassMinFeeMsgGasUsage uint64) FeeDecorator {
	if !globalfeeSubspace.HasKeyTable() {
		panic("global fee paramspace was not set up via module")
	}

	if !stakingSubspace.HasKeyTable() {
		panic("staking paramspace was not set up via module")
	}

	return FeeDecorator{
		BypassMinFeeMsgTypes:            bypassMsgTypes,
		GlobalMinFee:                    globalfeeSubspace,
		StakingSubspace:                 stakingSubspace,
		MaxTotalBypassMinFeeMsgGasUsage: maxTotalBypassMinFeeMsgGasUsage,
	}
}

// AnteHandle implements the AnteDecorator interface
func (mfd FeeDecorator) AnteHandle(ctx sdk.Context, tx sdk.Tx, simulate bool, next sdk.AnteHandler) (newCtx sdk.Context, err error) {
	feeTx, ok := tx.(sdk.FeeTx)
	if !ok {
		return ctx, sdkerrors.Wrap(sdkerrors.ErrTxDecode, "Tx must implement the sdk.FeeTx interface")
	}

	// Do not check minimum-gas-prices and global fees during simulations
	if simulate {
		return next(ctx, tx, simulate)
	}

	// Get the required fees according to the CheckTx or DeliverTx modes
	feeRequired, err := mfd.GetTxFeeRequired(ctx, feeTx)
	if err != nil {
		return ctx, err
	}

	// reject the transaction early if the feeCoins have more denoms than the fee requirement
	// feeRequired cannot be be empty
	if feeTx.GetFee().Len() > feeRequired.Len() {
		return ctx, sdkerrors.Wrapf(sdkerrors.ErrInvalidCoins, "fee is not a subset of required fees; got %s, required: %s", feeTx.GetFee().String(), feeRequired.String())
	}

	// Sort fee tx's coins, zero coins in feeCoins are already removed
	feeCoins := feeTx.GetFee().Sort()
	gas := feeTx.GetGas()
	msgs := feeTx.GetMsgs()

	// split feeRequired into zero and non-zero coins(nonZeroCoinFeesReq, zeroCoinFeesDenomReq), split feeCoins according to
	// nonZeroCoinFeesReq, zeroCoinFeesDenomReq,
	// so that feeCoins can be checked separately against them.
	nonZeroCoinFeesReq, zeroCoinFeesDenomReq := getNonZeroFees(feeRequired)

	// feeCoinsNonZeroDenom contains non-zero denominations from the feeRequired
	// feeCoinsNonZeroDenom is used to check if the fees meets the requirement imposed by nonZeroCoinFeesReq
	// when feeCoins does not contain zero coins' denoms in feeRequired
	feeCoinsNonZeroDenom, feeCoinsZeroDenom := splitCoinsByDenoms(feeCoins, zeroCoinFeesDenomReq)

	// Check that the fees are in expected denominations.
	// according to splitCoinsByDenoms(), feeCoinsZeroDenom must be in denom subset of zeroCoinFeesDenomReq.
	// check if feeCoinsNonZeroDenom is a subset of nonZeroCoinFeesReq.
	// special case: if feeCoinsNonZeroDenom=[], DenomsSubsetOf returns true
	// special case: if feeCoinsNonZeroDenom is not empty, but nonZeroCoinFeesReq empty, return false
	if !feeCoinsNonZeroDenom.DenomsSubsetOf(nonZeroCoinFeesReq) {
		return ctx, sdkerrors.Wrapf(sdkerrors.ErrInsufficientFee, "fee is not a subset of required fees; got %s, required: %s", feeCoins.String(), feeRequired.String())
	}

	// If the feeCoins pass the denoms check, check they are bypass-msg types.
	//
	// Bypass min fee requires:
	// 	- the tx contains only message types that can bypass the minimum fee,
	//	see BypassMinFeeMsgTypes;
	//	- the total gas limit per message does not exceed MaxTotalBypassMinFeeMsgGasUsage,
	//	i.e., totalGas <=  MaxTotalBypassMinFeeMsgGasUsage
	// Otherwise, minimum fees and global fees are checked to prevent spam.
	doesNotExceedMaxGasUsage := gas <= mfd.MaxTotalBypassMinFeeMsgGasUsage
	allowedToBypassMinFee := mfd.ContainsOnlyBypassMinFeeMsgs(msgs) && doesNotExceedMaxGasUsage
	if allowedToBypassMinFee {
		return next(ctx, tx, simulate)
	}

	// if the msg does not satisfy bypass condition and the feeCoins denoms are subset of feeRequired,
	// check the feeCoins amount against feeRequired
	//
	// when feeCoins=[]
	// special case: and there is zero coin in fee requirement, pass,
	// otherwise, err
	if len(feeCoins) == 0 {
		if len(zeroCoinFeesDenomReq) != 0 {
			return next(ctx, tx, simulate)
		}
		return ctx, sdkerrors.Wrapf(sdkerrors.ErrInsufficientFee, "insufficient fees; got: %s required: %s", feeCoins.String(), feeRequired.String())
	}

	// when feeCoins != []
	// special case: if TX has at least one of the zeroCoinFeesDenomReq, then it should pass
	if len(feeCoinsZeroDenom) > 0 {
		return next(ctx, tx, simulate)
	}

	// After all the checks, the tx is confirmed:
	// feeCoins denoms subset off feeRequired
	// Not bypass
	// feeCoins != []
	// Not contain zeroCoinFeesDenomReq's denoms
	//
	// check if the feeCoins's feeCoinsNonZeroDenom part has coins' amount higher/equal to nonZeroCoinFeesReq
	if !feeCoinsNonZeroDenom.IsAnyGTE(nonZeroCoinFeesReq) {
		return ctx, sdkerrors.Wrapf(sdkerrors.ErrInsufficientFee, "insufficient fees; got: %s required: %s", feeCoins.String(), feeRequired.String())
	}

	return next(ctx, tx, simulate)
}

// GetTxFeeRequired returns the required fees for the given FeeTx.
// In case the FeeTx's mode is CheckTx, it returns the combined requirements
// of local min gas prices and global fees. Otherwise, in DeliverTx, it returns the global fee.
func (mfd FeeDecorator) GetTxFeeRequired(ctx sdk.Context, tx sdk.FeeTx) (sdk.Coins, error) {
	// Get required global fee min gas prices
	// Note that it should never be empty since its default value is set to coin={"StakingBondDenom", 0}
	globalFees, err := mfd.GetGlobalFee(ctx, tx)
	if err != nil {
		return sdk.Coins{}, err
	}

	// In DeliverTx, the global fee min gas prices are the only tx fee requirements.
	if !ctx.IsCheckTx() {
		return globalFees, nil
	}

	// In CheckTx mode, the local and global fee min gas prices are combined
	// to form the tx fee requirements

	// Get local minimum-gas-prices
	localFees := GetMinGasPrice(ctx, int64(tx.GetGas()))

	// Return combined fee requirements
	return CombinedFeeRequirement(globalFees, localFees)
}

// GetGlobalFee returns the global fees for a given fee tx's gas
// (might also return 0denom if globalMinGasPrice is 0)
// sorted in ascending order.
// Note that ParamStoreKeyMinGasPrices type requires coins sorted.
func (mfd FeeDecorator) GetGlobalFee(ctx sdk.Context, feeTx sdk.FeeTx) (sdk.Coins, error) {
	var (
		globalMinGasPrices sdk.DecCoins
		err                error
	)

	if mfd.GlobalMinFee.Has(ctx, types.ParamStoreKeyMinGasPrices) {
		mfd.GlobalMinFee.Get(ctx, types.ParamStoreKeyMinGasPrices, &globalMinGasPrices)
	}
	// global fee is empty set, set global fee to 0uatom
	if len(globalMinGasPrices) == 0 {
		globalMinGasPrices, err = mfd.DefaultZeroGlobalFee(ctx)
		if err != nil {
			return sdk.Coins{}, err
		}
	}
	requiredGlobalFees := make(sdk.Coins, len(globalMinGasPrices))
	// Determine the required fees by multiplying each required minimum gas
	// price by the gas limit, where fee = ceil(minGasPrice * gasLimit).
	glDec := sdk.NewDec(int64(feeTx.GetGas()))
	for i, gp := range globalMinGasPrices {
		fee := gp.Amount.Mul(glDec)
		requiredGlobalFees[i] = sdk.NewCoin(gp.Denom, fee.Ceil().RoundInt())
	}

	return requiredGlobalFees.Sort(), nil
}

// DefaultZeroGlobalFee returns a zero coin with the staking module bond denom
func (mfd FeeDecorator) DefaultZeroGlobalFee(ctx sdk.Context) ([]sdk.DecCoin, error) {
	bondDenom := mfd.getBondDenom(ctx)
	if bondDenom == "" {
		return nil, errors.New("empty staking bond denomination")
	}

	return []sdk.DecCoin{sdk.NewDecCoinFromDec(bondDenom, sdk.NewDec(0))}, nil
}

func (mfd FeeDecorator) getBondDenom(ctx sdk.Context) string {
	// prevent the getter above to panic
	// when the staking subspace isn't set
	if !mfd.StakingSubspace.HasKeyTable() {
		return ""
	}

	var bondDenom string
	mfd.StakingSubspace.Get(ctx, stakingtypes.KeyBondDenom, &bondDenom)

	return bondDenom
}

// ContainsOnlyBypassMinFeeMsgs returns true if all the given msgs type are listed
// in the BypassMinFeeMsgTypes of the FeeDecorator.
func (mfd FeeDecorator) ContainsOnlyBypassMinFeeMsgs(msgs []sdk.Msg) bool {
	for _, msg := range msgs {
		if tmstrings.StringInSlice(sdk.MsgTypeURL(msg), mfd.BypassMinFeeMsgTypes) {
			continue
		}
		return false
	}

	return true
}

// GetMinGasPrice returns the validator's minimum gas prices
// fees given a gas limit
func GetMinGasPrice(ctx sdk.Context, gasLimit int64) sdk.Coins {
	minGasPrices := ctx.MinGasPrices()
	// special case: if minGasPrices=[], requiredFees=[]
	if minGasPrices.IsZero() {
		return sdk.Coins{}
	}

	requiredFees := make(sdk.Coins, len(minGasPrices))
	// Determine the required fees by multiplying each required minimum gas
	// price by the gas limit, where fee = ceil(minGasPrice * gasLimit).
	glDec := sdk.NewDec(gasLimit)
	for i, gp := range minGasPrices {
		fee := gp.Amount.Mul(glDec)
		requiredFees[i] = sdk.NewCoin(gp.Denom, fee.Ceil().RoundInt())
	}

	return requiredFees.Sort()
}
