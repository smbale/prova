// Copyright (c) 2014-2016 The btcsuite developers
// Copyright (c) 2017 BitGo
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"container/heap"
	"encoding/hex"
	"time"

	"github.com/bitgo/prova/blockchain"
	"github.com/bitgo/prova/btcec"
	"github.com/bitgo/prova/chaincfg/chainhash"
	"github.com/bitgo/prova/mempool"
	"github.com/bitgo/prova/mining"
	"github.com/bitgo/prova/provautil"
	"github.com/bitgo/prova/txscript"
	"github.com/bitgo/prova/wire"
)

const (
	// generatedBlockVersion is the version of the block being generated.
	// It is defined as a constant here rather than using the
	// wire.BlockVersion constant since a change in the block version
	// will require changes to the generated block.  Using the wire constant
	// for generated block version could allow creation of invalid blocks
	// for the updated version.
	generatedBlockVersion = 4

	// blockHeaderOverhead is the max number of bytes it takes to serialize
	// a block header and max possible transaction count.
	blockHeaderOverhead = wire.MaxBlockHeaderPayload + wire.MaxVarIntPayload

	// coinbaseFlags is added to the coinbase script of a generated block
	// and is used to monitor BIP16 support as well as blocks that are
	// generated via btcd.
	coinbaseFlags = "/prova/"
)

// txPrioItem houses a transaction along with extra information that allows the
// transaction to be prioritized and track dependencies on other transactions
// which have not been mined into a block yet.
type txPrioItem struct {
	tx       *provautil.Tx
	fee      int64
	priority float64
	feePerKB int64
	isAdmin  bool

	// dependsOn holds a map of transaction hashes which this one depends
	// on.  It will only be set when the transaction references other
	// transactions in the source pool and hence must come after them in
	// a block.
	dependsOn map[chainhash.Hash]struct{}
}

// isAdmin returns whether or not this transaction has an admin txout
// scriptpub.
func isAdmin(msgTx *wire.MsgTx) bool {
	for _, txOut := range msgTx.TxOut {
		pops, err := txscript.ParseScript(txOut.PkScript)
		if err != nil {
			return false
		}
		scriptClass := txscript.TypeOfScript(pops)
		if scriptClass == txscript.ProvaAdminTy {
			return true
		}
	}
	return false
}

// txPriorityQueueLessFunc describes a function that can be used as a compare
// function for a transaction priority queue (txPriorityQueue).
type txPriorityQueueLessFunc func(*txPriorityQueue, int, int) bool

// txPriorityQueue implements a priority queue of txPrioItem elements that
// supports an arbitrary compare function as defined by txPriorityQueueLessFunc.
type txPriorityQueue struct {
	lessFunc txPriorityQueueLessFunc
	items    []*txPrioItem
}

// Len returns the number of items in the priority queue.  It is part of the
// heap.Interface implementation.
func (pq *txPriorityQueue) Len() int {
	return len(pq.items)
}

// Less returns whether the item in the priority queue with index i should sort
// before the item with index j by deferring to the assigned less function.  It
// is part of the heap.Interface implementation.
func (pq *txPriorityQueue) Less(i, j int) bool {
	return pq.lessFunc(pq, i, j)
}

// Swap swaps the items at the passed indices in the priority queue.  It is
// part of the heap.Interface implementation.
func (pq *txPriorityQueue) Swap(i, j int) {
	pq.items[i], pq.items[j] = pq.items[j], pq.items[i]
}

// Push pushes the passed item onto the priority queue.  It is part of the
// heap.Interface implementation.
func (pq *txPriorityQueue) Push(x interface{}) {
	pq.items = append(pq.items, x.(*txPrioItem))
}

// Pop removes the highest priority item (according to Less) from the priority
// queue and returns it.  It is part of the heap.Interface implementation.
func (pq *txPriorityQueue) Pop() interface{} {
	n := len(pq.items)
	item := pq.items[n-1]
	pq.items[n-1] = nil
	pq.items = pq.items[0 : n-1]
	return item
}

// SetLessFunc sets the compare function for the priority queue to the provided
// function.  It also invokes heap.Init on the priority queue using the new
// function so it can immediately be used with heap.Push/Pop.
func (pq *txPriorityQueue) SetLessFunc(lessFunc txPriorityQueueLessFunc) {
	pq.lessFunc = lessFunc
	heap.Init(pq)
}

