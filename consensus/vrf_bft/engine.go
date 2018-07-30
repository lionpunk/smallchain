package vrf_bft

import (
	"tinychain/core/types"
	"tinychain/core/state"
	"tinychain/event"
	"tinychain/core"
	"tinychain/common"
	"time"
	"math/big"
	"sync/atomic"
	"tinychain/p2p"
	"tinychain/consensus/blockpool"
	"github.com/libp2p/go-libp2p-crypto"
	"github.com/libp2p/go-libp2p-peer"
	"sync"
	"tinychain/consensus"
	"tinychain/executor"
	"tinychain/core/txpool"
)

var (
	log = common.GetLogger("consensus")
)

const (
	// Period type of the BFT algorithm
	PROPOSE    = iota
	PRE_COMMIT
	COMMIT

	BLOCK_PROPOSE_GAP = 5 * time.Second
)

type TxPool interface {
	Start()
	Stop()
	Pending() types.Transactions
	Drop(transactions types.Transactions)
}

type Blockchain interface {
	LastBlock() *types.Block      // Last block in memory
	LastFinalBlock() *types.Block // Last irreversible block
}

// Engine is the main wrapper of dpos_bft algorithm
// The dpos_bft algorithm process procedures described below:
// 1. select the 21 block producer at a round, and shuffle the order of them.
// 2. every selected block producers propose a new block in turn, and multicast to other BPs.
// 3. Wait 0.5s for network io, and this BP kick off the bft process.
// 4. After bft performs successfully, all BPs commit the blocks.
type Engine struct {
	config *Config

	state            *state.StateDB           // current state
	seqNo            atomic.Value             // next block height at bft processing
	chain            Blockchain               // current blockchain
	bpPool           *bpPool                  // manage and operate block producers' info
	blockPool        consensus.BlockPool      // pool to retrieves new proposed blocks
	txPool           TxPool                   // TxPool used to filter out valid transactions
	receipts         sync.Map                 // save receipts of block map[uint64]receipts
	validator        consensus.BlockValidator // Block validator to validate state
	execCompleteChan chan struct{}            // channel for informing the process is completed

	bftState       atomic.Value // the state of current bft period
	preCommitVotes int          // pre-commit votes statistics
	commitVotes    int          // commit votes statistics

	event       *event.TypeMux
	processLock chan struct{}
	quitCh      chan struct{}

	newBlockSub       event.Subscription // Subscribe new block from block pool
	consensusSub      event.Subscription // Subscribe kick_off bft event
	receiptsSub       event.Subscription // Subscribe receipts event from executor
	commitCompleteSub event.Subscription // Subscribe commit complete event from executor
}

func New(config *common.Config, state *state.StateDB, chain Blockchain, id peer.ID, validator consensus.BlockValidator) (*Engine, error) {
	conf := newConfig(config)
	engine := &Engine{
		config:           conf,
		chain:            chain,
		state:            state,
		event:            event.GetEventhub(),
		validator:        validator,
		blockPool:        blockpool.NewBlockPool(config, validator, log, common.PROPOSE_BLOCK_MSG),
		execCompleteChan: make(chan struct{}),
		quitCh:           make(chan struct{}),
	}
	// init block producer info
	self := &blockProducer{
		id: id,
	}
	if conf.BP {
		privKey, err := crypto.UnmarshalPrivateKey(conf.PrivKey)
		if err != nil {
			return nil, err
		}
		self.privKey = privKey
		engine.txPool = txpool.NewTxPool(config, executor.NewTxValidator(executor.NewConfig(config), state), state, false, false)
	} else {
		engine.txPool = txpool.NewTxPool(config, executor.NewTxValidator(executor.NewConfig(config), state), state, true, true)
	}
	engine.bpPool = newBpPool(conf, log, self, chain)

	return engine, nil
}

func (eg *Engine) Start() error {
	eg.newBlockSub = eg.event.Subscribe(&core.BlockReadyEvent{})
	eg.consensusSub = eg.event.Subscribe(&core.ConsensusEvent{})
	eg.receiptsSub = eg.event.Subscribe(&core.NewReceiptsEvent{})
	eg.commitCompleteSub = eg.event.Subscribe(&core.CommitCompleteEvent{})

	// Get last height of final block
	eg.seqNo.Store(eg.chain.LastFinalBlock().Height())

	go eg.listen()
	if eg.config.BP {
		eg.init()
		go eg.proposeLoop()
		go eg.startBFT()
	}
	go eg.txPool.Start()
	return nil
}

func (eg *Engine) init() {
	eg.seqNo.Store(eg.chain.LastFinalBlock().Height() + 1)
	eg.bftState.Store(PROPOSE)
	eg.preCommitVotes = 0
	eg.commitVotes = 0
}

func (eg *Engine) nextBFTRound() {
	seqNo := eg.SeqNo()
	eg.blockPool.DelBlock(seqNo)
	eg.receipts.Delete(seqNo)
	eg.txPool.Drop(eg.chain.LastBlock().Transactions)

	eg.init()
}

func (eg *Engine) Stop() error {
	eg.txPool.Stop()
	eg.quitCh <- struct{}{}
	return nil
}

