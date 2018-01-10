// Copyright (C) 2017, Beijing Bochen Technology Co.,Ltd.  All rights reserved.
//
// This file is part of L0
//
// The L0 is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The L0 is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package ledger

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"math/big"
	"path/filepath"

	"errors"
	"strconv"
	"strings"

	"reflect"

	"github.com/bocheninc/L0/components/crypto"
	"github.com/bocheninc/L0/components/db"
	"github.com/bocheninc/L0/components/db/mongodb"
	"github.com/bocheninc/L0/components/log"
	"github.com/bocheninc/L0/components/utils"
	"github.com/bocheninc/L0/core/accounts"
	"github.com/bocheninc/L0/core/coordinate"
	"github.com/bocheninc/L0/core/ledger/block_storage"
	"github.com/bocheninc/L0/core/ledger/merge"
	"github.com/bocheninc/L0/core/ledger/state"
	"github.com/bocheninc/L0/core/notify"
	"github.com/bocheninc/L0/core/params"
	"github.com/bocheninc/L0/core/types"
	"gopkg.in/mgo.v2/bson"
)

var (
	ledgerInstance *Ledger
)

type ValidatorHandler interface {
	RemoveTxsInVerification(txs types.Transactions)
	SecurityPluginDir() string
}

// Ledger represents the ledger in blockchain
type Ledger struct {
	dbHandler *db.BlockchainDB
	block     *block_storage.Blockchain
	state     *state.BLKRWSet
	storage   *merge.Storage
	Validator ValidatorHandler
	conf      *Config
	mdb       *mongodb.Mdb
	mdbChan   chan []*db.WriteBatch
}

// NewLedger returns the ledger instance
func NewLedger(kvdb *db.BlockchainDB, conf *Config) *Ledger {
	if ledgerInstance == nil {
		ledgerInstance = &Ledger{
			dbHandler: kvdb,
			block:     block_storage.NewBlockchain(kvdb),
			state:     state.NewBLKRWSet(kvdb),
			storage:   merge.NewStorage(kvdb),
		}
		_, err := ledgerInstance.Height()
		if err != nil {
			if params.Nvp && params.Mongodb {
				ledgerInstance.mdb = mongodb.MongDB()
				ledgerInstance.block.RegisterColumn(ledgerInstance.mdb)
				ledgerInstance.state.RegisterColumn(ledgerInstance.mdb)
				ledgerInstance.mdbChan = make(chan []*db.WriteBatch)
				ledgerInstance.conf = conf
				go ledgerInstance.PutIntoMongoDB()
			}
			ledgerInstance.init()
		}
	}

	return ledgerInstance
}

func (ledger *Ledger) DBHandler() *db.BlockchainDB {
	return ledger.dbHandler
}

func (ledger *Ledger) reOrgBatches(batches []*db.WriteBatch) map[string][]*db.WriteBatch {
	reBatches := make(map[string][]*db.WriteBatch)
	for _, batch := range batches {
		columnName := batch.CfName
		batchKey := batch.Key
		if 0 == strings.Compare(batch.CfName, ledger.state.GetChainCodeCF()) {
			k, v := state.DecodeCompositeKey(string(batch.Key))

			columnName = "contract|" + k
			batchKey = []byte(v)
		}

		if _, ok := reBatches[columnName]; !ok {
			reBatches[columnName] = make([]*db.WriteBatch, 0)
		}

		reBatches[columnName] = append(reBatches[columnName], db.NewWriteBatch(columnName, batch.Operation, batchKey, batch.Value, batch.Typ))
	}

	return reBatches
}

