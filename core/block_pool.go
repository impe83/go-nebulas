// Copyright (C) 2017 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package core

import (
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	lru "github.com/hashicorp/golang-lru"
	"github.com/nebulasio/go-nebulas/core/pb"
	"github.com/nebulasio/go-nebulas/net"
	"github.com/nebulasio/go-nebulas/net/p2p"
	"github.com/nebulasio/go-nebulas/util/byteutils"
	"github.com/nebulasio/go-nebulas/util/logging"
	"github.com/sirupsen/logrus"
)

// constants
const (
	NoSender = ""
)

// BlockPool a pool of all received blocks from network.
// Blocks will be sent to Consensus when it passes signature verification.
type BlockPool struct {
	size                          int
	receiveBlockMessageCh         chan net.Message
	receiveDownloadBlockMessageCh chan net.Message
	quitCh                        chan int

	bc    *BlockChain
	cache *lru.Cache
	slot  *lru.Cache

	nm p2p.Manager
	mu sync.RWMutex
}

type linkedBlock struct {
	block      *Block
	pool       *BlockPool
	hash       byteutils.Hash
	parentHash byteutils.Hash

	parentBlock *linkedBlock
	childBlocks map[byteutils.HexHash]*linkedBlock
}

// NewBlockPool return new #BlockPool instance.
func NewBlockPool(size int) (*BlockPool, error) {
	bp := &BlockPool{
		size: size,
		receiveBlockMessageCh:         make(chan net.Message, size),
		receiveDownloadBlockMessageCh: make(chan net.Message, size),
		quitCh: make(chan int, 1),
	}
	var err error
	bp.cache, err = lru.New(size)
	if err != nil {
		return nil, err
	}
	bp.slot, _ = lru.New(size)
	if err != nil {
		return nil, err
	}
	return bp, nil
}

// RegisterInNetwork register message subscriber in network.
func (pool *BlockPool) RegisterInNetwork(nm p2p.Manager) {
	nm.Register(net.NewSubscriber(pool, pool.receiveBlockMessageCh, MessageTypeNewBlock))
	nm.Register(net.NewSubscriber(pool, pool.receiveBlockMessageCh, MessageTypeDownloadedBlockReply))
	nm.Register(net.NewSubscriber(pool, pool.receiveDownloadBlockMessageCh, MessageTypeDownloadedBlock))
	pool.nm = nm
}

// Start start loop.
func (pool *BlockPool) Start() {
	logging.CLog().WithFields(logrus.Fields{
		"size": pool.size,
	}).Info("Starting BlockPool...")

	go pool.loop()
}

// Stop stop loop.
func (pool *BlockPool) Stop() {
	logging.CLog().WithFields(logrus.Fields{
		"size": pool.size,
	}).Info("Stopping BlockPool...")

	pool.quitCh <- 0
}

func (pool *BlockPool) handleBlock(msg net.Message) {
	if msg.MessageType() != MessageTypeNewBlock && msg.MessageType() != MessageTypeDownloadedBlockReply {
		logging.VLog().WithFields(logrus.Fields{
			"msgType": msg.MessageType(),
			"msg":     msg,
			"err":     "neither new block nor download block response msg",
		}).Debug("Received unregistered message.")
		return
	}

	block := new(Block)
	pbblock := new(corepb.Block)
	if err := proto.Unmarshal(msg.Data().([]byte), pbblock); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"msgType": msg.MessageType(),
			"msg":     msg,
			"err":     err,
		}).Debug("Failed to unmarshal data.")
		return
	}
	if err := block.FromProto(pbblock); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"msgType": msg.MessageType(),
			"msg":     msg,
			"err":     err,
		}).Debug("Failed to recover a block from proto data.")
		return
	}

	diff := time.Now().Unix() - block.Timestamp()
	if msg.MessageType() == MessageTypeNewBlock && int64(math.Abs(float64(diff))) > AcceptedNetWorkDelay {
		logging.VLog().WithFields(logrus.Fields{
			"block": block,
			"diff":  diff,
			"limit": AcceptedNetWorkDelay,
			"err":   "timeout",
		}).Debug("Found a timeout block.")
	}

	logging.VLog().WithFields(logrus.Fields{
		"block": block,
		"type":  msg.MessageType(),
	}).Debug("Received a new block.")

	pool.PushAndRelay(msg.MessageFrom(), block)
}

