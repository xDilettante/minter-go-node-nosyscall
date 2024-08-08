package transaction

import (
	"encoding/hex"
	"fmt"
	"github.com/MinterTeam/minter-go-node/coreV2/state/swap"
	"math/big"

	"github.com/MinterTeam/minter-go-node/coreV2/code"
	"github.com/MinterTeam/minter-go-node/coreV2/state"
	"github.com/MinterTeam/minter-go-node/coreV2/state/commission"
	"github.com/MinterTeam/minter-go-node/coreV2/types"
	"github.com/MinterTeam/minter-go-node/hexutil"
	abcTypes "github.com/tendermint/tendermint/abci/types"
)

type DelegateDataV260 struct {
	PubKey types.Pubkey
	Coin   types.CoinID
	Value  *big.Int
}

func (data DelegateDataV260) Gas() int64 {
	return gasDelegate
}
func (data DelegateDataV260) TxType() TxType {
	return TypeDelegate
}

func (data DelegateDataV260) basicCheck(tx *Transaction, context *state.CheckState) *Response {
	coin := context.Coins().GetCoin(data.Coin)
	if coin == nil {
		return &Response{
			Code: code.CoinNotExists,
			Log:  fmt.Sprintf("Coin %s not exists", data.Coin),
			Info: EncodeError(code.NewCoinNotExists("", data.Coin.String())),
		}
	}

	if !coin.BaseOrHasReserve() {
		return &Response{
			Code: code.CoinReserveNotSufficient,
			Log:  "coin has no reserve",
			Info: EncodeError(code.NewCoinReserveNotSufficient(
				coin.GetFullSymbol(),
				coin.ID().String(),
				coin.Reserve().String(),
				"",
			)),
		}
	}

	sender, _ := tx.Sender()
	value := big.NewInt(0).Set(data.Value)
	if waitList := context.WaitList().Get(sender, data.PubKey, data.Coin); waitList != nil {
		value.Add(value, waitList.Value)
	}

	if value.Sign() < 1 {
		return &Response{
			Code: code.StakeShouldBePositive,
			Log:  "Stake should be positive",
			Info: EncodeError(code.NewStakeShouldBePositive(value.String())),
		}
	}

	if !context.Candidates().Exists(data.PubKey) {
		return &Response{
			Code: code.CandidateNotFound,
			Log:  "Candidate with such public key not found",
			Info: EncodeError(code.NewCandidateNotFound(data.PubKey.String())),
		}
	}

	low, b := context.Candidates().IsDelegatorStakeAllowed(sender, data.PubKey, data.Coin, value)
	if low {
		return &Response{
			Code: code.TooLowStake,
			Log:  "Stake is too low",
			Info: EncodeError(code.NewTooLowStake(sender.String(), data.PubKey.String(), value.String(), data.Coin.String(), coin.GetFullSymbol())),
		}
	}
	if b {
		return &Response{
			Code: code.TooBigStake,
			Log:  "Cannot be delegated to a candidate, and his total stake exceeds 20% of the network",
			Info: EncodeError(code.NewTooBigStake(sender.String(), data.PubKey.String(), value.String(), data.Coin.String(), coin.GetFullSymbol())),
		}
	}

	return nil
}

func (data DelegateDataV260) String() string {
	return fmt.Sprintf("DELEGATE pubkey:%s ",
		hexutil.Encode(data.PubKey[:]))
}

func (data DelegateDataV260) CommissionData(price *commission.Price) *big.Int {
	return price.Delegate
}