func (ledger *Ledger) PutIntoMongoDB() {
	contractCol := false
	var err error
	batchesBlockChan := make(chan []*db.WriteBatch, 1)

Out:
	for {
		select {
		case batches := <-ledger.mdbChan:
			log.Infof("all data len: %+v", len(batches))

			reBatches := ledger.reOrgBatches(batches)
			for colName, colBatches := range reBatches {
				contractCol = false
				if strings.Contains(colName, "contract") {
					contractCol = true
					cols := strings.SplitN(colName, "|", 2)
					if len(cols) == 2 {
						colName = cols[1]
					}
				}
				bulk := ledger.mdb.Coll(colName).Bulk()

				log.Infof("colName: %+v, start -----", colName)
				writeBatchCnt := 0
				for _, batch := range colBatches {
					var value interface{}
					if batch.Operation == db.OperationPut {
						if contractCol {
							contractData, err := state.DoContractStateData(batch.Value)
							if err != nil || contractData == nil && err == nil {
								log.Warnf("state data parse error, key: %+v, value: %+v", string(batch.Key), batch.Value)
							}

							if ok := IsJson(contractData); ok {
								err := json.Unmarshal(contractData, &value)
								if err != nil {
									log.Errorf("state data not unmarshal json, key: %+v, value: %+v", string(batch.Key), string(batch.Value))
								}
								log.Debugf("exec start store key:: %+v, %+v, %+v", string(batch.Key), reflect.TypeOf(value), batch.Value)

								switch value.(type) {
								case map[string]interface{}:
									bulk.Upsert(bson.M{"_id": string(batch.Key)}, value)
								default:
									bulk.Upsert(bson.M{"_id": string(batch.Key)}, bson.M{"data": value})
								}
							} else {
								log.Errorf("state data not json %+v, %+v", string(contractData), contractData)
								break Out
							}
						} else {
							switch colName {
							case ledger.state.GetAssetCF():
								var asset state.Asset
								utils.Deserialize(batch.Value, &asset)
								bal, _ := json.Marshal(asset)
								json.Unmarshal(bal, &value)
							case ledger.state.GetBalanceCF():
								var balance state.Balance
								utils.Deserialize(batch.Value, &balance)
								bal, _ := json.Marshal(balance)
								json.Unmarshal(bal, &value)
								log.Infof("balance: %+v", balance)
							case ledger.state.GetChainCodeCF():
							case ledger.block.GetBlockCF():
								var blockHeader types.BlockHeader
								utils.Deserialize(batch.Value, &blockHeader)
								bal, _ := json.Marshal(blockHeader)
								json.Unmarshal(bal, &value)
							case ledger.block.GetTransactionCF():
								var tx types.Transaction
								utils.Deserialize(batch.Value, &tx)
								bal, _ := json.Marshal(tx)
								json.Unmarshal(bal, &value)
								switch tp := tx.GetType(); tp {
								case types.TypeJSContractInit, types.TypeLuaContractInit, types.TypeContractInvoke:
								case types.TypeSecurity:
								default:
									balance, err := notify.GetBalanceByTxInCallbacks(&tx)
									if err != nil {
										log.Errorf("GetBalanceByTxInCallbacks, err: %+v", err)
										batchesBlockChan <- batches
										break Out
									}

									notify.RemoveTxInCallbacks(&tx)
									value.(map[string]interface{})["sender_balance"] = balance.Sender.Int64()
									value.(map[string]interface{})["receiver_balance"] = balance.Recipient.Int64()
								}

							case ledger.block.GetIndexCF():
								txs, ok := ledger.block.GetTransactionInBlock(batch.Value, batch.Typ)
								if ok != true {
									continue
								}
								bal, _ := json.Marshal(txs)
								json.Unmarshal(bal, &value)
							default:
								continue
							}
							bulk.Upsert(bson.M{"_id": utils.BytesToHex(batch.Key)}, value)
						}
					} else if batch.Operation == db.OperationDelete {
						bulk.Remove(bson.M{"_id": string(batch.Key)})
					}

					writeBatchCnt++
					if writeBatchCnt > 800 {
						_, err = bulk.Run()
						if err != nil {
							log.Errorf("Col: %+v, bulk run err: %+v", colName, err)
							batchesBlockChan <- batches
							break Out
						}
						bulk = ledger.mdb.Coll(colName).Bulk()
						writeBatchCnt = 0
					}
				}

				_, err = bulk.Run()
				if err != nil {
					log.Errorf("Col: %+v, bulk run err: %+v", colName, err)
					batchesBlockChan <- batches
					break Out
				}
			}
		}
	}

	//in case not to block the main process
	for {
		select {
		case batches := <-ledger.mdbChan:
			_ = batches
			log.Warn("recv new batches, but no handle")
		case batches := <-batchesBlockChan:
			go ledger.writeBlock(batches)
		}
	}
}