func (pool *BlockPool) handleDownloadedBlock(msg net.Message) {
	if msg.MessageType() != MessageTypeDownloadedBlock {
		logging.VLog().WithFields(logrus.Fields{
			"messageType": msg.MessageType(),
			"message":     msg,
			"err":         "wrong msg type",
		}).Debug("Failed to received a download request.")
		return
	}

	pbDownloadBlock := new(corepb.DownloadBlock)
	if err := proto.Unmarshal(msg.Data().([]byte), pbDownloadBlock); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"msgType": msg.MessageType(),
			"msg":     msg,
			"err":     err,
		}).Debug("Failed to unmarshal data.")
		return
	}

	if byteutils.Equal(pbDownloadBlock.Hash, GenesisHash) {
		logging.VLog().WithFields(logrus.Fields{
			"download.hash": byteutils.Hex(pbDownloadBlock.Hash),
		}).Debug("Asked to download genesis's parent, ignore it.")
		return
	}

	block := pool.bc.GetBlock(pbDownloadBlock.Hash)
	if block == nil {
		logging.VLog().WithFields(logrus.Fields{
			"download.hash": byteutils.Hex(pbDownloadBlock.Hash),
		}).Debug("Failed to find the block asked for.")
		return
	}

	if !block.Signature().Equals(pbDownloadBlock.Sign) {
		logging.VLog().WithFields(logrus.Fields{
			"download.hash": byteutils.Hex(pbDownloadBlock.Hash),
			"download.sign": byteutils.Hex(pbDownloadBlock.Sign),
			"expect.sign":   block.Signature().Hex(),
		}).Debug("Failed to check the block's signature.")
		return
	}

	parent := pool.bc.GetBlock(block.header.parentHash)
	if parent == nil {
		logging.VLog().WithFields(logrus.Fields{
			"block": block,
		}).Debug("Failed to find the block's parent.")
		return
	}

	pbBlock, err := parent.ToProto()
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"parent": parent,
			"err":    err,
		}).Debug("Failed to convert the block's parent to proto data.")
		return
	}
	bytes, err := proto.Marshal(pbBlock)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"parent": parent,
			"err":    err,
		}).Debug("Failed to marshal the block's parent.")
		return
	}
	pool.nm.SendMsg(MessageTypeDownloadedBlockReply, bytes, msg.MessageFrom(), net.MessagePriorityNormal)

	logging.VLog().WithFields(logrus.Fields{
		"block":  block,
		"parent": parent,
	}).Debug("Responsed to the download request.")
}

func (pool *BlockPool) loop() {
	logging.CLog().Info("Started BlockPool.")
	timerChan := time.NewTicker(time.Second).C
	for {
		select {
		case <-timerChan:
			metricsCachedNewBlock.Update(int64(len(pool.receiveBlockMessageCh)))
			metricsCachedDownloadBlock.Update(int64(len(pool.receiveDownloadBlockMessageCh)))
		case <-pool.quitCh:
			logging.CLog().Info("Stopped BlockPool.")
			return
		case msg := <-pool.receiveBlockMessageCh:
			pool.handleBlock(msg)
		case msg := <-pool.receiveDownloadBlockMessageCh:
			pool.handleDownloadedBlock(msg)
		}
	}
}

func mockBlockFromNetwork(block *Block) (*Block, error) {
	pbBlock, err := block.ToProto()
	if err != nil {
		return nil, err
	}
	bytes, err := proto.Marshal(pbBlock)
	if err := proto.Unmarshal(bytes, pbBlock); err != nil {
		return nil, err
	}
	block = new(Block)
	block.FromProto(pbBlock)
	return block, nil
}

// Push block into block pool
func (pool *BlockPool) Push(block *Block) error {
	startAt := time.Now().Unix()
	defer logging.VLog().WithFields(logrus.Fields{
		"block": block,
		"time":  time.Now().Unix() - startAt,
	}).Debug("Push Block Over.")

	pool.mu.Lock()
	defer pool.mu.Unlock()
	block, err := mockBlockFromNetwork(block)
	if err != nil {
		return err
	}
	pushErr := pool.push(NoSender, block)
	if pushErr != nil && pushErr != ErrDuplicatedBlock {
		return pushErr
	}
	return nil
}

// PushAndRelay push block into block pool and relay it.
func (pool *BlockPool) PushAndRelay(sender string, block *Block) error {
	startAt := time.Now().Unix()
	defer logging.VLog().WithFields(logrus.Fields{
		"block": block,
		"time":  time.Now().Unix() - startAt,
	}).Debug("Push&Relay Block Over.")

	pool.mu.Lock()
	defer pool.mu.Unlock()

	block, err := mockBlockFromNetwork(block)
	if err != nil {
		return err
	}
	if err := pool.push(sender, block); err != nil {
		return err
	}
	pool.nm.Relay(MessageTypeNewBlock, block, net.MessagePriorityHigh)
	return nil
}

