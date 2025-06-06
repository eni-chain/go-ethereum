// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/bloombits"
	"github.com/ethereum/go-ethereum/core/rawdb"

	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// EthAPIBackendV2 implements ethapi.Backend and tracers.Backend for full nodes
type EthAPIBackendV2 struct {
	extRPCEnabled       bool
	allowUnprotectedTxs bool
	eth                 *Ethereum
	gpo                 *gasprice.Oracle
}

// ChainConfig returns the active chain configuration.
func (b *EthAPIBackendV2) ChainConfig() *params.ChainConfig {
	return b.eth.blockchain.Config()
}

func (b *EthAPIBackendV2) CurrentBlock() *types.Header {
	return b.eth.blockchain.CurrentBlock()
}

func (b *EthAPIBackendV2) SetHead(number uint64) {
	b.eth.handler.downloader.Cancel()
	b.eth.blockchain.SetHead(number)
}

func (b *EthAPIBackendV2) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	// Pending block is only known by the miner
	if number == rpc.PendingBlockNumber {
		block, _, _ := b.eth.miner.Pending()
		if block == nil {
			return nil, errors.New("pending block is not available")
		}
		return block.Header(), nil
	}
	// Otherwise resolve and return the block
	if number == rpc.LatestBlockNumber {
		return b.eth.blockchain.CurrentBlock(), nil
	}
	if number == rpc.FinalizedBlockNumber {
		block := b.eth.blockchain.CurrentFinalBlock()
		if block == nil {
			return nil, errors.New("finalized block not found")
		}
		return block, nil
	}
	if number == rpc.SafeBlockNumber {
		block := b.eth.blockchain.CurrentSafeBlock()
		if block == nil {
			return nil, errors.New("safe block not found")
		}
		return block, nil
	}
	return b.eth.blockchain.GetHeaderByNumber(uint64(number)), nil
}

func (b *EthAPIBackendV2) HeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Header, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.HeaderByNumber(ctx, blockNr)
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		header := b.eth.blockchain.GetHeaderByHash(hash)
		if header == nil {
			return nil, errors.New("header for hash not found")
		}
		if blockNrOrHash.RequireCanonical && b.eth.blockchain.GetCanonicalHash(header.Number.Uint64()) != hash {
			return nil, errors.New("hash is not currently canonical")
		}
		return header, nil
	}
	return nil, errors.New("invalid arguments; neither block nor hash specified")
}

func (b *EthAPIBackendV2) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return b.eth.blockchain.GetHeaderByHash(hash), nil
}

func (b *EthAPIBackendV2) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	// Pending block is only known by the miner
	if number == rpc.PendingBlockNumber {
		block, _, _ := b.eth.miner.Pending()
		if block == nil {
			return nil, errors.New("pending block is not available")
		}
		return block, nil
	}
	// Otherwise resolve and return the block
	if number == rpc.LatestBlockNumber {
		header := b.eth.blockchain.CurrentBlock()
		return b.eth.blockchain.GetBlock(header.Hash(), header.Number.Uint64()), nil
	}
	if number == rpc.FinalizedBlockNumber {
		header := b.eth.blockchain.CurrentFinalBlock()
		if header == nil {
			return nil, errors.New("finalized block not found")
		}
		return b.eth.blockchain.GetBlock(header.Hash(), header.Number.Uint64()), nil
	}
	if number == rpc.SafeBlockNumber {
		header := b.eth.blockchain.CurrentSafeBlock()
		if header == nil {
			return nil, errors.New("safe block not found")
		}
		return b.eth.blockchain.GetBlock(header.Hash(), header.Number.Uint64()), nil
	}
	return b.eth.blockchain.GetBlockByNumber(uint64(number)), nil
}

func (b *EthAPIBackendV2) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return b.eth.blockchain.GetBlockByHash(hash), nil
}

// GetBody returns body of a block. It does not resolve special block numbers.
func (b *EthAPIBackendV2) GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error) {
	if number < 0 || hash == (common.Hash{}) {
		return nil, errors.New("invalid arguments; expect hash and no special block numbers")
	}
	if body := b.eth.blockchain.GetBody(hash); body != nil {
		return body, nil
	}
	return nil, errors.New("block body not found")
}

func (b *EthAPIBackendV2) BlockByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Block, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.BlockByNumber(ctx, blockNr)
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		header := b.eth.blockchain.GetHeaderByHash(hash)
		if header == nil {
			return nil, errors.New("header for hash not found")
		}
		if blockNrOrHash.RequireCanonical && b.eth.blockchain.GetCanonicalHash(header.Number.Uint64()) != hash {
			return nil, errors.New("hash is not currently canonical")
		}
		block := b.eth.blockchain.GetBlock(hash, header.Number.Uint64())
		if block == nil {
			return nil, errors.New("header found, but block body is missing")
		}
		return block, nil
	}
	return nil, errors.New("invalid arguments; neither block nor hash specified")
}