// VerifyChain verifys the blockchain data
func (ledger *Ledger) VerifyChain() {
	height, err := ledger.Height()
	if err != nil {
		log.Panicf("VerifyChain -- Height %s", err)
	}
	currentBlockHeader, err := ledger.block.GetBlockByNumber(height)
	for i := height; i >= 1; i-- {
		previousBlockHeader, err := ledger.block.GetBlockByNumber(i - 1) // storage
		if previousBlockHeader != nil && err != nil {
			log.Panicf("VerifyChain -- GetBlockByNumber %s", err)
		}
		// verify previous block
		if !previousBlockHeader.Hash().Equal(currentBlockHeader.PreviousHash) {
			log.Panicf("VerifyChain -- block [%d] mismatch, veifychain breaks", i)
		}
		currentBlockHeader = previousBlockHeader
	}
}

// GetGenesisBlock returns the genesis block of the ledger
func (ledger *Ledger) GetGenesisBlock() *types.BlockHeader {
	genesisBlockHeader, err := ledger.GetBlockByNumber(0)
	if err != nil {
		panic(err)
	}
	return genesisBlockHeader
}

// AppendBlock appends a new block to the ledger,flag = true pack up block ,flag = false sync block
func (ledger *Ledger) AppendBlock(block *types.Block, flag bool) error {
	var (
		txWriteBatchs []*db.WriteBatch
		txs           types.Transactions
		errTxs        types.Transactions
	)

	// bh, _ := ledger.Height()
	// ledger.state.SetHeight(bh)

	// txWriteBatchs, block.Transactions, errTxs = ledger.executeTransactions(block.Transactions, flag)

	// block.Header.TxsMerkleHash = merkleRootHash(block.Transactions)
	// writeBatchs := ledger.block.AppendBlock(block)
	// writeBatchs = append(writeBatchs, txWriteBatchs...)
	// writeBatchs = append(writeBatchs, ledger.state.WriteBatchs()...)
	// if err := ledger.dbHandler.AtomicWrite(writeBatchs); err != nil {
	// 	return err
	// }
	// if params.Nvp && params.Mongodb {
	// 	ledger.mdbChan <- writeBatchs
	// }

	// ledger.Validator.RemoveTxsInVerification(errTxs)
	// ledger.Validator.RemoveTxsInVerification(block.Transactions)

	// for _, tx := range block.Transactions {
	// 	if (tx.GetType() == types.TypeMerged && !ledger.checkCoordinate(tx)) || tx.GetType() == types.TypeAcrossChain {
	// 		txs = append(txs, tx)
	// 	}
	// }
	// if err := ledger.storage.ClassifiedTransaction(txs); err != nil {
	// 	return err
	// }
	// log.Infoln("blockHeight: ", block.Height(), "need merge Txs len : ", len(txs), "all Txs len: ", len(block.Transactions))

	return nil
}

// GetBlockByNumber gets the block by the given number
func (ledger *Ledger) GetBlockByNumber(number uint32) (*types.BlockHeader, error) {
	return ledger.block.GetBlockByNumber(number)
}

// GetBlockByHash returns the block detail by hash
func (ledger *Ledger) GetBlockByHash(blockHashBytes []byte) (*types.BlockHeader, error) {
	return ledger.block.GetBlockByHash(blockHashBytes)
}

