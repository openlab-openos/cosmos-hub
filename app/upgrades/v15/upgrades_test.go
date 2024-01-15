package v15_test

import (
	"testing"
	"time"

	"cosmossdk.io/math"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	tmtime "github.com/cometbft/cometbft/types/time"
	"github.com/cosmos/cosmos-sdk/testutil/mock"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	vesting "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
	banktestutil "github.com/cosmos/cosmos-sdk/x/bank/testutil"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/gaia/v15/app/helpers"
	v15 "github.com/cosmos/gaia/v15/app/upgrades/v15"
)

func TestMigrateMinCommissionRate(t *testing.T) {
	gaiaApp := helpers.Setup(t)
	ctx := gaiaApp.NewUncachedContext(true, tmproto.Header{})

	// set min commission rate to 0
	stakingParams := gaiaApp.StakingKeeper.GetParams(ctx)
	stakingParams.MinCommissionRate = sdk.ZeroDec()
	err := gaiaApp.StakingKeeper.SetParams(ctx, stakingParams)
	require.NoError(t, err)

	// confirm all commissions are 0
	stakingKeeper := gaiaApp.StakingKeeper

	for _, val := range stakingKeeper.GetAllValidators(ctx) {
		require.Equal(t, val.Commission.CommissionRates.Rate, sdk.ZeroDec(), "non-zero previous commission rate for validator %s", val.GetOperator())
	}

	// pre-test min commission rate is 0
	require.Equal(t, stakingKeeper.GetParams(ctx).MinCommissionRate, sdk.ZeroDec(), "non-zero previous min commission rate")

	// run the test and confirm the values have been updated
	v15.MigrateMinCommissionRate(ctx, *gaiaApp.AppKeepers.StakingKeeper)

	newStakingParams := gaiaApp.StakingKeeper.GetParams(ctx)
	require.NotEqual(t, newStakingParams.MinCommissionRate, sdk.ZeroDec(), "failed to update min commission rate")
	require.Equal(t, newStakingParams.MinCommissionRate, sdk.NewDecWithPrec(5, 2), "failed to update min commission rate")

	for _, val := range stakingKeeper.GetAllValidators(ctx) {
		require.Equal(t, val.Commission.CommissionRates.Rate, newStakingParams.MinCommissionRate, "failed to update update commission rate for validator %s", val.GetOperator())
	}

	// set one of the validators commission rate to 10% and ensure it is not updated
	updateValCommission := sdk.NewDecWithPrec(10, 2)
	updateVal := stakingKeeper.GetAllValidators(ctx)[0]
	updateVal.Commission.CommissionRates.Rate = updateValCommission
	stakingKeeper.SetValidator(ctx, updateVal)

	v15.MigrateMinCommissionRate(ctx, *gaiaApp.AppKeepers.StakingKeeper)
	for _, val := range stakingKeeper.GetAllValidators(ctx) {
		if updateVal.OperatorAddress == val.OperatorAddress {
			require.Equal(t, val.Commission.CommissionRates.Rate, updateValCommission, "should not update commission rate for validator %s", val.GetOperator())
		} else {
			require.Equal(t, val.Commission.CommissionRates.Rate, newStakingParams.MinCommissionRate, "failed to update update commission rate for validator %s", val.GetOperator())
		}
	}
}

func TestMigrateValidatorsSigningInfos(t *testing.T) {
	gaiaApp := helpers.Setup(t)
	ctx := gaiaApp.NewUncachedContext(true, tmproto.Header{})
	slashingKeeper := gaiaApp.SlashingKeeper

	signingInfosNum := 8
	emptyAddrCtr := 0

	// create some dummy signing infos, half of which with an empty address field
	for i := 0; i < signingInfosNum; i++ {
		pubKey, err := mock.NewPV().GetPubKey()
		require.NoError(t, err)

		consAddr := sdk.ConsAddress(pubKey.Address())
		info := slashingtypes.NewValidatorSigningInfo(
			consAddr,
			0,
			0,
			time.Unix(0, 0),
			false,
			0,
		)

		if i <= signingInfosNum/2 {
			info.Address = ""
			emptyAddrCtr++
		}

		slashingKeeper.SetValidatorSigningInfo(ctx, consAddr, info)
		require.NoError(t, err)
	}

	// check signing info were correctly created
	slashingKeeper.IterateValidatorSigningInfos(ctx, func(address sdk.ConsAddress, info slashingtypes.ValidatorSigningInfo) (stop bool) {
		if info.Address == "" {
			emptyAddrCtr--
		}

		return false
	})
	require.Zero(t, emptyAddrCtr)

	// upgrade signing infos
	v15.MigrateSigningInfos(ctx, slashingKeeper)

	// check that all signing info have the address field correctly updated
	slashingKeeper.IterateValidatorSigningInfos(ctx, func(address sdk.ConsAddress, info slashingtypes.ValidatorSigningInfo) (stop bool) {
		require.NotEmpty(t, info.Address)
		require.Equal(t, address.String(), info.Address)

		return false
	})
}

