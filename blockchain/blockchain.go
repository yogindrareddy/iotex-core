// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided ‘as is’ and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package blockchain

import (
	"math"
	"os"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/blockdb"
	cm "github.com/iotexproject/iotex-core/common"
	"github.com/iotexproject/iotex-core/config"
	cp "github.com/iotexproject/iotex-core/crypto"
	"github.com/iotexproject/iotex-core/iotxaddress"
	"github.com/iotexproject/iotex-core/proto"
	"github.com/iotexproject/iotex-core/txvm"
)

const (
	// GenesisCoinbaseData is the text in genesis block
	GenesisCoinbaseData = "The Times 03/Jan/2009 Chancellor on brink of second bailout for banks"
)

var (
	// ErrInvalidBlock is the error returned when the block is not vliad
	ErrInvalidBlock = errors.New("failed to validate the block")
)

// Blockchain implements the IBlockchain interface
type Blockchain struct {
	blockDb *blockdb.BlockDB
	config  *config.Config
	chainID uint32
	height  uint32
	tip     cp.Hash32B
	Utk     *UtxoTracker // tracks the current UTXO pool
}

// NewBlockchain creates a new blockchain instance
func NewBlockchain(db *blockdb.BlockDB, cfg *config.Config) *Blockchain {
	chain := &Blockchain{
		blockDb: db,
		config:  cfg,
		Utk:     NewUtxoTracker()}
	return chain
}

// Init initializes the blockchain
func (bc *Blockchain) Init() error {
	tip, height, err := bc.blockDb.Init()
	if err != nil {
		return err
	}

	copy(bc.tip[:], tip)
	bc.height = height

	// build UTXO pool
	// Genesis block has height 0
	for i := uint32(0); i <= bc.height; i++ {
		blk, err := bc.GetBlockByHeight(i)
		if err != nil {
			return err
		}
		bc.Utk.UpdateUtxoPool(blk)
	}
	return nil
}

// Close closes the Db connection
func (bc *Blockchain) Close() error {
	return bc.blockDb.Close()
}

// commitBlock commits Block to Db
func (bc *Blockchain) commitBlock(blk *Block) (err error) {
	// post-commit actions
	defer func() {
		// update tip hash and height
		if r := recover(); r != nil {
			return
		}

		// update tip hash/height
		bc.tip = blk.HashBlock()
		bc.height = blk.Header.height

		// update UTXO pool
		bc.Utk.UpdateUtxoPool(blk)
	}()

	// serialize the block
	serialized, err := blk.Serialize()
	if err != nil {
		panic(err)
	}

	hash := blk.HashBlock()
	if err = bc.blockDb.CheckInBlock(serialized, hash[:], blk.Header.height); err != nil {
		panic(err)
	}
	return
}

// GetHeightByHash returns block's height by hash
func (bc *Blockchain) GetHeightByHash(hash cp.Hash32B) (uint32, error) {
	return bc.blockDb.GetBlockHeight(hash[:])
}

// GetHashByHeight returns block's hash by height
func (bc *Blockchain) GetHashByHeight(height uint32) (cp.Hash32B, error) {
	hash := cp.ZeroHash32B
	dbHash, err := bc.blockDb.GetBlockHash(height)
	copy(hash[:], dbHash)
	return hash, err
}

// GetBlockByHeight returns block from the blockchain hash by height
func (bc *Blockchain) GetBlockByHeight(height uint32) (*Block, error) {
	hash, err := bc.GetHashByHeight(height)
	if err != nil {
		return nil, err
	}
	return bc.GetBlockByHash(hash)
}

// GetBlockByHash returns block from the blockchain hash by hash
func (bc *Blockchain) GetBlockByHash(hash cp.Hash32B) (*Block, error) {
	serialized, err := bc.blockDb.CheckOutBlock(hash[:])
	if err != nil {
		return nil, err
	}

	// deserialize the block
	blk := Block{}
	if err := blk.Deserialize(serialized); err != nil {
		return nil, err
	}
	return &blk, nil
}

// TipHash returns tip block's hash
func (bc *Blockchain) TipHash() cp.Hash32B {
	return bc.tip
}

// TipHeight returns tip block's height
func (bc *Blockchain) TipHeight() uint32 {
	return bc.height
}

// Reset reset for next block
func (bc *Blockchain) Reset() {
	bc.Utk.Reset()
}

// ValidateBlock validates a new block before adding it to the blockchain
func (bc *Blockchain) ValidateBlock(blk *Block) error {
	if blk == nil {
		return errors.Wrap(ErrInvalidBlock, "Block is nil")
	}
	// verify new block has correctly linked to current tip
	if blk.Header.prevBlockHash != bc.tip {
		return errors.Wrapf(ErrInvalidBlock, "Wrong prev hash %x, expecting %x", blk.Header.prevBlockHash, bc.tip)
	}

	// verify new block has height incremented by 1
	if blk.Header.height != 0 && blk.Header.height != bc.height+1 {
		return errors.Wrapf(ErrInvalidBlock, "Wrong block height %d, expecting %d", blk.Header.height, bc.height+1)
	}

	// validate all Tx conforms to blockchain protocol

	// validate UXTO contained in this Tx
	return bc.Utk.ValidateUtxo(blk)
}

// MintNewBlock creates a new block with given transactions.
// Note: the coinbase transaction will be added to the given transactions
// when minting a new block.
func (bc *Blockchain) MintNewBlock(txs []*Tx, toaddr, data string) *Block {
	txs = append(txs, NewCoinbaseTx(toaddr, bc.config.Chain.BlockReward, data))
	return NewBlock(bc.chainID, bc.height+1, bc.tip, txs)
}

