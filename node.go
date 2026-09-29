package ledger

import (
	"log"
)

// Node 在链与交易池之上封装并发提交与出块入口。
type Node struct {
	chain    *Chain
	pool     *Pool
	coinbase Address
	logger   *log.Logger
}

// NewNode 创建一个节点。
func NewNode(chain *Chain, coinbase Address, poolCap, futureCap int, logger *log.Logger) *Node {
	return &Node{
		chain:    chain,
		pool:     NewPool(chain, poolCap, futureCap, logger),
		coinbase: coinbase,
		logger:   logger,
	}
}

// Submit 并发安全地提交一笔待打包交易。
func (n *Node) Submit(t *Transaction) error {
	return n.pool.Add(t)
}

// ProduceBlock 从交易池确定性选出交易并生产、提交一个新区块。
//
// 流程：
//  1. 从池确定性 Select（与提交顺序无关）；
//  2. 在当前链状态快照上按规范顺序执行，剔除非法交易（如重组后余额已不足）；
//  3. 构造区块（交易按规范顺序写入）、计算状态根并提交给链；
//  4. 从池中移除已打包交易；若触发重组则同步交易池。
func (n *Node) ProduceBlock(weight uint64, maxTxs int) (*Block, error) {
	parent := n.chain.Tip()
	selected := n.pool.Select(maxTxs)
	n.logger.Printf("[node] produce: parent=%s height=%d weight=%d selected=%d/%d ready txs",
		short(parent.Hash), parent.Height+1, weight, len(selected), n.pool.PendingSize())

	// 在链状态快照上预执行，剔除此时已不合法的交易（例如重组后 nonce/余额变化）。
	st := n.chain.State()
	exec := make([]*Transaction, 0, len(selected))
	for _, t := range selected {
		r := st.Apply(t, n.coinbase)
		if !r.Success {
			n.logger.Printf("[node] produce: drop tx=%s at block-build time reason=%v", short(t.TxHash()), r.Reason)
			n.pool.Remove(t.TxHash())
			continue
		}
		exec = append(exec, t)
	}

	blk := &Block{
		Height:       parent.Height + 1,
		ParentHash:   parent.Hash,
		Coinbase:     n.coinbase,
		Weight:       weight,
		Transactions: exec, // Select 已按规范顺序返回
	}
	blk.Hash = blk.BlockHash()
	blk.StateRoot = st.RootHash()

	res, err := n.chain.AcceptBlock(blk)
	if err != nil {
		n.logger.Printf("[node] produce: block=%s rejected reason=%v", short(blk.Hash), err)
		return nil, err
	}

	// 从池中移除已打包交易。
	for _, t := range exec {
		n.pool.Remove(t.TxHash())
	}
	// 若发生重组，同步交易池：新链已含交易移除、被回滚交易重新入池。
	if res.Forked {
		n.pool.OnReorg(res)
	}
	n.logger.Printf("[node] produce: block=%s accepted height=%d txs=%d root=%s reorg=%v",
		short(blk.Hash), blk.Height, len(exec), short(blk.StateRoot), res.Forked)
	return blk, nil
}

// Chain 返回底层链。
func (n *Node) Chain() *Chain { return n.chain }

// Pool 返回底层交易池。
func (n *Node) Pool() *Pool { return n.pool }
