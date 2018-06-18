package types

import (
	"math/big"
	"sync/atomic"
	"tinychain/common"
	json "github.com/json-iterator/go"
	"github.com/libp2p/go-libp2p-crypto"
	"errors"
	"tinychain/bmt"
	"tinychain/db/leveldb"
	"strconv"
)

const (
	MaxTxSize = 32 * 1024 // Maximum transaction size
)

var (
	ErrSignNotFound    = errors.New("signature not found")
	ErrPubkeyNotFound  = errors.New("public key not found")
	ErrAddressNotMatch = errors.New("address not match")
)

type Transaction struct {
	txData

	txHash atomic.Value // hash cache
	size   atomic.Value // size cache

	PubKey    []byte `json:"pub_key"`   // Public key
	Signature []byte `json:"signature"` // Signature of tx
}

type txData struct {
	Nonce    uint64         `json:"nonce"`     // Account nonce, which is used to avoid double spending
	GasLimit uint64         `json:"gas_limit"` // Gas limit of a tx
	Value    *big.Int       `json:"value"`     // Transferring value
	From     common.Address `json:"from"`
	To       common.Address `json:"to"` // Recipient of this tx, nil means contract creation
	Payload  []byte         `json:"payload"`
}

func NewTransaction(nonce, gasLimit uint64, value *big.Int, payload []byte, from, to common.Address) *Transaction {
	return &Transaction{txData: NewTxData(nonce, gasLimit, value, payload, from, to)}
}

func NewTxData(nonce, gasLimit uint64, value *big.Int, payload []byte, from, to common.Address) txData {
	return txData{
		Nonce:    nonce,
		GasLimit: gasLimit,
		Value:    value,
		Payload:  payload,
		From:     from,
		To:       to,
	}
}

func (txd txData) Serialize() ([]byte, error) { return json.Marshal(txd) }
func (txd txData) Deserialize(d []byte) error { return json.Unmarshal(d, txd) }

func (tx *Transaction) Serialize() ([]byte, error) { return json.Marshal(tx) }
func (tx *Transaction) Deserialize(d []byte) error { return json.Unmarshal(d, tx) }

func (tx *Transaction) Hash() common.Hash {
	if hash := tx.txHash.Load(); hash != nil {
		return hash.(common.Hash)
	}
	txdata := NewTxData(tx.Nonce, tx.GasLimit, tx.Value, tx.Payload, tx.From, tx.To)
	data, _ := txdata.Serialize()
	h := common.Sha256(data)
	tx.txHash.Store(h)
	return h
}

// Sign the transaction with private key
func (tx *Transaction) Sign(privKey crypto.PrivKey) ([]byte, error) {
	if sign := tx.Signature; sign != nil {
		return sign, nil
	}
	hash := tx.Hash()
	s, err := privKey.Sign(hash[:])
	if err != nil {
		return nil, err
	}
	tx.Signature = s
	tx.PubKey, err = privKey.GetPublic().Bytes()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Verify transaction signature by specific public key
func (tx *Transaction) Verify() (bool, error) {
	if tx.Signature == nil {
		return false, ErrSignNotFound
	}
	if tx.PubKey == nil {
		return false, ErrPubkeyNotFound
	}
	pubKey, err := crypto.UnmarshalPublicKey(tx.PubKey)
	if err != nil {
		return false, err
	}
	// Verify address
	address, err := common.GenAddrByPubkey(pubKey)
	if err != nil {
		return false, err
	}
	if address != tx.From {
		return false, ErrAddressNotMatch
	}

	// Verify tx hash
	hash := tx.Hash()
	equal, err := pubKey.Verify(hash[:], tx.Signature)
	if err != nil {
		return false, err
	}
	return equal, nil
}

func (tx *Transaction) Cost() *big.Int {
	return new(big.Int).Add(tx.Value, new(big.Int).SetUint64(tx.GasLimit))
}

func (tx *Transaction) Size() uint32 {
	if size := tx.size.Load(); size != nil {
		return size.(uint32)
	}
	data, _ := tx.Serialize()
	size := uint32(len(data))
	tx.size.Store(size)
	return size
}

type Transactions []*Transaction

func (txs Transactions) Hash() common.Hash {
	txSet := bmt.WriteSet{}
	for i, tx := range txs {
		txSet[strconv.Itoa(i)] = tx.Hash().Bytes()
	}
	root, _ := bmt.Hash(txSet)
	return root
}

func (txs Transactions) Commit(db *leveldb.LDBDatabase) error {
	txSet := bmt.WriteSet{}
	for _, tx := range txs {
		txSet[tx.Hash().String()] = tx.Hash().Bytes()
	}
	return bmt.Commit(txSet, db)
}

type NonceSortedList Transactions

func (txs NonceSortedList) Len() int {
	return len(txs)
}

func (txs NonceSortedList) Less(i, j int) bool {
	return txs[i].Nonce < txs[j].Nonce
}

func (txs NonceSortedList) Swap(i, j int) {
	txs[i], txs[j] = txs[j], txs[i]
}

// TxMeta represents the meta data of a transaction,
// contains the index of transacitons in a certain block
type TxMeta struct {
	Hash    common.Hash `json:"block_hash"`
	Height  *big.Int    `json:"height"`
	TxIndex uint64      `json:"tx_index"`
}

func (tm *TxMeta) Serialize() ([]byte, error) {
	return json.Marshal(tm)
}

func (tm *TxMeta) Deserialize(d []byte) error {
	return json.Unmarshal(d, tm)
}