// txPQByPriority sorts a txPriorityQueue by transaction priority and then fees
// per kilobyte.
func txPQByPriority(pq *txPriorityQueue, i, j int) bool {
	// Always prioritize admin transactions.
	if pq.items[i].isAdmin {
		return true
	}
	// Using > here so that pop gives the highest priority item as opposed
	// to the lowest.  Sort by priority first, then fee.
	if pq.items[i].priority == pq.items[j].priority {
		return pq.items[i].feePerKB > pq.items[j].feePerKB
	}
	return pq.items[i].priority > pq.items[j].priority

}

// txPQByFee sorts a txPriorityQueue by fees per kilobyte and then transaction
// priority.
func txPQByFee(pq *txPriorityQueue, i, j int) bool {
	// Always prioritize admin transactions.
	if pq.items[i].isAdmin {
		return true
	}
	// Using > here so that pop gives the highest fee item as opposed
	// to the lowest.  Sort by fee first, then priority.
	if pq.items[i].feePerKB == pq.items[j].feePerKB {
		return pq.items[i].priority > pq.items[j].priority
	}
	return pq.items[i].feePerKB > pq.items[j].feePerKB
}

// newTxPriorityQueue returns a new transaction priority queue that reserves the
// passed amount of space for the elements.  The new priority queue uses either
// the txPQByPriority or the txPQByFee compare function depending on the
// sortByFee parameter and is already initialized for use with heap.Push/Pop.
// The priority queue can grow larger than the reserved space, but extra copies
// of the underlying array can be avoided by reserving a sane value.
func newTxPriorityQueue(reserve int, sortByFee bool) *txPriorityQueue {
	pq := &txPriorityQueue{
		items: make([]*txPrioItem, 0, reserve),
	}
	if sortByFee {
		pq.SetLessFunc(txPQByFee)
	} else {
		pq.SetLessFunc(txPQByPriority)
	}
	return pq
}

// BlockTemplate houses a block that has yet to be solved along with additional
// details about the fees and the number of signature operations for each
// transaction in the block.
type BlockTemplate struct {
	// Block is a block that is ready to be solved by miners.  Thus, it is
	// completely valid with the exception of satisfying the proof-of-work
	// requirement.
	Block *wire.MsgBlock

	// Fees contains the amount of fees each transaction in the generated
	// template pays in base units.  Since the first transaction is the
	// coinbase, the first entry (offset 0) will contain the negative of the
	// sum of the fees of all other transactions.
	Fees []int64

	// SigOpCounts contains the number of signature operations each
	// transaction in the generated template performs.
	SigOpCounts []int64

	// Height is the height at which the block template connects to the main
	// chain.
	Height uint32

	// ValidPayAddress indicates whether or not the template coinbase pays
	// to an address or is redeemable by anyone.  See the documentation on
	// NewBlockTemplate for details on which this can be useful to generate
	// templates without a coinbase payment address.
	ValidPayAddress bool
}

// mergeUtxoView adds all of the entries in view to viewA.  The result is that
// viewA will contain all of its original entries plus all of the entries
// in viewB.  It will replace any entries in viewB which also exist in viewA
// if the entry in viewA is fully spent.
func mergeUtxoView(viewA *blockchain.UtxoViewpoint, viewB *blockchain.UtxoViewpoint) {
	viewAEntries := viewA.Entries()
	for hash, entryB := range viewB.Entries() {
		if entryA, exists := viewAEntries[hash]; !exists ||
			entryA == nil || entryA.IsFullySpent() {

			viewAEntries[hash] = entryB
		}
	}
}

// standardCoinbaseScript returns a standard script suitable for use as the
// signature script of the coinbase transaction of a new block.  In particular,
// it starts with the block height that is required by version 2 blocks and adds
// the extra nonce as well as additional coinbase flags.
func standardCoinbaseScript() ([]byte, error) {
	return txscript.NewScriptBuilder().AddData([]byte(coinbaseFlags)).
		Script()
}