func (eg *Engine) listen() {
	for {
		select {
		case ev := <-eg.newBlockSub.Chan():
			block := ev.(*core.BlockReadyEvent).Block
			if eg.config.BP {
				go eg.multicastBlock(block)
			} else {
				go eg.broadcast(block)
			}
		case ev := <-eg.consensusSub.Chan():
			block := ev.(*core.ConsensusEvent).Block
			eg.blockPool.AddBlock(block)
			eg.multicastBlock(block)
		case ev := <-eg.receiptsSub.Chan():
			rev := ev.(*core.NewReceiptsEvent)
			eg.setReceipts(rev.Block.Height(), rev.Receipts)
			eg.execCompleteChan <- struct{}{}
		case <-eg.quitCh:
			eg.consensusSub.Unsubscribe()
			eg.receiptsSub.Unsubscribe()
			return
		}
	}
}

// proposeLoop set a loop to try to propose a block every BLOCK_PROPOSE_GAP
func (eg *Engine) proposeLoop() {
	ticker := time.NewTicker(BLOCK_PROPOSE_GAP)
	for {
		select {
		case <-ticker.C:
			self := eg.bpPool.reachSelfTurn()
			if self == nil {
				continue
			}
			// TODO try to check the current state of bpPool and decide to propose block or not
			eg.proposeBlock(eg.txPool.Pending())
		}
	}
}

// SeqNo returns the block height at current BFT process
func (eg *Engine) SeqNo() uint64 {
	if height := eg.seqNo.Load(); height != nil {
		return height.(uint64)
	}
	return 0
}

func (eg *Engine) Self() *blockProducer {
	return eg.bpPool.self
}

func (eg *Engine) Address() common.Address {
	return eg.bpPool.self.Addr()
}

func (eg *Engine) getBFTState() int {
	if s := eg.bftState.Load(); s != nil {
		return s.(int)
	}
	return 0
}

func (eg *Engine) setBFTState(priod int) {
	eg.bftState.Store(priod)
}

func (eg *Engine) getReceipts(height uint64) types.Receipts {
	if receipts, exist := eg.receipts.Load(height); exist {
		return receipts.(types.Receipts)
	}
	return nil
}

func (eg *Engine) setReceipts(height uint64, receipts types.Receipts) {
	eg.receipts.Store(height, receipts)
}

// proposeBlock proposes a new block without state_root and receipts_root
func (eg *Engine) proposeBlock(txs types.Transactions) {
	header := &types.Header{
		ParentHash: eg.chain.LastBlock().Hash(),
		Height:     eg.chain.LastBlock().Height() + 1,
		Coinbase:   eg.Address(),
		Extra:      eg.config.Extra,
		Time:       new(big.Int).SetInt64(time.Now().Unix()),
		GasLimit:   eg.config.GasLimit,
	}

	block := types.NewBlock(header, txs)
	go eg.event.Post(&core.ProposeBlockEvent{block})
	log.Infof("Block producer %s propose a new block, height = #%d", eg.Address(), block.Height())
}

// process processes the block received from the solo BP.
// It is invoked by not-BP peers.
func (eg *Engine) process() {
	<-eg.processLock
	block := eg.blockPool.GetBlock(eg.chain.LastBlock().Height() + 1)
	if block != nil {
		go eg.event.Post(&core.ExecBlockEvent{block})
	}
}

// multicastBlock send the new proposed block to other BPs
func (eg *Engine) multicastBlock(block *types.Block) error {
	var pids []peer.ID
	for _, bp := range eg.bpPool.getBPs() {
		pids = append(pids, bp.id)
	}
	data, err := block.Serialize()
	if err != nil {
		return err
	}
	go eg.event.Post(&p2p.MultiSendEvent{
		Targets: pids,
		Typ:     common.PROPOSE_BLOCK_MSG,
		Data:    data,
	})
	eg.bftState.Store(PRE_COMMIT)
	return nil
}

func (eg *Engine) broadcast(block *types.Block) error {
	data, err := block.Serialize()
	if err != nil {
		return err
	}
	go eg.event.Post(&p2p.BroadcastEvent{
		Typ:  common.PROPOSE_BLOCK_MSG,
		Data: data,
	})
	return nil
}

func (eg *Engine) Finalize(header *types.Header, state *state.StateDB, txs types.Transactions, receipts types.Receipts) (*types.Block, error) {
	root, err := state.IntermediateRoot()
	if err != nil {
		return nil, err
	}
	header.StateRoot = root
	header.ReceiptsHash = receipts.Hash()

	header.TxRoot = txs.Hash()
	newBlk := types.NewBlock(header, txs)
	newBlk.PubKey, err = eg.Self().pubKey.Bytes()
	if err != nil {
		return nil, err
	}
	newBlk.Sign(eg.Self().PrivKey())
	return newBlk, nil
}

// Protocols return protocols used from p2p layer to handle messages.
func (eg *Engine) Protocols() []p2p.Protocol {
	return []p2p.Protocol{
		eg,
		eg.bpPool,
		eg.blockPool,
	}
}