func (b *EthAPIBackendV2) Pending() (*types.Block, types.Receipts, vm.StateDB) {
	return b.eth.miner.Pending()
}

func (b *EthAPIBackendV2) StateAndHeaderByNumber(ctx context.Context, number rpc.BlockNumber) (vm.StateDB, *types.Header, error) {
	// Pending state is only known by the miner
	if number == rpc.PendingBlockNumber {
		block, _, state := b.eth.miner.Pending()
		if block == nil || state == nil {
			return nil, nil, errors.New("pending state is not available")
		}
		return state, block.Header(), nil
	}
	// Otherwise resolve the block number and return its state
	header, err := b.HeaderByNumber(ctx, number)
	if err != nil {
		return nil, nil, err
	}
	if header == nil {
		return nil, nil, errors.New("header not found")
	}
	stateDb, err := b.eth.BlockChain().StateAt(header.Root)
	if err != nil {
		return nil, nil, err
	}
	return stateDb, header, nil
}

func (b *EthAPIBackendV2) StateAndHeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (vm.StateDB, *types.Header, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.StateAndHeaderByNumber(ctx, blockNr)
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		header, err := b.HeaderByHash(ctx, hash)
		if err != nil {
			return nil, nil, err
		}
		if header == nil {
			return nil, nil, errors.New("header for hash not found")
		}
		if blockNrOrHash.RequireCanonical && b.eth.blockchain.GetCanonicalHash(header.Number.Uint64()) != hash {
			return nil, nil, errors.New("hash is not currently canonical")
		}
		stateDb, err := b.eth.BlockChain().StateAt(header.Root)
		if err != nil {
			return nil, nil, err
		}
		return stateDb, header, nil
	}
	return nil, nil, errors.New("invalid arguments; neither block nor hash specified")
}

func (b *EthAPIBackendV2) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	return b.eth.blockchain.GetReceiptsByHash(hash), nil
}

func (b *EthAPIBackendV2) GetLogs(ctx context.Context, hash common.Hash, number uint64) ([][]*types.Log, error) {
	return rawdb.ReadLogs(b.eth.chainDb, hash, number), nil
}

func (b *EthAPIBackendV2) GetEVM(ctx context.Context, state vm.StateDB, header *types.Header, vmConfig *vm.Config, blockCtx *vm.BlockContext) *vm.EVM {
	if vmConfig == nil {
		vmConfig = b.eth.blockchain.GetVMConfig()
	}
	var context vm.BlockContext
	if blockCtx != nil {
		context = *blockCtx
	} else {
		context = core.NewEVMBlockContext(header, b.eth.BlockChain(), nil)
	}
	return vm.NewEVM(context, state, b.ChainConfig(), *vmConfig)
}

func (b *EthAPIBackendV2) SubscribeRemovedLogsEvent(ch chan<- core.RemovedLogsEvent) event.Subscription {
	return b.eth.BlockChain().SubscribeRemovedLogsEvent(ch)
}

func (b *EthAPIBackendV2) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return b.eth.BlockChain().SubscribeChainEvent(ch)
}

func (b *EthAPIBackendV2) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return b.eth.BlockChain().SubscribeChainHeadEvent(ch)
}

func (b *EthAPIBackendV2) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return b.eth.BlockChain().SubscribeLogsEvent(ch)
}

func (b *EthAPIBackendV2) SendTx(ctx context.Context, signedTx *types.Transaction) error {
	if locals := b.eth.localTxTracker; locals != nil {
		locals.Track(signedTx)
	}
	return b.eth.txPool.Add([]*types.Transaction{signedTx}, false)[0]
}

func (b *EthAPIBackendV2) GetPoolTransactions() (types.Transactions, error) {
	pending := b.eth.txPool.Pending(txpool.PendingFilter{})
	var txs types.Transactions
	for _, batch := range pending {
		for _, lazy := range batch {
			if tx := lazy.Resolve(); tx != nil {
				txs = append(txs, tx)
			}
		}
	}
	return txs, nil
}

func (b *EthAPIBackendV2) GetPoolTransaction(hash common.Hash) *types.Transaction {
	return b.eth.txPool.Get(hash)
}

// GetTransaction retrieves the lookup along with the transaction itself associate
// with the given transaction hash.
//
// An error will be returned if the transaction is not found, and background
// indexing for transactions is still in progress. The error is used to indicate the
// scenario explicitly that the transaction might be reachable shortly.
//
// A null will be returned in the transaction is not found and background transaction
// indexing is already finished. The transaction is not existent from the perspective
// of node.
func (b *EthAPIBackendV2) GetTransaction(ctx context.Context, txHash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64, error) {
	lookup, tx, err := b.eth.blockchain.GetTransactionLookup(txHash)
	if err != nil {
		return false, nil, common.Hash{}, 0, 0, err
	}
	if lookup == nil || tx == nil {
		return false, nil, common.Hash{}, 0, 0, nil
	}
	return true, tx, lookup.BlockHash, lookup.BlockIndex, lookup.Index, nil
}

