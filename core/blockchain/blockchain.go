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

package blockchain

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/bocheninc/L0/components/crypto"
	"github.com/bocheninc/L0/components/log"
	"github.com/bocheninc/L0/core/blockchain/validator"
	"github.com/bocheninc/L0/core/consensus"
	"github.com/bocheninc/L0/core/ledger"
	"github.com/bocheninc/L0/core/notify"
	"github.com/bocheninc/L0/core/types"
)

// NetworkStack defines the relay interface
type NetworkStack interface {
	Relay(inv types.IInventory)
}

// Blockchain is blockchain instance
type Blockchain struct {
	mu                 sync.Mutex
	wg                 sync.WaitGroup
	currentBlockHeader *types.BlockHeader
	ledger             *ledger.Ledger
	// validator
	validator validator.Validator
	// consensus
	consenter consensus.Consenter
	// network stack
	pm NetworkStack

	quitCh chan bool
	txCh   chan *types.Transaction
	blkCh  chan *types.Block

	orphans *list.List
	// 0 respresents sync block, 1 respresents sync done
	synced bool
}

// load loads local blockchain data
func (bc *Blockchain) load() {
	log.Debugf("========start load========")
	t := time.Now()
	//bc.ledger.VerifyChain()
	delay := time.Since(t)

	height, err := bc.ledger.Height()

	if err != nil {
		log.Error("GetBlockHeight error", err)
		return
	}
	bc.currentBlockHeader, err = bc.ledger.GetBlockByNumber(height)

	if bc.currentBlockHeader == nil || err != nil {
		log.Errorf("GetBlockByNumber error %v ", err)
		panic(err)
	}

	log.Debugf("Load blockchain data, bestblockhash: %s height: %d load delay : %v ", bc.currentBlockHeader.Hash(), height, delay)
}

// NewBlockchain returns a fully initialised blockchain service using input data
func NewBlockchain(ledger *ledger.Ledger) *Blockchain {
	bc := &Blockchain{
		mu:                 sync.Mutex{},
		wg:                 sync.WaitGroup{},
		ledger:             ledger,
		quitCh:             make(chan bool),
		txCh:               make(chan *types.Transaction, 10000),
		blkCh:              make(chan *types.Block, 10),
		currentBlockHeader: new(types.BlockHeader),
		orphans:            list.New(),
	}
	bc.load()
	return bc
}

// SetBlockchainValidator sets the validator of the blockchain
func (bc *Blockchain) SetBlockchainValidator(validator validator.Validator) {
	bc.validator = validator
	bc.ledger.Validator = bc.validator
}

// SetBlockchainConsenter sets the consenter of the blockchain
func (bc *Blockchain) SetBlockchainConsenter(consenter consensus.Consenter) {
	bc.consenter = consenter
	if bc.consenter.Name() == "noops" {
		bc.Start()
	}
}

// SetNetworkStack sets the node of the blockchain
func (bc *Blockchain) SetNetworkStack(pm NetworkStack) {
	bc.pm = pm
}

// CurrentHeight returns current heigt of the current block
func (bc *Blockchain) CurrentHeight() uint32 {
	return bc.currentBlockHeader.Height
}

// CurrentBlockHash returns current block hash of the current block
func (bc *Blockchain) CurrentBlockHash() crypto.Hash {
	return bc.currentBlockHeader.Hash()
}

// GetNextBlockHash returns the next block hash
func (bc *Blockchain) GetNextBlockHash(h crypto.Hash) (crypto.Hash, error) {
	blockHeader, err := bc.ledger.GetBlockByHash(h.Bytes())
	if blockHeader == nil || err != nil {
		return h, err
	}
	nextBlockHeader, err := bc.ledger.GetBlockByNumber(blockHeader.Height + 1)
	if nextBlockHeader == nil || err != nil {
		return h, err
	}
	hash := nextBlockHeader.Hash()
	return hash, nil
}

// GetTransaction returns transaction in ledger first then txBool
func (bc *Blockchain) GetTransaction(txHash crypto.Hash) (*types.Transaction, error) {
	tx, err := bc.ledger.GetTxByTxHash(txHash.Bytes())
	if bc.validator != nil && err != nil {
		var ok bool
		if tx, ok = bc.validator.GetTransactionByHash(txHash); ok {
			return tx, nil
		}
	}

	return tx, err
}

// Start starts blockchain services
func (bc *Blockchain) Start() {
	// bc.wg.Add(1)
	// start consesnus
	bc.StartConsensusService()
	// start txpool
	bc.StartTxPoolService()
	log.Debug("BlockChain Service start")
	// bc.wg.Wait()
}

func (bc *Blockchain) Synced() bool {
	return bc.synced
}

var allProcessBlock time.Duration
var allBlockCnt int64
var allTransactionCnt int64

