package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"sync"
)

// GenesisHash 是创世块哈希（固定常量）。
const GenesisHash = "genesis"

// 区块验证相关错误。
var (
	ErrOrphan            = errors.New("orphan block: unknown parent")
	ErrInvalidBlock      = errors.New("invalid block")
	ErrStateRootMismatch = errors.New("state root mismatch")
	ErrUnknownBlock      = errors.New("unknown block")
	ErrBlockExists       = errors.New("block already exists")
)

// ReorgResult 描述一次重组的范围。
type ReorgResult struct {
	Forked     bool     // 是否真的发生了重组
	Ancestor   *Block   // 新旧链的共同祖先
	RolledBack []*Block // 被回滚的旧链区块（从高到低）
	Reapplied  []*Block // 重放的新链区块（从低到高）
	OldTip     *Block
	NewTip     *Block
}

// Chain 是一条支持分叉与重组的简化链。
type Chain struct {
	mu       sync.RWMutex
	state    *StateDB              // 当前规范链顶端状态
	blocks   map[string]*Block     // 所有已接受区块（含分叉）
	snaps    map[string]*StateDB   // 区块哈希 -> 执行该块后的状态快照
	receipts map[string][]*Receipt // 区块哈希 -> 收据（按规范顺序）
	tip      *Block
	logger   *log.Logger
}

// NewChain 用创世分配初始化链。
func NewChain(genesisAlloc map[Address]uint64, logger *log.Logger) *Chain {
	st := NewStateDB()
	for addr, bal := range genesisAlloc {
		st.credit(addr, bal)
	}
	genesis := &Block{
		Height:     0,
		ParentHash: "",
		Hash:       GenesisHash,
		StateRoot:  st.RootHash(),
	}
	c := &Chain{
		state:    st,
		blocks:   map[string]*Block{GenesisHash: genesis},
		snaps:    map[string]*StateDB{GenesisHash: st.Snapshot()},
		receipts: map[string][]*Receipt{GenesisHash: {}},
		tip:      genesis,
		logger:   logger,
	}
	c.logger.Printf("[chain] genesis created root=%s alloc=%d accounts", short(genesis.StateRoot), len(genesisAlloc))
	return c
}

// State 返回当前规范链状态的深拷贝快照。
func (c *Chain) State() *StateDB {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state.Snapshot()
}

// StateAtBlock 返回指定区块执行完成后状态的深拷贝快照（分叉块也可查询）。
func (c *Chain) StateAtBlock(hash string) (*StateDB, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.snaps[hash]
	if !ok {
		return nil, false
	}
	return st.Snapshot(), true
}

// Tip 返回当前规范链顶端区块。
func (c *Chain) Tip() *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tip
}

// GetBlock 按哈希取区块（含分叉区块）。
func (c *Chain) GetBlock(hash string) (*Block, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.blocks[hash]
	return b, ok
}

// BlockAtHeight 返回规范链上指定高度的区块。
func (c *Chain) BlockAtHeight(height uint64) (*Block, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cur := c.tip
	if height > cur.Height {
		return nil, false
	}
	path := c.pathToGenesisLocked(cur) // 从高到低
	idx := len(path) - 1 - int(height)
	if idx < 0 || idx >= len(path) {
		return nil, false
	}
	return path[idx], true
}

// ReceiptsOf 返回某区块的收据。
func (c *Chain) ReceiptsOf(blockHash string) ([]*Receipt, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rs, ok := c.receipts[blockHash]
	if !ok {
		return nil, false
	}
	out := make([]*Receipt, len(rs))
	copy(out, rs)
	return out, true
}

// TxReceipt 在规范链上按交易哈希查找收据（含所在区块哈希）。
func (c *Chain) TxReceipt(txHash string) (string, *Receipt, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cur := c.tip
	for cur.Hash != GenesisHash {
		for _, r := range c.receipts[cur.Hash] {
			if r.TxHash == txHash {
				return cur.Hash, r, true
			}
		}
		cur = c.blocks[cur.ParentHash]
	}
	return "", nil, false
}

// pathToGenesisLocked 返回区块到创世（不含创世）的路径，顺序为从高到低（调用方持锁）。
func (c *Chain) pathToGenesisLocked(b *Block) []*Block {
	var path []*Block
	for b != nil && b.Hash != GenesisHash {
		path = append(path, b)
		b = c.blocks[b.ParentHash]
	}
	return path
}

// pathFromAncestorLocked 返回 ancestor 之后（不含）到 tip 的区块，顺序为从低到高。
func (c *Chain) pathFromAncestorLocked(tip *Block, ancestorHash string) []*Block {
	down := c.pathToGenesisLocked(tip) // 高 -> 低
	var up []*Block
	for _, b := range down {
		if b.Hash == ancestorHash {
			break
		}
		up = append(up, b)
	}
	// 反转为低 -> 高
	for i, j := 0, len(up)-1; i < j; i, j = i+1, j-1 {
		up[i], up[j] = up[j], up[i]
	}
	return up
}