//GetTransactionHashList returns transactions hash list by block number
func (ledger *Ledger) GetTransactionHashList(number uint32) ([]crypto.Hash, error) {

	txHashsBytes, err := ledger.block.GetTransactionHashList(number)
	if err != nil {
		return nil, err
	}

	txHashs := []crypto.Hash{}

	utils.Deserialize(txHashsBytes, &txHashs)

	return txHashs, nil
}

// Height returns height of ledger
func (ledger *Ledger) Height() (uint32, error) {
	return ledger.block.GetBlockchainHeight()
}

//ComplexQuery com
func (ledger *Ledger) ComplexQuery(key string) ([]byte, error) {
	return ledger.state.ComplexQuery(key)
}

//GetLastBlockHash returns last block hash
func (ledger *Ledger) GetLastBlockHash() (crypto.Hash, error) {
	height, err := ledger.block.GetBlockchainHeight()
	if err != nil {
		return crypto.Hash{}, err
	}
	lastBlock, err := ledger.block.GetBlockByNumber(height)
	if err != nil {
		return crypto.Hash{}, err
	}
	return lastBlock.Hash(), nil
}

//GetBlockHashByNumber returns block hash by block number
func (ledger *Ledger) GetBlockHashByNumber(blockNum uint32) (crypto.Hash, error) {

	hashBytes, err := ledger.block.GetBlockHashByNumber(blockNum)
	if err != nil {
		return crypto.Hash{}, err
	}

	blockHash := new(crypto.Hash)

	blockHash.SetBytes(hashBytes)

	return *blockHash, err
}

// GetTxsByBlockHash returns transactions  by block hash and transactionType
func (ledger *Ledger) GetTxsByBlockHash(blockHashBytes []byte, transactionType uint32) (types.Transactions, error) {
	return ledger.block.GetTransactionsByHash(blockHashBytes, transactionType)
}

//GetTxsByBlockNumber returns transactions by blcokNumber and transactionType
func (ledger *Ledger) GetTxsByBlockNumber(blockNumber uint32, transactionType uint32) (types.Transactions, error) {
	return ledger.block.GetTransactionsByNumber(blockNumber, transactionType)
}

//GetTxByTxHash returns transaction by tx hash []byte
func (ledger *Ledger) GetTxByTxHash(txHashBytes []byte) (*types.Transaction, error) {
	return ledger.block.GetTransactionByTxHash(txHashBytes)
}

// GetBalances returns balance by account
func (ledger *Ledger) GetBalances(addr accounts.Address) (*state.Balance, error) {
	return ledger.state.GetBalances(addr.String())
}

// GetAsset returns asset
func (ledger *Ledger) GetAsset(id uint32) (*state.Asset, error) {
	return ledger.state.GetAsset(id)
}

// GetAssets returns assets
func (ledger *Ledger) GetAssets() (map[uint32]*state.Asset, error) {
	return ledger.state.GetAssets()
}

//GetMergedTransaction returns merged transaction within a specified period of time
func (ledger *Ledger) GetMergedTransaction(duration uint32) (types.Transactions, error) {

	txs, err := ledger.storage.GetMergedTransaction(duration)
	if err != nil {
		return nil, err
	}
	return txs, nil
}

//PutTxsHashByMergeTxHash put transactions hashs by merge transaction hash
func (ledger *Ledger) PutTxsHashByMergeTxHash(mergeTxHash crypto.Hash, txsHashs []crypto.Hash) error {
	return ledger.storage.PutTxsHashByMergeTxHash(mergeTxHash, txsHashs)
}