// StartConsensusService starts consensus service
func (bc *Blockchain) StartConsensusService() {
	go bc.consenter.Start()
	go func() {
		for {
			select {
			case commitedTxs := <-bc.consenter.OutputTxsChannel():
				startTime := time.Now()
				//add lo
				log.Infof("Outputs StartConsensusService len=%d", len(commitedTxs.Txs))

				height, _ := bc.ledger.Height()
				height++
				if commitedTxs.Height == height {
					if !bc.synced {
						bc.synced = true
					}
					bc.processConsensusOutput(commitedTxs)
				} else if commitedTxs.Height > height {
					//orphan
					bc.orphans.PushBack(commitedTxs)
					for elem := bc.orphans.Front(); elem != nil; elem = elem.Next() {
						ocommitedTxs := elem.Value.(*consensus.OutputTxs)
						if ocommitedTxs.Height < height {
							bc.orphans.Remove(elem)
						} else if ocommitedTxs.Height == height {
							bc.orphans.Remove(elem)
							bc.processConsensusOutput(ocommitedTxs)
							height++
						} else {
							break
						}
					}
					if bc.orphans.Len() > 100 {
						bc.orphans.Remove(bc.orphans.Front())
					}
					//bc.orphans.PushBack(commitedTxs)
				} /*else if bc.synced {
					log.Panicf("Height %d already exist in ledger", commitedTxs.Height)
				}*/

				oneBlockTime := time.Now().Sub(startTime)
				allProcessBlock += oneBlockTime
				allBlockCnt += 1
				allTransactionCnt += int64(len(commitedTxs.Txs))
				if allBlockCnt%10 == 0 {
					avg_blk_time := allProcessBlock.Nanoseconds() / allBlockCnt / 1000 / 1000
					avg_tx_time := allProcessBlock.Nanoseconds() / allTransactionCnt / 1000 / 1000
					log.Debugf("WriteLedger, blockCnt: %d, txCnt: %d, avg_blk_time: %d, avg_tx_time: %d", allBlockCnt, allTransactionCnt, avg_blk_time, avg_tx_time)
				}
				log.Debugf("WriteLedger, blk_ht: %+v , tx_nums: %+v, time: %s", height, len(commitedTxs.Txs), oneBlockTime)

			}
		}
	}()
}

func (bc *Blockchain) processConsensusOutput(output *consensus.OutputTxs) {
	blk := bc.GenerateBlock(output.Txs, output.Time)
	if blk.Height() == output.Height {
		bc.pm.Relay(blk)
	}
}

// StartTxPool starts txpool service
func (bc *Blockchain) StartTxPoolService() {
	bc.validator.Start()
}

// ProcessTransaction processes new transaction from the network
func (bc *Blockchain) ProcessTransaction(tx *types.Transaction, needNotify bool) bool {
	// step 1: validate and mark transaction
	// step 2: add transaction to txPool
	// if atomic.LoadUint32(&bc.synced) == 0 {
	log.Debugf("[Blockchain] new tx, tx_hash: %v, tx_sender: %v, tx_nonce: %v", tx.Hash().String(), tx.Sender().String(), tx.Nonce())
	if bc.validator == nil {
		return true
	}

	err := bc.validator.ProcessTransaction(tx)
	log.Debugf("[Blockchain] new tx, tx_hash: %v, tx_sender: %v, tx_nonce: %v, end", tx.Hash().String(), tx.Sender().String(), tx.Nonce())
	if err != nil {
		if needNotify {
			notify.TxNotify(tx, fmt.Errorf("process transaction %v failed, %v", tx.Hash(), err))
		}
		log.Errorf(fmt.Sprintf("process transaction %v failed, %v", tx.Hash(), err))
		return false
	}

	return true
}

// ProcessBlock processes new block from the network,flag = true pack up block ,flag = false sync block
func (bc *Blockchain) ProcessBlock(blk *types.Block, flag bool) bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	log.Debugf("block previoushash %s, currentblockhash %s,len %d", blk.PreviousHash(), bc.CurrentBlockHash(), len(blk.Transactions))
	if blk.PreviousHash() == bc.CurrentBlockHash() {
		bc.ledger.AppendBlock(blk, flag)
		notify.BlockNotify(blk)
		log.Infof("New Block  %s, height: %d Transaction Number: %d", blk.Hash(), blk.Height(), len(blk.Transactions))
		bc.currentBlockHeader = blk.Header
		return true
	}
	return false
}

func (bc *Blockchain) merkleRootHash(txs []*types.Transaction) crypto.Hash {
	if len(txs) > 0 {
		hashs := make([]crypto.Hash, 0)
		for _, tx := range txs {
			hashs = append(hashs, tx.Hash())
		}
		return crypto.ComputeMerkleHash(hashs)[0]
	}
	return crypto.Hash{}
}

// GenerateBlock gets transactions from consensus service and generates a new block
func (bc *Blockchain) GenerateBlock(txs types.Transactions, createTime uint32) *types.Block {
	var (
		// default value is empty hash
		merkleRootHash crypto.Hash
	)

	blk := types.NewBlock(bc.currentBlockHeader.Hash(),
		createTime, bc.currentBlockHeader.Height+1,
		uint32(100),
		merkleRootHash,
		txs,
	)
	return blk
}