// TotalWeight 返回某区块所在分支的累计权重（沿父链求和，含创世之上的所有区块）。
func (c *Chain) TotalWeight(b *Block) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var w uint64
	for b != nil && b.Hash != GenesisHash {
		w += b.Weight
		b = c.blocks[b.ParentHash]
	}
	return w
}

// verifyBlock 在父块快照上按规范顺序执行区块并校验状态根，返回执行后状态与收据。
// 同时要求区块内交易必须已经是规范顺序，保证任何入口提交的块都遵循同一规则。
func (c *Chain) verifyBlock(parent *Block, b *Block) (*StateDB, []*Receipt, error) {
	if b.Height != parent.Height+1 {
		return nil, nil, fmt.Errorf("%w: height %d does not extend parent height %d", ErrInvalidBlock, b.Height, parent.Height)
	}
	if b.ParentHash != parent.Hash {
		return nil, nil, fmt.Errorf("%w: parent hash mismatch", ErrInvalidBlock)
	}
	if b.Hash != b.BlockHash() {
		return nil, nil, fmt.Errorf("%w: block hash mismatch", ErrInvalidBlock)
	}
	// 交易必须按规范顺序排列，避免“同批不同序”的歧义。
	for i := 1; i < len(b.Transactions); i++ {
		if txLess(b.Transactions[i], b.Transactions[i-1]) {
			return nil, nil, fmt.Errorf("%w: transactions not in canonical order", ErrInvalidBlock)
		}
	}
	st := c.snaps[parent.Hash].Snapshot()
	rs := make([]*Receipt, len(b.Transactions))
	for i, t := range b.Transactions {
		rs[i] = st.Apply(t, b.Coinbase)
		if !rs[i].Success {
			// 区块中不允许静默失败的交易：构造块时即应过滤；直连接口也明确拒绝。
			return nil, nil, fmt.Errorf("%w: tx %s included but invalid: %v", ErrInvalidBlock, short(t.TxHash()), rs[i].Reason)
		}
	}
	if st.RootHash() != b.StateRoot {
		c.logger.Printf("[chain] VERIFY-FAIL block=%s claimed_root=%s computed_root=%s",
			short(b.Hash), short(b.StateRoot), st.RootHash()[:12])
		return nil, nil, fmt.Errorf("%w: block=%s", ErrStateRootMismatch, short(b.Hash))
	}
	return st, rs, nil
}

// txLess 按规范顺序比较两笔交易（a 是否应排在 b 前）。
func txLess(a, b *Transaction) bool {
	if c := bytes.Compare(a.From[:], b.From[:]); c != 0 {
		return c < 0
	}
	if a.Nonce != b.Nonce {
		return a.Nonce < b.Nonce
	}
	if c := bytes.Compare(a.To[:], b.To[:]); c != 0 {
		return c < 0
	}
	if a.Amount != b.Amount {
		return a.Amount < b.Amount
	}
	return a.TxHash() < b.TxHash()
}

// AcceptBlock 验证并接受一个区块；若它（或其所在分支）累计权重超过当前规范链则触发重组。
func (c *Chain) AcceptBlock(b *Block) (*ReorgResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.blocks[b.Hash]; exists {
		return &ReorgResult{Forked: false, OldTip: c.tip, NewTip: c.tip}, ErrBlockExists
	}
	parent, ok := c.blocks[b.ParentHash]
	if !ok {
		return nil, fmt.Errorf("%w: parent=%s height=%d", ErrOrphan, b.ParentHash, b.Height)
	}
	st, rs, err := c.verifyBlock(parent, b)
	if err != nil {
		return nil, err
	}
	c.blocks[b.Hash] = b
	c.snaps[b.Hash] = st
	c.receipts[b.Hash] = rs
	c.logger.Printf("[chain] ACCEPT block=%s height=%d parent=%s weight=%d txs=%d root=%s",
		short(b.Hash), b.Height, short(b.ParentHash), b.Weight, len(b.Transactions), short(b.StateRoot))

	// 只有新累计权重严格更大才切换规范链（相等时保留旧链，保证确定性）。
	newWeight := c.totalWeightLocked(b)
	oldWeight := c.totalWeightLocked(c.tip)
	c.logger.Printf("[chain] branch compare: new_branch=%d (tip=%s) vs canonical=%d (tip=%s)",
		newWeight, short(b.Hash), oldWeight, short(c.tip.Hash))
	if newWeight <= oldWeight {
		return &ReorgResult{Forked: false, OldTip: c.tip, NewTip: c.tip}, nil
	}
	res := c.reorgLocked(b)
	return res, nil
}