// AddBlockCommit adds a new block into blockchain
func (bc *Blockchain) AddBlockCommit(blk *Block) error {
	if err := bc.ValidateBlock(blk); err != nil {
		return err
	}

	// commit block into blockchain DB
	return bc.commitBlock(blk)
}

// AddBlockSync adds a past block into blockchain
// used by block syncer when the chain in out-of-sync
func (bc *Blockchain) AddBlockSync(blk *Block) error {
	// directly commit block into blockchain DB
	return bc.commitBlock(blk)
}

// StoreBlock persists the blocks in the range to file on disk
func (bc *Blockchain) StoreBlock(start, end uint32) error {
	return bc.blockDb.StoreBlockToFile(start, end)
}

// ReadBlock read the block from file on disk
func (bc *Blockchain) ReadBlock(height uint32) *Block {
	file, err := os.Open(blockdb.BlockData)
	defer file.Close()
	if err != nil {
		glog.Error(err)
		return nil
	}

	// read block index
	indexSize := make([]byte, 4)
	file.Read(indexSize)
	size := cm.MachineEndian.Uint32(indexSize)
	indexBytes := make([]byte, size)
	if n, err := file.Read(indexBytes); err != nil || n != int(size) {
		glog.Error(err)
		return nil
	}
	blkIndex := iproto.BlockIndex{}
	if proto.Unmarshal(indexBytes, &blkIndex) != nil {
		glog.Error(err)
		return nil
	}

	// read the specific block
	index := height - blkIndex.Start
	file.Seek(int64(4+size+blkIndex.Offset[index]), 0)
	size = blkIndex.Offset[index+1] - blkIndex.Offset[index]
	blkBytes := make([]byte, size)
	if n, err := file.Read(blkBytes); err != nil || n != int(size) {
		glog.Error(err)
		return nil
	}
	blk := Block{}
	if blk.Deserialize(blkBytes) != nil {
		glog.Error(err)
		return nil
	}
	return &blk
}

// CreateBlockchain creates a new blockchain and DB instance
func CreateBlockchain(address string, cfg *config.Config) *Blockchain {
	db, dbFileExist := blockdb.NewBlockDB(cfg)
	if db == nil {
		glog.Error("cannot find db")
		return nil
	}
	chain := NewBlockchain(db, cfg)

	if dbFileExist {
		glog.Info("Blockchain already exists.")

		if err := chain.Init(); err != nil {
			glog.Fatalf("Failed to create Blockchain, error = %v", err)
			return nil
		}
		return chain
	}

	// create genesis block
	cbtx := NewCoinbaseTx(address, cfg.Chain.TotalSupply, GenesisCoinbaseData)
	genesis := NewBlock(chain.chainID, 0, cp.ZeroHash32B, []*Tx{cbtx})
	genesis.Header.timestamp = 0

	// Genesis block has height 0
	if genesis.Header.height != 0 {
		glog.Errorf("Genesis block has height = %d, expecting 0", genesis.Height())
		return nil
	}

	// add Genesis block as very first block
	if err := chain.AddBlockCommit(genesis); err != nil {
		glog.Error(err)
		return nil
	}
	return chain
}

// BalanceOf returns the balance of an address
func (bc *Blockchain) BalanceOf(address string) uint64 {
	_, balance := bc.Utk.UtxoEntries(address, math.MaxUint64)
	return balance
}

// UtxoPool returns the UTXO pool of current blockchain
func (bc *Blockchain) UtxoPool() map[cp.Hash32B][]*TxOutput {
	return bc.Utk.utxoPool
}

// createTx creates a transaction paying 'amount' from 'from' to 'to'
func (bc *Blockchain) createTx(from iotxaddress.Address, amount uint64, to []*Payee, isRaw bool) *Tx {
	utxo, change := bc.Utk.UtxoEntries(from.Address, amount)
	if utxo == nil {
		glog.Errorf("Fail to get UTXO for %v", from.Address)
		return nil
	}

	in := []*TxInput{}
	for _, out := range utxo {
		unlock := []byte(out.TxOutputPb.String())
		if !isRaw {
			var err error
			unlock, err = txvm.SignatureScript([]byte(out.TxOutputPb.String()), from.PublicKey, from.PrivateKey)
			if err != nil {
				return nil
			}
		}

		in = append(in, bc.Utk.CreateTxInputUtxo(out.txHash, out.outIndex, unlock))
	}

	out := []*TxOutput{}
	for _, payee := range to {
		out = append(out, bc.Utk.CreateTxOutputUtxo(payee.Address, payee.Amount))
	}
	if change > 0 {
		out = append(out, bc.Utk.CreateTxOutputUtxo(from.Address, change))
	}

	return NewTx(1, in, out, 0)
}

// CreateTransaction creates a signed transaction paying 'amount' from 'from' to 'to'
func (bc *Blockchain) CreateTransaction(from iotxaddress.Address, amount uint64, to []*Payee) *Tx {
	return bc.createTx(from, amount, to, false)
}

// CreateRawTransaction creates a unsigned transaction paying 'amount' from 'from' to 'to'
func (bc *Blockchain) CreateRawTransaction(from iotxaddress.Address, amount uint64, to []*Payee) *Tx {
	return bc.createTx(from, amount, to, true)
}