func (data DelegateDataV260) Run(tx *Transaction, context state.Interface, rewardPool *big.Int, currentBlock uint64, price *big.Int) Response {
	sender, _ := tx.Sender()

	var checkState *state.CheckState
	var isCheck bool
	if checkState, isCheck = context.(*state.CheckState); !isCheck {
		checkState = state.NewCheckState(context.(*state.State))
	}

	response := data.basicCheck(tx, checkState)
	if response != nil {
		return *response
	}

	commissionInBaseCoin := price
	commissionPoolSwapper := checkState.Swap().GetSwapper(tx.GasCoin, types.GetBaseCoinID())
	gasCoin := checkState.Coins().GetCoin(tx.GasCoin)
	commission, isGasCommissionFromPoolSwap, errResp := CalculateCommission(checkState, commissionPoolSwapper, gasCoin, commissionInBaseCoin)
	if errResp != nil {
		return *errResp
	}

	if checkState.Accounts().GetBalance(sender, tx.GasCoin).Cmp(commission) < 0 {
		return Response{
			Code: code.InsufficientFunds,
			Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %s %s", sender.String(), commission, gasCoin.GetFullSymbol()),
			Info: EncodeError(code.NewInsufficientFunds(sender.String(), commission.String(), gasCoin.GetFullSymbol(), gasCoin.ID().String())),
		}
	}

	if checkState.Accounts().GetBalance(sender, data.Coin).Cmp(data.Value) < 0 {
		coin := checkState.Coins().GetCoin(data.Coin)
		return Response{
			Code: code.InsufficientFunds,
			Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %s %s", sender.String(), data.Value, coin.GetFullSymbol()),
			Info: EncodeError(code.NewInsufficientFunds(sender.String(), data.Value.String(), coin.GetFullSymbol(), coin.ID().String())),
		}
	}

	if data.Coin == tx.GasCoin {
		totalTxCost := big.NewInt(0)
		totalTxCost.Add(totalTxCost, data.Value)
		totalTxCost.Add(totalTxCost, commission)

		if checkState.Accounts().GetBalance(sender, tx.GasCoin).Cmp(totalTxCost) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %s %s", sender.String(), totalTxCost.String(), gasCoin.GetFullSymbol()),
				Info: EncodeError(code.NewInsufficientFunds(sender.String(), totalTxCost.String(), gasCoin.GetFullSymbol(), gasCoin.ID().String())),
			}
		}
	}
	var tags []abcTypes.EventAttribute
	if deliverState, ok := context.(*state.State); ok {
		var tagsCom *tagPoolChange
		if isGasCommissionFromPoolSwap {
			var (
				poolIDCom  uint32
				detailsCom *swap.ChangeDetailsWithOrders
				ownersCom  []*swap.OrderDetail
			)
			commission, commissionInBaseCoin, poolIDCom, detailsCom, ownersCom = deliverState.Swapper().PairSellWithOrders(tx.CommissionCoin(), types.GetBaseCoinID(), commission, big.NewInt(0))
			tagsCom = &tagPoolChange{
				PoolID:   poolIDCom,
				CoinIn:   tx.CommissionCoin(),
				ValueIn:  commission.String(),
				CoinOut:  types.GetBaseCoinID(),
				ValueOut: commissionInBaseCoin.String(),
				Orders:   detailsCom,
				// Sellers:  ownersCom,
			}
			for _, value := range ownersCom {
				deliverState.Accounts.AddBalance(value.Owner, tx.CommissionCoin(), value.ValueBigInt)
			}
		} else if !tx.GasCoin.IsBaseCoin() {
			deliverState.Coins.SubVolume(tx.CommissionCoin(), commission)
			deliverState.Coins.SubReserve(tx.CommissionCoin(), commissionInBaseCoin)
		}
		deliverState.Accounts.SubBalance(sender, tx.GasCoin, commission)
		rewardPool.Add(rewardPool, commissionInBaseCoin)
		deliverState.Accounts.SubBalance(sender, data.Coin, data.Value)

		value := big.NewInt(0).Set(data.Value)
		if waitList := deliverState.Waitlist.Get(sender, data.PubKey, data.Coin); waitList != nil {
			value.Add(value, waitList.Value)
			deliverState.Waitlist.Delete(sender, data.PubKey, data.Coin)
		}

		deliverState.Candidates.Delegate(sender, data.PubKey, data.Coin, value, big.NewInt(0))
		deliverState.Accounts.SetNonce(sender, tx.Nonce)

		tags = []abcTypes.EventAttribute{
			{Key: []byte("tx.commission_in_base_coin"), Value: []byte(commissionInBaseCoin.String())},
			{Key: []byte("tx.commission_conversion"), Value: []byte(isGasCommissionFromPoolSwap.String()), Index: true},
			{Key: []byte("tx.commission_amount"), Value: []byte(commission.String())},
			{Key: []byte("tx.commission_details"), Value: []byte(tagsCom.string())},
			{Key: []byte("tx.public_key"), Value: []byte(hex.EncodeToString(data.PubKey[:])), Index: true},
			{Key: []byte("tx.coin_id"), Value: []byte(data.Coin.String()), Index: true},
		}
	}

	return Response{
		Code: code.OK,
		Tags: tags,
	}
}
