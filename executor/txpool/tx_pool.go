package txpool

import (
	"tinychain/common"
	"tinychain/core/types"
	"tinychain/event"
	"sync"
	"tinychain/core/state"
	"errors"
	"tinychain/core"
	batcher "github.com/yyh1102/go-batcher"
	"sort"
)

var (
	log = common.GetLogger("txpool")

	ErrTxDuplicate = errors.New("transaction duplicate")
	ErrPoolFull    = errors.New("tx_pool is full")
	ErrTxDiscard   = errors.New("old transaction is better, discard the new one")
)

type TxValidator interface {
	ValidateTx(transaction *types.Transaction) error
}

type TxPool struct {
	config       *Config        // Txpool config
	currentState *state.StateDB // Current state
	validator    TxValidator    // Tx validator wrapper
	all          *txLookup      // Cache all tx hash to accelerate searching
	batch        *batcher.Batch // Batch for txs launching
	event        *event.TypeMux
	quitCh       chan struct{}

	// all valid and processable txs.
	// map[common.Address]*txList
	pending sync.Map

	// all new-added and non-processable txs,including valid and invalid txs.
	// map[common.Address]*txList
	queue sync.Map

	newTxSub event.Subscription
}

func NewTxPool(config *Config, validator TxValidator, state *state.StateDB) *TxPool {
	tp := &TxPool{
		config:       config,
		validator:    validator,
		event:        event.GetEventhub(),
		all:          newTxLookup(),
		currentState: state,
		quitCh:       make(chan struct{}, 1),
	}

	batch := batcher.NewBatch(
		"NEW_TXS",
		config.BatchCapacity,
		config.BatchTimeout,
		tp.launch,
	)

	tp.batch = batch
	return tp
}

func (tp *TxPool) Start() {
	tp.newTxSub = tp.event.Subscribe(&core.NewTxEvent{})
	go tp.listen()
}

func (tp *TxPool) listen() {
	for {
		select {
		case ev := <-tp.newTxSub.Chan():
			go tp.add(ev.(*core.NewTxEvent).Tx)
		case <-tp.quitCh:
			tp.newTxSub.Unsubscribe()
			break
		}
	}
}

func (tp *TxPool) launch(batch []interface{}) {
	go tp.event.Post(&core.ExecPendingTxEvent{
		Txs: tp.Pending(),
	})
}

// Pending returns all nonce-asec-sorted and gasPrice-desec-sorted list of transactions for every address
func (tp *TxPool) Pending() types.Transactions {
	var results types.Transactions
	tp.pending.Range(func(key, value interface{}) bool {
		list := value.(*txList).All()
		for _, tx := range list {
			results = append(results, tx)
		}
		return true
	})

	sort.Sort(types.SortedList(results))
	return results
}

func (tp *TxPool) Add(tx *types.Transaction) error {
	return tp.add(tx)
}

func (tp *TxPool) getQueue(addr common.Address) *txList {
	if tl, exist := tp.queue.Load(addr); exist {
		return tl.(*txList)
	}
	return nil
}

func (tp *TxPool) getPending(addr common.Address) *txList {
	if tl, exist := tp.pending.Load(addr); exist {
		return tl.(*txList)
	}
	return nil
}

func (tp *TxPool) add(tx *types.Transaction) error {
	// Check tx duplicate
	if tp.all.Get(tx.Hash()) {
		log.Errorf("Transaction %s duplicate.", tx.Hash())
		return ErrTxDuplicate
	}

	// Validate tx
	if err := tp.validate(tx); err != nil {
		log.Errorf("Validate tx failed, %s", err)
		return err
	}

	// check txpool queue is full or not
	if tp.all.Len() >= tp.config.MaxTxSize {
		log.Warning(ErrPoolFull.Error())
		return ErrPoolFull
	}

	// Check whether to replace a pending tx
	replace, old := tp.replacePending(tx)
	if replace {
		log.Errorf("replace an old pending tx %s", old.Hash())
		return nil
	}

	// Add queue
	err := tp.addQueue(tx)
	if err != nil {
		return err
	}

	// Check processable
	tp.activate([]common.Address{tx.From})
	return nil
}

func (tp *TxPool) addQueue(tx *types.Transaction) error {
	tl := tp.getQueue(tx.From)
	if tl == nil {
		tl := newTxList()
		tl.add(tx, tp.config.PriceBump)
		tp.queue.Store(tx.From, tl)
		return nil
	}
	inserted, _ := tl.add(tx, tp.config.PriceBump)
	if !inserted {
		return ErrTxDiscard
	}

	// Check tx is existed in pool or not
	if !tp.all.Get(tx.Hash()) {
		tp.all.Add(tx.Hash())
	}

	return nil
}

// replacePending check whether to replace tx in pending list,
// and if yes, return true
func (tp *TxPool) replacePending(tx *types.Transaction) (bool, *types.Transaction) {
	tl := tp.getPending(tx.From)
	if tl == nil {
		return false, nil
	}
	canReplace, old := tl.CanInsert(tx, tp.config.PriceBump)
	if canReplace && old != nil {
		tl.Put(tx)
	}

	return canReplace && old != nil, old
}

// activate moves transaction that have become processable from
// the queue to the pending list. During this process, all
// invalid transactions (low nonce, low balance) are deleted.
func (tp *TxPool) activate(addrs []common.Address) {
	var activeTxs types.Transactions
	for _, addr := range addrs {
		state := tp.currentState.GetStateObj(addr)

		// Remove transaction that have processed at prev state
		tl := tp.getPending(addr)
		tl.filter(func(tx *types.Transaction) bool {
			return tx.Nonce < state.Nonce()
		})

		// Activate transaction in queue
		tl = tp.getQueue(addr)
		if tl == nil {
			continue
		}
		// 1. drop all low-nonce transaction
		for _, tx := range tl.Forget(state.Nonce()) {
			tp.all.Del(tx.Hash())
		}

		// 2. drop all costly transaction
		for _, tx := range tl.Release(state.Balance()) {
			tp.all.Del(tx.Hash())
		}

		// 3. Get sequentially increasing list and activate them
		for _, tx := range tl.Ready(state.Nonce()) {
			if err := tp.addPending(tx); err != nil {
				continue
			}
			activeTxs = append(activeTxs, tx)
		}
	}
	if len(activeTxs) > 0 {
		tp.postBatch(activeTxs)
	}
}

// addPending moves processable txs in queue to pending.
func (tp *TxPool) addPending(tx *types.Transaction) error {
	tl := tp.getPending(tx.From)
	if tl == nil {
		tl = newTxList()
		tp.pending.Store(tx.From, tl)
	}

	inserted, old := tl.add(tx, tp.config.PriceBump)
	if !inserted {
		tp.all.Del(tx.Hash())
		return ErrTxDiscard
	}

	if old != nil {
		tp.all.Del(old.Hash())
	}
	return nil
}

func (tp *TxPool) validate(tx *types.Transaction) error {
	return tp.validator.ValidateTx(tx)
}

func (tp *TxPool) postBatch(txs types.Transactions) {
	var batch []interface{}
	for _, tx := range txs {
		batch = append(batch, tx)
	}
	tp.batch.Batch(batch)
}