// PushAndBroadcast push block into block pool and broadcast it.
func (pool *BlockPool) PushAndBroadcast(block *Block) error {
	startAt := time.Now().Unix()
	defer logging.VLog().WithFields(logrus.Fields{
		"block": block,
		"time":  time.Now().Unix() - startAt,
	}).Debug("Push&Broadcast Block Over.")

	pool.mu.Lock()
	defer pool.mu.Unlock()

	block, err := mockBlockFromNetwork(block)
	if err != nil {
		return err
	}
	if err := pool.push(NoSender, block); err != nil {
		return err
	}
	pool.nm.Broadcast(MessageTypeNewBlock, block, net.MessagePriorityHigh)
	return nil
}

func (pool *BlockPool) download(sender string, block *Block) error {
	downloadMsg := &corepb.DownloadBlock{
		Hash: block.Hash(),
		Sign: block.Signature(),
	}
	bytes, err := proto.Marshal(downloadMsg)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"block": block,
			"err":   err,
		}).Debug("Failed to send download request.")
		return err
	}

	pool.nm.SendMsg(MessageTypeDownloadedBlock, bytes, sender, net.MessagePriorityNormal)

	logging.VLog().WithFields(logrus.Fields{
		"target": sender,
		"block":  block,
		"tail":   pool.bc.TailBlock(),
		"gap":    strconv.Itoa(int(block.Timestamp()-pool.bc.TailBlock().Timestamp())) + "s",
		"limit":  strconv.Itoa(int(DynastyInterval)) + "s",
	}).Info("Send download request.")

	return nil
}

func (pool *BlockPool) push(sender string, block *Block) error {
	logging.VLog().WithFields(logrus.Fields{
		"block": block,
	}).Debug("Try to push a new block.")

	startAt := time.Now().UnixNano()

	// verify non-dup block
	if pool.cache.Contains(block.Hash().Hex()) ||
		pool.bc.GetBlock(block.Hash()) != nil {
		metricsDuplicatedBlock.Inc(1)
		logging.VLog().WithFields(logrus.Fields{
			"block": block,
		}).Debug("Found duplicated block.")
		return ErrDuplicatedBlock
	}
	checkDupAt := time.Now().UnixNano()

	// verify block integrity
	if err := block.VerifyIntegrity(pool.bc.chainID, pool.bc.ConsensusHandler()); err != nil {
		metricsInvalidBlock.Inc(1)
		logging.VLog().WithFields(logrus.Fields{
			"block": block,
			"err":   err,
		}).Debug("Failed to check block integrity.")
		return err
	}
	checkIntegrityAt := time.Now().UnixNano()

	bc := pool.bc
	cache := pool.cache

	var plb *linkedBlock
	lb := newLinkedBlock(block, pool)

	if preBlock, exist := pool.slot.Get(lb.block.Timestamp()); exist {
		metricsInvalidBlock.Inc(1)
		logging.VLog().WithFields(logrus.Fields{
			"curBlock": lb.block,
			"preBlock": preBlock.(*Block),
			"sender":   sender,
		}).Warn("Found someone minted multiple blocks at same time.")
		return ErrDoubleBlockMinted
	}
	pool.slot.Add(lb.block.Timestamp(), lb.block)
	cache.Add(lb.hash.Hex(), lb)
	checkSlotAt := time.Now().UnixNano()

	// find child block in pool.
	for _, k := range cache.Keys() {
		v, _ := cache.Get(k)
		c := v.(*linkedBlock)
		if c.parentHash.Equals(lb.hash) {
			// found child block and continue.
			c.LinkParent(lb)
		}
	}
	findChildrenAt := time.Now().UnixNano()

	// find parent block in cache.
	v, _ := cache.Get(lb.parentHash.Hex())
	if v != nil {
		// found in cache.
		plb = v.(*linkedBlock)
		lb.LinkParent(plb)

		for plb.parentBlock != nil {
			plb = plb.parentBlock
		}

		logging.VLog().WithFields(logrus.Fields{
			"block": plb.block,
		}).Warn("Found unlinked ancestor.")

		if sender == NoSender {
			return ErrMissingParentBlock
		}

		if err := pool.download(sender, plb.block); err != nil {
			return err
		}

		return nil
	}
	findParentAt := time.Now().UnixNano()

	// find parent in Chain.
	var parentBlock *Block
	if parentBlock = bc.GetBlock(lb.parentHash); parentBlock == nil {
		// still not found, wait to parent block from network.
		if sender == NoSender {
			return ErrMissingParentBlock
		}

		// do sync if there are so many empty slots.
		if int(lb.block.Height())-int(bc.TailBlock().Height()) > ChunkSize {
			if bc.StartActiveSync() {
				logging.CLog().WithFields(logrus.Fields{
					"tail":    bc.tailBlock,
					"block":   block,
					"offline": strconv.Itoa(int(lb.block.Timestamp()-bc.TailBlock().Timestamp())) + "s",
					"limit":   strconv.Itoa(int(DynastyInterval)) + "s",
				}).Warn("Offline too long, pend mining and restart sync from others.")
			}
			return ErrInvalidBlockCannotFindParentInLocalAndTrySync
		}

		if err := pool.download(sender, lb.block); err != nil {
			return err
		}
		return ErrInvalidBlockCannotFindParentInLocalAndTryDownload
	}
	getParentAt := time.Now().UnixNano()

	// found in BlockChain, then we can verify the state root, and tell the Consensus all the tails.
	// performance depth-first search to verify state root, and get all tails.
	allBlocks, tailBlocks, err := lb.travelToLinkAndReturnAllValidBlocks(parentBlock)
	if err != nil {
		return err
	}
	verifyAt := time.Now().UnixNano()

	if err := bc.putVerifiedNewBlocks(parentBlock, allBlocks, tailBlocks); err != nil {
		return err
	}
	putAt := time.Now().UnixNano()

	// remove allBlocks from cache.
	for _, v := range allBlocks {
		cache.Remove(v.Hash().Hex())
	}

	// notify consensus to handle new block.
	if err := pool.bc.ConsensusHandler().ForkChoice(); err != nil {
		return err
	}
	forkchoiceAt := time.Now().UnixNano()

	logging.VLog().WithFields(logrus.Fields{
		"startAt":             startAt,
		"time.checkdup":       checkDupAt - startAt,
		"time.checkintegrity": checkIntegrityAt - checkDupAt,
		"time.slot":           checkSlotAt - checkIntegrityAt,
		"time.findchildern":   findChildrenAt - checkSlotAt,
		"time.findparent":     findParentAt - findChildrenAt,
		"time.getparent":      getParentAt - findParentAt,
		"time.verify":         verifyAt - getParentAt,
		"time.put":            putAt - verifyAt,
		"time.forkchoice":     forkchoiceAt - putAt,
		"time.push":           time.Now().UnixNano() - startAt,
	}).Info("Succeed to put a block on chain.")

	return nil
}

