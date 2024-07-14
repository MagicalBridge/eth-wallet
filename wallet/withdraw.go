package wallet

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/the-web3/eth-wallet/wallet/ethereum"

	"github.com/the-web3/eth-wallet/common/tasks"
	"github.com/the-web3/eth-wallet/config"
	"github.com/the-web3/eth-wallet/database"
	"github.com/the-web3/eth-wallet/wallet/node"
)

var (
	EthGasLimit          uint64 = 21000
	TokenGasLimit        uint64 = 120000
	maxFeePerGas                = big.NewInt(2900000000)
	maxPriorityFeePerGas        = big.NewInt(2600000000)
)

type Withdraw struct {
	db             *database.DB
	chainConf      *config.ChainConfig
	client         node.EthClient
	resourceCtx    context.Context
	resourceCancel context.CancelFunc
	tasks          tasks.Group
}

func NewWithdraw(cfg *config.Config, db *database.DB, client node.EthClient, shutdown context.CancelCauseFunc) (*Withdraw, error) {
	resCtx, resCancel := context.WithCancel(context.Background())
	return &Withdraw{
		db:             db,
		chainConf:      &cfg.Chain,
		client:         client,
		resourceCtx:    resCtx,
		resourceCancel: resCancel,
		tasks: tasks.Group{HandleCrit: func(err error) {
			shutdown(fmt.Errorf("critical error in deposit: %w", err))
		}},
	}, nil
}

func (w *Withdraw) Close() error {
	var result error
	w.resourceCancel()
	if err := w.tasks.Wait(); err != nil {
		result = errors.Join(result, fmt.Errorf("failed to await deposit %w"), err)
	}
	return nil
}

func (w *Withdraw) Start() error {
	log.Info("start withdraw......")
	tickerWithdrawWorker := time.NewTicker(time.Second * 5)
	w.tasks.Go(func() error {
		for range tickerWithdrawWorker.C {
			withdrawList, err := w.db.Withdraw.UnSendWithdrawList()
			if err != nil {
				log.Error("get unsend withdraw list fail", "err", err)
				return err
			}
			hotWallet, err := w.db.Addresses.QueryHotWalletInfo()
			if err != nil {
				log.Error("query hot wallet info err", "err", err)
				return err
			}
			// nonce
			nonce, err := w.client.TxCountByAddress(hotWallet.Address)
			if err != nil {
				log.Error("query nonce by address fail", "err", err)
				return err
			}
			var returnWithdrawList []database.Withdraw
			index := 0
			for _, withdraw := range withdrawList {
				var buildData []byte
				var gasLimit uint64
				var toAddress *common.Address
				var amount *big.Int
				amountToken, _ := strconv.ParseInt(hotWallet.Balance, 10, 64)
				if withdraw.ToAddress.Hex() != "0x00" {
					buildData = ethereum.BuildErc20Data(withdraw.ToAddress, big.NewInt(amountToken))
					toAddress = &withdraw.ToKenAddress
					gasLimit = TokenGasLimit
					amount = big.NewInt(0)
				} else {
					toAddress = toAddress
					gasLimit = EthGasLimit
					amount = big.NewInt(amountToken)
				}
				dFeeTx := &types.DynamicFeeTx{
					ChainID:   big.NewInt(int64(w.chainConf.ChainID)),
					Nonce:     nonce.Uint64(),
					GasTipCap: maxPriorityFeePerGas,
					GasFeeCap: maxFeePerGas,
					Gas:       gasLimit,
					To:        toAddress,
					Value:     amount,
					Data:      buildData,
				}
				rawTx, err := ethereum.OfflineSignTx(dFeeTx, hotWallet.PrivateKey, big.NewInt(int64(w.chainConf.ChainID)))
				if err != nil {
					log.Error("offline transaction fail", "err", err)
					return err
				}
				//  sendRawTx
				log.Info("Offline sign tx success", "rawTx", rawTx)

				hash, err := w.client.SendRawTransaction(rawTx)
				if err != nil {
					log.Error("send raw transaction fail", "err", err)
					return err
				}
				returnWithdrawList[index].Hash = *hash
				returnWithdrawList[index].GUID = withdraw.GUID

				//  修改数据库
				err = w.db.Withdraw.MarkWithdrawToSend(returnWithdrawList)
				if err != nil {
					log.Error("mark withdraw send fail", "err", err)
					return err
				}
				index++
			}
		}
		return nil
	})
	return nil
}