// totalWeightLocked 累计权重（调用方持锁）。
func (c *Chain) totalWeightLocked(b *Block) uint64 {
	var w uint64
	for b != nil && b.Hash != GenesisHash {
		w += b.Weight
		b = c.blocks[b.ParentHash]
	}
	return w
}

// reorgLocked 回滚旧链到共同祖先、重放新链，并用反向状态转移校验回滚自洽（调用方持锁）。
func (c *Chain) reorgLocked(newTip *Block) *ReorgResult {
	oldTip := c.tip
	ancestor := c.ancestorLocked(oldTip, newTip)
	rollback := c.pathFromAncestorLocked(oldTip, ancestor.Hash) // 低 -> 高
	reapply := c.pathFromAncestorLocked(newTip, ancestor.Hash)  // 低 -> 高

	c.logger.Printf("[chain] REORG START old_tip=%s new_tip=%s ancestor=%s(height=%d) rollback=%d reapply=%d",
		short(oldTip.Hash), short(newTip.Hash), short(ancestor.Hash), ancestor.Height, len(rollback), len(reapply))

	// 1) 回滚：从顶端向下，对每个块逐笔反向执行成功交易（顺序为块内倒序）。
	//    回滚结束后的状态必须与共同祖先快照逐字节一致，否则说明扣款/退款不自洽。
	rollState := c.state.Snapshot()
	rolled := make([]*Block, 0, len(rollback))
	for i := len(rollback) - 1; i >= 0; i-- {
		blk := rollback[i]
		for j := len(blk.Transactions) - 1; j >= 0; j-- {
			rollState.reverse(blk.Transactions[j], blk.Coinbase)
		}
		rolled = append(rolled, blk)
		c.logger.Printf("[chain] REORG rollback block=%s height=%d reversed=%d txs (coinbase fee refunded)",
			short(blk.Hash), blk.Height, len(blk.Transactions))
	}
	if rollState.RootHash() != c.snaps[ancestor.Hash].RootHash() {
		// 不变量被破坏：回滚后状态必须精确回到祖先状态。
		c.logger.Fatalf("[chain] REORG INVARIANT BROKEN: post-rollback root=%s ancestor root=%s",
			rollState.RootHash()[:12], c.snaps[ancestor.Hash].RootHash()[:12])
	}

	// 2) 重放：直接采用新链各块验证时保存的快照（重放结果与快照一致即可切换）。
	//    为体现“重放”过程，这里在祖先快照上再次执行并逐块比对根。
	replay := c.snaps[ancestor.Hash].Snapshot()
	for _, blk := range reapply {
		for _, t := range blk.Transactions {
			r := replay.Apply(t, blk.Coinbase)
			if !r.Success {
				c.logger.Fatalf("[chain] REORG INVARIANT BROKEN: re-applied tx=%s failed: %v", short(t.TxHash()), r.Reason)
			}
		}
		if replay.RootHash() != c.snaps[blk.Hash].RootHash() {
			c.logger.Fatalf("[chain] REORG INVARIANT BROKEN: replay root mismatch at block=%s", short(blk.Hash))
		}
		c.logger.Printf("[chain] REORG reapply block=%s height=%d txs=%d root=%s",
			short(blk.Hash), blk.Height, len(blk.Transactions), replay.RootHash()[:12])
	}

	// 3) 原子切换规范链顶端状态。
	c.tip = newTip
	c.state = replay
	c.logger.Printf("[chain] REORG DONE new_tip=%s height=%d root=%s",
		short(newTip.Hash), newTip.Height, c.state.RootHash()[:12])

	return &ReorgResult{
		Forked:     true,
		Ancestor:   ancestor,
		RolledBack: rolled, // 高 -> 低
		Reapplied:  reapply,
		OldTip:     oldTip,
		NewTip:     newTip,
	}
}

// ancestorLocked 查找两区块的共同祖先（调用方持锁）。
func (c *Chain) ancestorLocked(a, b *Block) *Block {
	seen := make(map[string]struct{})
	for a != nil {
		seen[a.Hash] = struct{}{}
		a = c.blocks[a.ParentHash]
	}
	for b != nil {
		if _, ok := seen[b.Hash]; ok {
			return b
		}
		b = c.blocks[b.ParentHash]
	}
	return c.blocks[GenesisHash]
}

// Balance 是查询当前规范链账户余额的便捷方法。
func (c *Chain) Balance(addr Address) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state.Balance(addr)
}

// short 安全地截断日志里使用的哈希前缀（genesis 等短串不会越界）。
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