func (pool *BlockPool) setBlockChain(bc *BlockChain) {
	pool.bc = bc
}

func newLinkedBlock(block *Block, pool *BlockPool) *linkedBlock {
	return &linkedBlock{
		block:       block,
		pool:        pool,
		hash:        block.Hash(),
		parentHash:  block.ParentHash(),
		parentBlock: nil,
		childBlocks: make(map[byteutils.HexHash]*linkedBlock),
	}
}

func (lb *linkedBlock) LinkParent(parentBlock *linkedBlock) {
	lb.parentBlock = parentBlock
	parentBlock.childBlocks[lb.hash.Hex()] = lb
}

func (lb *linkedBlock) travelToLinkAndReturnAllValidBlocks(parentBlock *Block) ([]*Block, []*Block, error) {
	startAt := time.Now().UnixNano()

	if err := lb.block.LinkParentBlock(parentBlock); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"parent": parentBlock,
			"block":  lb.block,
			"err":    err,
		}).Error("Failed to link the block with its parent.")
		return nil, nil, err
	}
	linkAt := time.Now().UnixNano()

	if err := lb.block.VerifyExecution(parentBlock, lb.pool.bc.ConsensusHandler()); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"block": lb.block,
			"err":   err,
		}).Error("Failed to execute block.")
		return nil, nil, err
	}
	executionAt := time.Now().UnixNano()

	logging.VLog().WithFields(logrus.Fields{
		"block":          lb.block,
		"time.link":      linkAt - startAt,
		"time.execution": executionAt - linkAt,
		"time.verified":  time.Now().UnixNano() - startAt,
	}).Info("Block Verified.")

	allBlocks := []*Block{lb.block}
	tailBlocks := []*Block{}

	if len(lb.childBlocks) == 0 {
		tailBlocks = append(tailBlocks, lb.block)
	}

	for _, clb := range lb.childBlocks {
		a, b, err := clb.travelToLinkAndReturnAllValidBlocks(lb.block)
		if err == nil {
			allBlocks = append(allBlocks, a...)
			tailBlocks = append(tailBlocks, b...)
		}
	}

	return allBlocks, tailBlocks, nil
}