//GetTxsByMergeTxHash gets transactions
func (ledger *Ledger) GetTxsByMergeTxHash(mergeTxHash crypto.Hash) (types.Transactions, error) {
	txsHashs, err := ledger.storage.GetTxsByMergeTxHash(mergeTxHash)
	if err != nil {
		return nil, err
	}

	txs := types.Transactions{}
	for _, v := range txsHashs {
		tx, err := ledger.GetTxByTxHash(v.Bytes())
		if err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	return txs, nil
}

//QueryContract processes new contract query transaction
func (ledger *Ledger) QueryContract(tx *types.Transaction) ([]byte, error) {
	return ledger.state.QueryContract(tx)
}

// init generates the genesis block
func (ledger *Ledger) init() error {

	// genesis block
	blockHeader := new(types.BlockHeader)
	blockHeader.TimeStamp = uint32(0)
	blockHeader.Nonce = uint32(100)
	blockHeader.Height = 0

	genesisBlock := new(types.Block)
	genesisBlock.Header = blockHeader

	for _, addr := range params.PublicAddress {
		sender := accounts.HexToAddress(addr)
		issueTx := types.NewTransaction{
			params.ChainID,
			params.ChainID,
			types.TypeIssue,
			uint32(0),
			sender,
			sender,
			uint32(0),
			big.NewInt(0),
			big.NewInt(0),
			uint32(0),
		}
		issueCoin := make(map[string]interface{})
		issueCoin["id"] = uint32(0)
		tx.Payload, _ = json.Marshal(issueCoin)
		sig, _ := issueKey.Sign(tx.SignHash().Bytes())
		genesisBlock.Transactions = append(genesisBlock.Transactions, issueTx)
		break
	}

	writeBatchs := ledger.block.AppendBlock(genesisBlock)

	// admin address
	buf, err := contract.ConcrateStateJson(contract.DefaultAdminAddr)
	if err != nil {
		return err
	}

	writeBatchs = append(writeBatchs,
		db.NewWriteBatch(contract.ColumnFamily,
			db.OperationPut,
			[]byte(contract.EnSmartContractKey(params.GlobalStateKey, params.AdminKey)),
			buf.Bytes(), contract.ColumnFamily))

	// global contract
	buf, err = contract.ConcrateStateJson(&contract.DefaultGlobalContract)
	if err != nil {
		return err
	}

	writeBatchs = append(writeBatchs,
		db.NewWriteBatch(contract.ColumnFamily,
			db.OperationPut,
			[]byte(contract.EnSmartContractKey(params.GlobalStateKey, params.GlobalContractKey)),
			buf.Bytes(), contract.ColumnFamily))

	err = ledger.dbHandler.AtomicWrite(writeBatchs)
	if err != nil {
		return err
	}
	if params.Nvp && params.Mongodb {
		ledger.mdb.RegisterCollection(params.GlobalStateKey)
		ledger.mdbChan <- writeBatchs
	}

	return err

}

func (ledger *Ledger) checkCoordinate(tx *types.Transaction) bool {
	fromChainID := coordinate.HexToChainCoordinate(tx.FromChain()).Bytes()
	toChainID := coordinate.HexToChainCoordinate(tx.ToChain()).Bytes()
	if bytes.Equal(fromChainID, toChainID) {
		return true
	}
	return false
}

func (ledger *Ledger) writeBlock(data interface{}) error {
	var bvalue []byte
	switch data.(type) {
	case []*db.WriteBatch:
		orgData := data.([]*db.WriteBatch)
		bvalue = utils.Serialize(orgData)
	}

	height, err := ledger.Height()
	if err != nil {
		log.Errorf("can't get blockchain height")
	}

	path := ledger.conf.ExceptBlockDir + string(filepath.Separator)
	fileName := path + strconv.Itoa(int(height))
	if utils.FileExist(fileName) {
		log.Infof("BlockChan have error, please check ...")
		return errors.New("except block have existed")
	}

	err = ioutil.WriteFile(fileName, bvalue, 0666)
	if err != nil {
		log.Errorf("write file: %s fail err: %+v", fileName, err)
	}
	return err
}

func merkleRootHash(txs []*types.Transaction) crypto.Hash {
	if len(txs) > 0 {
		hashs := make([]crypto.Hash, 0)
		for _, tx := range txs {
			hashs = append(hashs, tx.Hash())
		}
		return crypto.ComputeMerkleHash(hashs)[0]
	}
	return crypto.Hash{}
}

func IsJson(src []byte) bool {
	var value interface{}
	return json.Unmarshal(src, &value) == nil
}