func (b *EthAPIBackendV2) GetPoolNonce(ctx context.Context, addr common.Address) (uint64, error) {
	return b.eth.txPool.Nonce(addr), nil
}

func (b *EthAPIBackendV2) Stats() (runnable int, blocked int) {
	return b.eth.txPool.Stats()
}

func (b *EthAPIBackendV2) TxPoolContent() (map[common.Address][]*types.Transaction, map[common.Address][]*types.Transaction) {
	return b.eth.txPool.Content()
}

func (b *EthAPIBackendV2) TxPoolContentFrom(addr common.Address) ([]*types.Transaction, []*types.Transaction) {
	return b.eth.txPool.ContentFrom(addr)
}

func (b *EthAPIBackendV2) TxPool() *txpool.TxPool {
	return b.eth.txPool
}

func (b *EthAPIBackendV2) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return b.eth.txPool.SubscribeTransactions(ch, true)
}

func (b *EthAPIBackendV2) SyncProgress() ethereum.SyncProgress {
	prog := b.eth.Downloader().Progress()
	if txProg, err := b.eth.blockchain.TxIndexProgress(); err == nil {
		prog.TxIndexFinishedBlocks = txProg.Indexed
		prog.TxIndexRemainingBlocks = txProg.Remaining
	}
	return prog
}

func (b *EthAPIBackendV2) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	return b.gpo.SuggestTipCap(ctx)
}

func (b *EthAPIBackendV2) FeeHistory(ctx context.Context, blockCount uint64, lastBlock rpc.BlockNumber, rewardPercentiles []float64) (firstBlock *big.Int, reward [][]*big.Int, baseFee []*big.Int, gasUsedRatio []float64, baseFeePerBlobGas []*big.Int, blobGasUsedRatio []float64, err error) {
	return b.gpo.FeeHistory(ctx, blockCount, lastBlock, rewardPercentiles)
}

func (b *EthAPIBackendV2) BlobBaseFee(ctx context.Context) *big.Int {
	if excess := b.CurrentHeader().ExcessBlobGas; excess != nil {
		return eip4844.CalcBlobFee(b.ChainConfig(), b.CurrentHeader())
	}
	return nil
}

func (b *EthAPIBackendV2) ChainDb() ethdb.Database {
	return b.eth.ChainDb()
}

func (b *EthAPIBackendV2) EventMux() *event.TypeMux {
	return b.eth.EventMux()
}

func (b *EthAPIBackendV2) AccountManager() *accounts.Manager {
	return b.eth.AccountManager()
}

func (b *EthAPIBackendV2) ExtRPCEnabled() bool {
	return b.extRPCEnabled
}

func (b *EthAPIBackendV2) UnprotectedAllowed() bool {
	return b.allowUnprotectedTxs
}

func (b *EthAPIBackendV2) RPCGasCap() uint64 {
	return b.eth.config.RPCGasCap
}

func (b *EthAPIBackendV2) RPCEVMTimeout() time.Duration {
	return b.eth.config.RPCEVMTimeout
}

func (b *EthAPIBackendV2) RPCTxFeeCap() float64 {
	return b.eth.config.RPCTxFeeCap
}

func (b *EthAPIBackendV2) BloomStatus() (uint64, uint64) {
	sections, _, _ := b.eth.bloomIndexer.Sections()
	return params.BloomBitsBlocks, sections
}

func (b *EthAPIBackendV2) ServiceFilter(ctx context.Context, session *bloombits.MatcherSession) {
	for i := 0; i < bloomFilterThreads; i++ {
		go session.Multiplex(bloomRetrievalBatch, bloomRetrievalWait, b.eth.bloomRequests)
	}
}

func (b *EthAPIBackendV2) Engine() consensus.Engine {
	return b.eth.engine
}

func (b *EthAPIBackendV2) CurrentHeader() *types.Header {
	return b.eth.blockchain.CurrentHeader()
}

func (b *EthAPIBackendV2) StateAtBlock(ctx context.Context, block *types.Block, reexec uint64, base vm.StateDB, readOnly bool, preferDisk bool) (vm.StateDB, tracers.StateReleaseFunc, error) {
	//return b.eth.stateAtBlock(ctx, block, reexec, base, readOnly, preferDisk)
	return base, nil, nil
}

func (b *EthAPIBackendV2) StateAtTransaction(ctx context.Context, block *types.Block, txIndex int, reexec uint64) (*types.Transaction, vm.BlockContext, vm.StateDB, tracers.StateReleaseFunc, error) {
	return b.eth.stateAtTransaction(ctx, block, txIndex, reexec)
}