func TestMigrateVestingAccount(t *testing.T) {
	gaiaApp := helpers.Setup(t)

	now := tmtime.Now()
	endTime := now.Add(24 * time.Hour)

	bankKeeper := gaiaApp.BankKeeper
	accountKeeper := gaiaApp.AccountKeeper
	distrKeeper := gaiaApp.DistrKeeper
	stakingKeeper := gaiaApp.StakingKeeper

	ctx := gaiaApp.NewUncachedContext(true, tmproto.Header{})
	ctx = ctx.WithBlockHeader(tmproto.Header{Time: now})

	validator := stakingKeeper.GetAllValidators(ctx)[0]
	bondDenom := stakingKeeper.GetParams(ctx).BondDenom

	// create continuous vesting account
	origCoins := sdk.NewCoins(sdk.NewInt64Coin(bondDenom, 100))
	addr := sdk.AccAddress([]byte("addr1_______________"))

	vestingAccount := vesting.NewContinuousVestingAccount(
		authtypes.NewBaseAccountWithAddress(addr),
		origCoins,
		now.Unix(),
		endTime.Unix(),
	)

	require.True(t, vestingAccount.GetVestingCoins(now).IsEqual(origCoins))

	accountKeeper.SetAccount(ctx, vestingAccount)

	// check vesting account balance was set correctly
	require.NoError(t, bankKeeper.ValidateBalance(ctx, addr))
	require.Empty(t, bankKeeper.GetAllBalances(ctx, addr))

	// send original vesting coin amount
	require.NoError(t, banktestutil.FundAccount(bankKeeper, ctx, addr, origCoins))
	require.True(t, origCoins.IsEqual(bankKeeper.GetAllBalances(ctx, addr)))

	initBal := bankKeeper.GetAllBalances(ctx, vestingAccount.GetAddress())
	require.True(t, initBal.IsEqual(origCoins))

	// save validator tokens
	oldValTokens := validator.Tokens

	// delegate all vesting account tokens
	_, err := stakingKeeper.Delegate(
		ctx, vestingAccount.GetAddress(),
		origCoins.AmountOf(bondDenom),
		stakingtypes.Unbonded,
		validator,
		true)
	require.NoError(t, err)

	// check that the validator's tokens and shares increased
	validator = stakingKeeper.GetAllValidators(ctx)[0]
	del, found := stakingKeeper.GetDelegation(ctx, addr, validator.GetOperator())
	require.True(t, found)
	require.True(t, validator.Tokens.Equal(oldValTokens.Add(origCoins.AmountOf(bondDenom))))
	require.Equal(
		t,
		validator.TokensFromShares(del.Shares),
		math.LegacyNewDec(origCoins.AmountOf(bondDenom).Int64()),
	)

	// check vesting account delegations
	vestingAccount = accountKeeper.GetAccount(ctx, addr).(*vesting.ContinuousVestingAccount)
	require.Equal(t, vestingAccount.GetDelegatedVesting(), origCoins)
	require.Empty(t, vestingAccount.GetDelegatedFree())

	// vest half of the tokens
	ctx = ctx.WithBlockTime(now.Add(12 * time.Hour))

	currVestingCoins := vestingAccount.GetVestingCoins(ctx.BlockTime())
	currVestedCoins := vestingAccount.GetVestedCoins(ctx.BlockTime())

	require.True(t, currVestingCoins.IsEqual(origCoins.QuoInt(math.NewInt(2))))
	require.True(t, currVestedCoins.IsEqual(origCoins.QuoInt(math.NewInt(2))))

	// execute migration script
	v15.MigrateVestingAccount(ctx, addr, &gaiaApp.AppKeepers)

	// check that the validator's delegation is removed and that
	// the total tokens decreased
	validator = stakingKeeper.GetAllValidators(ctx)[0]
	_, found = stakingKeeper.GetDelegation(ctx, addr, validator.GetOperator())
	require.False(t, found)
	require.Equal(
		t,
		validator.TokensFromShares(validator.DelegatorShares),
		math.LegacyNewDec(oldValTokens.Int64()),
	)

	// check that the resulting account is of BaseAccount type now
	account, ok := accountKeeper.GetAccount(ctx, addr).(*authtypes.BaseAccount)
	require.True(t, ok)
	// check that the account values are still the same
	require.EqualValues(t, account, vestingAccount.BaseAccount)

	// check that the account's balance still has the vested tokens
	require.True(t, bankKeeper.GetAllBalances(ctx, addr).IsEqual(currVestedCoins))
	// check that the community pool balance received the vesting tokens
	require.True(
		t,
		distrKeeper.GetFeePoolCommunityCoins(ctx).
			IsEqual(sdk.NewDecCoinsFromCoins(currVestingCoins...)),
	)
}