// createCoinbaseTx returns a coinbase transaction paying an appropriate subsidy
// based on the passed block height to the provided address.  When the address
// is nil, the coinbase transaction will instead be redeemable by anyone.
//
// See the comment for NewBlockTemplate for more information about why the nil
// address handling is useful.
func createCoinbaseTx(coinbaseScript []byte, nextBlockHeight uint32, addr provautil.Address) (*provautil.Tx, error) {
	// Create the script to pay to the provided payment address if one was
	// specified.  Otherwise create a script that allows the coinbase to be
	// redeemable by anyone.
	var pkScript []byte
	if addr != nil {
		var err error
		pkScript, err = txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		scriptBuilder := txscript.NewScriptBuilder()
		pkScript, err = scriptBuilder.AddOp(txscript.OP_TRUE).Script()
		if err != nil {
			return nil, err
		}
	}

	tx := wire.NewMsgTx()
	tx.AddTxIn(&wire.TxIn{
		// Coinbase transactions have no inputs, so previous outpoint is
		// zero hash and max index.
		PreviousOutPoint: *wire.NewOutPoint(&chainhash.Hash{},
			wire.MaxPrevOutIndex),
		SignatureScript: coinbaseScript,
		Sequence:        wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{
		Value: blockchain.CalcBlockSubsidy(nextBlockHeight,
			activeNetParams.Params),
		PkScript: pkScript,
	})

	// Add block height as a locktime to make a unique txid.
	// Since BIP30 transactions are required to have unique txids. This is
	// normally covered with the block height in the coinbase scriptSig.
	// Since scriptSigs have been eliminated from the txid, dummy locktime
	// with the block height is created to add back uniqueness to the hash.
	// There is no consensus rule that this must exist, it is just
	// included as a convenient way to provide uniqueness.
	tx.LockTime = nextBlockHeight

	var w bytes.Buffer
	err := tx.Serialize(&w)
	if err == nil {
		minrLog.Debugf("Created coinbase tx: %v", hex.EncodeToString(w.Bytes()))
	}

	return provautil.NewTx(tx), nil
}

// spendTransaction updates the passed view by marking the inputs to the passed
// transaction as spent.  It also adds all outputs in the passed transaction
// which are not provably unspendable as available unspent transaction outputs.
func spendTransaction(utxoView *blockchain.UtxoViewpoint, tx *provautil.Tx, height uint32) error {
	for _, txIn := range tx.MsgTx().TxIn {
		originHash := &txIn.PreviousOutPoint.Hash
		originIndex := txIn.PreviousOutPoint.Index
		entry := utxoView.LookupEntry(originHash)
		if entry != nil {
			entry.SpendOutput(originIndex)
		}
	}

	utxoView.AddTxOuts(tx, height)
	return nil
}

// logSkippedDeps logs any dependencies which are also skipped as a result of
// skipping a transaction while generating a block template at the trace level.
func logSkippedDeps(tx *provautil.Tx, deps map[chainhash.Hash]*txPrioItem) {
	if deps == nil {
		return
	}

	for _, item := range deps {
		minrLog.Tracef("Skipping tx %s since it depends on %s\n",
			item.tx.Hash(), tx.Hash())
	}
}

// minimumMedianTime returns the minimum allowed timestamp for a block building
// on the end of the current best chain.  In particular, it is one second after
// the median timestamp of the last several blocks per the chain consensus
// rules.
func minimumMedianTime(chainState *blockchain.BestState) time.Time {
	return chainState.MedianTime.Add(time.Second)
}

// medianAdjustedTime returns the current time adjusted to ensure it is at least
// one second after the median timestamp of the last several blocks per the
// chain consensus rules.
func medianAdjustedTime(chainState *blockchain.BestState, timeSource blockchain.MedianTimeSource) time.Time {
	// The timestamp for the block must not be before the median timestamp
	// of the last several blocks.  Thus, choose the maximum between the
	// current time and one second after the past median time.  The current
	// timestamp is truncated to a second boundary before comparison since a
	// block timestamp does not supported a precision greater than one
	// second.
	newTimestamp := timeSource.AdjustedTime()
	minTimestamp := minimumMedianTime(chainState)
	if newTimestamp.Before(minTimestamp) {
		newTimestamp = minTimestamp
	}

	return newTimestamp
}

// BlkTmplGenerator provides a type that can be used to generate block templates
// based on a given mining policy and source of transactions to choose from.
// It also houses additional state required in order to ensure the templates
// are built on top of the current best chain and adhere to the consensus rules.
//
// See the NewBlockTemplate method for a detailed description of how the block
// template is generated.
type BlkTmplGenerator struct {
	policy       *mining.Policy
	txSource     mining.TxSource
	sigCache     *txscript.SigCache
	hashCache    *txscript.HashCache
	blockManager *blockManager
	timeSource   blockchain.MedianTimeSource
}

// newBlkTmplGenerator returns a new block template generator for the given
// policy using transactions from the provided transaction source.
//
// The additional state-related fields are required in order to ensure the
// templates are built on top of the current best chain and adhere to the
// consensus rules.
func newBlkTmplGenerator(policy *mining.Policy, txSource mining.TxSource,
	timeSource blockchain.MedianTimeSource, sigCache *txscript.SigCache,
	hashCache *txscript.HashCache, blockManager *blockManager) *BlkTmplGenerator {

	return &BlkTmplGenerator{
		policy:       policy,
		txSource:     txSource,
		sigCache:     sigCache,
		hashCache:    hashCache,
		blockManager: blockManager,
		timeSource:   timeSource,
	}
}

// NewBlockTemplate returns a new block template that is ready to be solved
// using the transactions from the passed transaction source pool and a coinbase
// that either pays to the passed address if it is not nil, or a coinbase that
// is redeemable by anyone if the passed address is nil.  The nil address
// functionality is useful since there are cases such as the getblocktemplate
// RPC where external mining software is responsible for creating their own
// coinbase which will replace the one generated for the block template.  Thus
// the need to have configured address can be avoided.
//
// The transactions selected and included are prioritized according to several
// factors.  First, each transaction has a priority calculated based on its
// value, age of inputs, and size.  Transactions which consist of larger
// amounts, older inputs, and small sizes have the highest priority.  Second, a
// fee per kilobyte is calculated for each transaction.  Transactions with a
// higher fee per kilobyte are preferred.  Finally, the block generation related
// policy settings are all taken into account.
//
// Transactions which only spend outputs from other transactions already in the
// block chain are immediately added to a priority queue which either
// prioritizes based on the priority (then fee per kilobyte) or the fee per
// kilobyte (then priority) depending on whether or not the BlockPrioritySize
// policy setting allots space for high-priority transactions.  Transactions
// which spend outputs from other transactions in the source pool are added to a
// dependency map so they can be added to the priority queue once the
// transactions they depend on have been included.
//
// Once the high-priority area (if configured) has been filled with
// transactions, or the priority falls below what is considered high-priority,
// the priority queue is updated to prioritize by fees per kilobyte (then
// priority).
//
// When the fees per kilobyte drop below the TxMinFreeFee policy setting, the
// transaction will be skipped unless the BlockMinSize policy setting is
// nonzero, in which case the block will be filled with the low-fee/free
// transactions until the block size reaches that minimum size.
//
// Any transactions which would cause the block to exceed the BlockMaxSize
// policy setting, exceed the maximum allowed signature operations per block, or
// otherwise cause the block to be invalid are skipped.
//
// Given the above, a block generated by this function is of the following form:
//
//   -----------------------------------  --  --
//  |      Coinbase Transaction         |   |   |
//  |-----------------------------------|   |   |
//  |                                   |   |   | ----- policy.BlockPrioritySize
//  |   High-priority Transactions      |   |   |
//  |                                   |   |   |
//  |-----------------------------------|   | --
//  |                                   |   |
//  |                                   |   |
//  |                                   |   |--- policy.BlockMaxSize
//  |  Transactions prioritized by fee  |   |
//  |  until <= policy.TxMinFreeFee     |   |
//  |                                   |   |
//  |                                   |   |
//  |                                   |   |
//  |-----------------------------------|   |
//  |  Low-fee/Non high-priority (free) |   |
//  |  transactions (while block size   |   |
//  |  <= policy.BlockMinSize)          |   |
//   -----------------------------------  --
func (g *BlkTmplGenerator) NewBlockTemplate(payToAddress provautil.Address, validateKey *btcec.PrivateKey) (*BlockTemplate, error) {
	// Locals for faster access.
	policy := g.policy
	blockManager := g.blockManager
	timeSource := g.timeSource
	sigCache := g.sigCache
	hashCache := g.hashCache

	// Extend the most recently known best block.
	best := blockManager.chain.BestSnapshot()
	prevHash := best.Hash
	nextBlockHeight := best.Height + 1

	// Create a standard coinbase transaction paying to the provided
	// address.  NOTE: The coinbase value will be updated to include the
	// fees from the selected transactions later after they have actually
	// been selected.  It is created here to detect any errors early
	// before potentially doing a lot of work below.  The extra nonce helps
	// ensure the transaction is not a duplicate transaction (paying the
	// same value to the same public key address would otherwise be an
	// identical transaction for block version 1).
	coinbaseScript, err := standardCoinbaseScript()
	if err != nil {
		return nil, err
	}
	coinbaseTx, err := createCoinbaseTx(coinbaseScript, nextBlockHeight,
		payToAddress)
	if err != nil {
		return nil, err
	}
	numCoinbaseSigOps := int64(blockchain.CountSigOps(coinbaseTx))

	// Get the current source transactions and create a priority queue to
	// hold the transactions which are ready for inclusion into a block
	// along with some priority related and fee metadata.  Reserve the same
	// number of items that are available for the priority queue.  Also,
	// choose the initial sort order for the priority queue based on whether
	// or not there is an area allocated for high-priority transactions.
	sourceTxns := g.txSource.MiningDescs()
	sortedByFee := policy.BlockPrioritySize == 0
	priorityQueue := newTxPriorityQueue(len(sourceTxns), sortedByFee)

	// Create a slice to hold the transactions to be included in the
	// generated block with reserved space.  Also create a utxo view to
	// house all of the input transactions so multiple lookups can be
	// avoided.
	blockTxns := make([]*provautil.Tx, 0, len(sourceTxns))
	blockTxns = append(blockTxns, coinbaseTx)
	blockUtxos := blockchain.NewUtxoViewpoint()
	keyView := blockchain.NewKeyViewpoint()
	keyView.SetLastKeyID(blockManager.chain.LastKeyID())
	keyView.SetKeys(blockManager.chain.AdminKeySets())
	keyView.SetKeyIDs(blockManager.chain.KeyIDs())

	// dependers is used to track transactions which depend on another
	// transaction in the source pool.  This, in conjunction with the
	// dependsOn map kept with each dependent transaction helps quickly
	// determine which dependent transactions are now eligible for inclusion
	// in the block once each transaction has been included.
	dependers := make(map[chainhash.Hash]map[chainhash.Hash]*txPrioItem)

	// Create slices to hold the fees and number of signature operations
	// for each of the selected transactions and add an entry for the
	// coinbase.  This allows the code below to simply append details about
	// a transaction as it is selected for inclusion in the final block.
	// However, since the total fees aren't known yet, use a dummy value for
	// the coinbase fee which will be updated later.
	txFees := make([]int64, 0, len(sourceTxns))
	txSigOpCounts := make([]int64, 0, len(sourceTxns))
	txFees = append(txFees, -1) // Updated once known
	txSigOpCounts = append(txSigOpCounts, numCoinbaseSigOps)

	minrLog.Debugf("Considering %d transactions for inclusion to new block",
		len(sourceTxns))

mempoolLoop:
	for _, txDesc := range sourceTxns {
		// A block can't have more than one coinbase or contain
		// non-finalized transactions.
		tx := txDesc.Tx
		if blockchain.IsCoinBase(tx) {
			minrLog.Tracef("Skipping coinbase tx %s", tx.Hash())
			continue
		}
		if !blockchain.IsFinalizedTransaction(tx, nextBlockHeight,
			timeSource.AdjustedTime()) {
			minrLog.Tracef("Skipping non-finalized tx %s", tx.Hash())
			continue
		}

		// Fetch all of the utxos referenced by the this transaction.
		// NOTE: This intentionally does not fetch inputs from the
		// mempool since a transaction which depends on other
		// transactions in the mempool must come after those
		// dependencies in the final generated block.
		utxos, err := blockManager.chain.FetchUtxoView(tx)
		if err != nil {
			minrLog.Warnf("Unable to fetch utxo view for tx %s: "+
				"%v", tx.Hash(), err)
			continue
		}

		// Setup dependencies for any transactions which reference
		// other transactions in the mempool so they can be properly
		// ordered below.
		prioItem := &txPrioItem{tx: tx}
		for _, txIn := range tx.MsgTx().TxIn {
			originHash := &txIn.PreviousOutPoint.Hash
			originIndex := txIn.PreviousOutPoint.Index
			utxoEntry := utxos.LookupEntry(originHash)
			if utxoEntry == nil || utxoEntry.IsOutputSpent(originIndex) {
				if !g.txSource.HaveTransaction(originHash) {
					minrLog.Tracef("Skipping tx %s because "+
						"it references unspent output "+
						"%s which is not available",
						tx.Hash(), txIn.PreviousOutPoint)
					continue mempoolLoop
				}

				// The transaction is referencing another
				// transaction in the source pool, so setup an
				// ordering dependency.
				deps, exists := dependers[*originHash]
				if !exists {
					deps = make(map[chainhash.Hash]*txPrioItem)
					dependers[*originHash] = deps
				}
				deps[*prioItem.tx.Hash()] = prioItem
				if prioItem.dependsOn == nil {
					prioItem.dependsOn = make(
						map[chainhash.Hash]struct{})
				}
				prioItem.dependsOn[*originHash] = struct{}{}

				// Skip the check below. We already know the
				// referenced transaction is available.
				continue
			}
		}

		// Calculate the final transaction priority using the input
		// value age sum as well as the adjusted transaction size.  The
		// formula is: sum(inputValue * inputAge) / adjustedTxSize
		prioItem.priority = mining.CalcPriority(tx.MsgTx(), utxos,
			nextBlockHeight)

		// Calculate the fee in Atoms/kB.
		txSize := tx.MsgTx().SerializeSize()
		prioItem.feePerKB = (txDesc.Fee * 1000) / int64(txSize)
		prioItem.fee = txDesc.Fee
		prioItem.isAdmin = isAdmin(tx.MsgTx())

		// Add the transaction to the priority queue to mark it ready
		// for inclusion in the block unless it has dependencies.
		if prioItem.dependsOn == nil {
			heap.Push(priorityQueue, prioItem)
		}

		// Merge the referenced outputs from the input transactions to
		// this transaction into the block utxo view.  This allows the
		// code below to avoid a second lookup.
		mergeUtxoView(blockUtxos, utxos)
	}

	minrLog.Tracef("Priority queue len %d, dependers len %d",
		priorityQueue.Len(), len(dependers))

	// The starting block size is the size of the block header plus the max
	// possible transaction count size, plus the size of the coinbase
	// transaction.
	blockSize := blockHeaderOverhead + uint32(coinbaseTx.MsgTx().SerializeSize())
	blockSigOps := numCoinbaseSigOps
	totalFees := int64(0)

	// Choose which transactions make it into the block.
	for priorityQueue.Len() > 0 {
		// Grab the highest priority (or highest fee per kilobyte
		// depending on the sort order) transaction.
		prioItem := heap.Pop(priorityQueue).(*txPrioItem)
		tx := prioItem.tx

		// Grab the list of transactions which depend on this one (if any).
		deps := dependers[*tx.Hash()]

		// Enforce maximum block size.  Also check for overflow.
		txSize := uint32(tx.MsgTx().SerializeSize())
		blockPlusTxSize := blockSize + txSize
		if blockPlusTxSize < blockSize || blockPlusTxSize >= policy.BlockMaxSize {
			minrLog.Tracef("Skipping tx %s because it would exceed "+
				"the max block size", tx.Hash())
			logSkippedDeps(tx, deps)
			continue
		}

		// Enforce maximum signature operations per block.  Also check
		// for overflow.
		numSigOps := int64(blockchain.CountSigOps(tx))
		if blockSigOps+numSigOps < blockSigOps ||
			blockSigOps+numSigOps > blockchain.MaxSigOpsPerBlock {
			minrLog.Tracef("Skipping tx %s because it would "+
				"exceed the maximum sigops per block", tx.Hash())
			logSkippedDeps(tx, deps)
			continue
		}
		numP2SHSigOps, err := blockchain.CountP2SHSigOps(tx, false,
			blockUtxos)
		if err != nil {
			minrLog.Tracef("Skipping tx %s due to error in "+
				"CountP2SHSigOps: %v", tx.Hash(), err)
			logSkippedDeps(tx, deps)
			continue
		}
		numSigOps += int64(numP2SHSigOps)
		if blockSigOps+numSigOps < blockSigOps ||
			blockSigOps+numSigOps > blockchain.MaxSigOpsPerBlock {
			minrLog.Tracef("Skipping tx %s because it would "+
				"exceed the maximum sigops per block (p2sh)",
				tx.Hash())
			logSkippedDeps(tx, deps)
			continue
		}

		// Skip free transactions once the block is larger than the
		// minimum block size.
		if sortedByFee &&
			prioItem.feePerKB < int64(policy.TxMinFreeFee) &&
			blockPlusTxSize >= policy.BlockMinSize {

			minrLog.Tracef("Skipping tx %s with feePerKB %d "+
				"< TxMinFreeFee %d and block size %d >= "+
				"minBlockSize %d", tx.Hash(), prioItem.feePerKB,
				policy.TxMinFreeFee, blockPlusTxSize,
				policy.BlockMinSize)
			logSkippedDeps(tx, deps)
			continue
		}

		// Prioritize by fee per kilobyte once the block is larger than
		// the priority size or there are no more high-priority
		// transactions.
		if !sortedByFee && (blockPlusTxSize >= policy.BlockPrioritySize ||
			prioItem.priority <= mempool.MinHighPriority) {

			minrLog.Tracef("Switching to sort by fees per "+
				"kilobyte blockSize %d >= BlockPrioritySize "+
				"%d || priority %.2f <= minHighPriority %.2f",
				blockPlusTxSize, policy.BlockPrioritySize,
				prioItem.priority, mempool.MinHighPriority)

			sortedByFee = true
			priorityQueue.SetLessFunc(txPQByFee)

			// Put the transaction back into the priority queue and
			// skip it so it is re-priortized by fees if it won't
			// fit into the high-priority section or the priority is
			// too low.  Otherwise this transaction will be the
			// final one in the high-priority section, so just fall
			// though to the code below so it is added now.
			if blockPlusTxSize > policy.BlockPrioritySize ||
				prioItem.priority < mempool.MinHighPriority {

				heap.Push(priorityQueue, prioItem)
				continue
			}
		}

		// Ensure the transaction inputs pass all of the necessary
		// preconditions before allowing it to be added to the block.
		_, err = blockchain.CheckTransactionInputs(tx, nextBlockHeight,
			blockUtxos, activeNetParams.Params)
		if err != nil {
			minrLog.Tracef("Skipping tx %s due to error in "+
				"CheckTransactionInputs: %v", tx.Hash(), err)
			logSkippedDeps(tx, deps)
			continue
		}

		// CheckTransactionOutputs checks outputs for state violations.
		err = blockchain.CheckTransactionOutputs(tx, keyView)
		if err != nil {
			minrLog.Tracef("Skipping tx %s due to error in "+
				"CheckTransactionOutputs: %v", tx.Hash(), err)
			logSkippedDeps(tx, deps)
			continue
		}

		err = blockchain.ValidateTransactionScripts(tx, blockUtxos, keyView,
			txscript.StandardVerifyFlags, sigCache, hashCache)
		if err != nil {
			minrLog.Tracef("Skipping tx %s due to error in "+
				"ValidateTransactionScripts: %v", tx.Hash(), err)
			logSkippedDeps(tx, deps)
			continue
		}

		// Spend the transaction inputs in the block utxo view and add
		// an entry for it to ensure any transactions which reference
		// this one have it available as an input and can ensure they
		// aren't double spending.
		spendTransaction(blockUtxos, tx, nextBlockHeight)

		// Add the transaction to the block, increment counters, and
		// save the fees and signature operation counts to the block
		// template.
		blockTxns = append(blockTxns, tx)
		blockSize += txSize
		blockSigOps += numSigOps
		totalFees += prioItem.fee
		txFees = append(txFees, prioItem.fee)
		txSigOpCounts = append(txSigOpCounts, numSigOps)

		minrLog.Tracef("Adding tx %s (priority %.2f, feePerKB %d)",
			prioItem.tx.Hash(), prioItem.priority, prioItem.feePerKB)

		// Add transactions which depend on this one (and also do not
		// have any other unsatisified dependencies) to the priority
		// queue.
		for _, item := range deps {
			// Add the transaction to the priority queue if there
			// are no more dependencies after this one.
			delete(item.dependsOn, *tx.Hash())
			if len(item.dependsOn) == 0 {
				heap.Push(priorityQueue, item)
			}
		}
	}

	// Now that the actual transactions have been selected, update the
	// block size for the real transaction count and coinbase value with
	// the total fees accordingly.
	blockSize -= wire.MaxVarIntPayload -
		uint32(wire.VarIntSerializeSize(uint64(len(blockTxns))))
	coinbaseTx.MsgTx().TxOut[0].Value += totalFees
	txFees[0] = -totalFees

	// Coinbase transactions that pay out zero value can avoid making new
	// UTXOs by spending to a nullDataTy.  The header block size must be
	// updated accordingly.
	if coinbaseTx.MsgTx().TxOut[0].Value == 0 {
		cbScriptByteLen := len(coinbaseTx.MsgTx().TxOut[0].PkScript)
		nullScript, err := txscript.NewScriptBuilder().
			AddOp(txscript.OP_RETURN).Script()
		if err != nil {
			return nil, err
		}
		blockSize -= uint32(cbScriptByteLen - len(nullScript))
		coinbaseTx.MsgTx().TxOut[0].PkScript = nullScript
	}

	// Calculate the required difficulty for the block.  The timestamp
	// is potentially adjusted to ensure it comes after the median time of
	// the last several blocks per the chain consensus rules.
	ts := medianAdjustedTime(best, timeSource)
	reqDifficulty, err := blockManager.chain.CalcNextRequiredDifficulty()
	if err != nil {
		return nil, err
	}

	// Create a new block ready to be solved.
	merkles := blockchain.BuildMerkleTreeStore(blockTxns)
	var msgBlock wire.MsgBlock
	msgBlock.Header = wire.BlockHeader{
		Version:    generatedBlockVersion,
		PrevBlock:  *prevHash,
		MerkleRoot: *merkles[len(merkles)-1],
		Timestamp:  ts,
		Bits:       reqDifficulty,
		Height:     uint32(nextBlockHeight),
		Size:       blockSize,
	}

	// Sign the block
	msgBlock.Header.Sign(validateKey)

	for _, tx := range blockTxns {
		if err := msgBlock.AddTransaction(tx.MsgTx()); err != nil {
			return nil, err
		}
	}

	// Finally, perform a full check on the created block against the chain
	// consensus rules to ensure it properly connects to the current best
	// chain with no issues.
	block := provautil.NewBlock(&msgBlock)
	if err := blockManager.chain.CheckConnectBlock(block); err != nil {
		return nil, err
	}

	minrLog.Debugf("Created new block template (%d transactions, %d in "+
		"fees, %d signature operations, %d bytes, target difficulty "+
		"%064x)", len(msgBlock.Transactions), totalFees, blockSigOps,
		blockSize, blockchain.CompactToBig(msgBlock.Header.Bits))

	return &BlockTemplate{
		Block:           &msgBlock,
		Fees:            txFees,
		SigOpCounts:     txSigOpCounts,
		Height:          nextBlockHeight,
		ValidPayAddress: payToAddress != nil,
	}, nil
}

// UpdateBlockTime updates the timestamp in the header of the passed block to
// the current time while taking into account the median time of the last
// several blocks to ensure the new time is after that time per the chain
// consensus rules.  Finally, it will update the target difficulty if needed
// based on the new time for the test networks since their target difficulty can
// change based upon time.
func (g *BlkTmplGenerator) UpdateBlockTime(msgBlock *wire.MsgBlock,
	validateKey *btcec.PrivateKey) error {

	// The new timestamp is potentially adjusted to ensure it comes after
	// the median time of the last several blocks per the chain consensus
	// rules.
	best := g.blockManager.chain.BestSnapshot()
	newTimestamp := medianAdjustedTime(best, g.timeSource)
	msgBlock.Header.Timestamp = newTimestamp

	// Re-sign the block, since we updated the block time
	msgBlock.Header.Sign(validateKey)

	return nil
}
