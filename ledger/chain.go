package ledger

// blockNode 保存区块、其执行收据以及在父结果状态上执行本块后的状态。
type blockNode struct {
	block    *Block
	parent   Hash
	receipts []*Receipt
	state    *StateDB
	weight   uint64 // 从创世到本块的累计权重
}

// Chain 维护区块树、规范链（canonical chain）及当前账本状态。
type Chain struct {
	genesis    *StateDB
	nodes      map[string]*blockNode // blockHash(Hex) -> node
	children   map[string][]Hash     // parentHash(Hex) -> child hashes
	head       Hash
	headHeight uint64
	// canonical 记录当前规范链上每个高度对应的区块哈希，便于收据查询与审计。
	canonical map[uint64]Hash
}

// NewChain 以创世状态初始化链（创世块哈希为空、高度为 0）。
func NewChain(genesis *StateDB) *Chain {
	return &Chain{
		genesis:  genesis.Copy(),
		nodes:    map[string]*blockNode{},
		children: map[string][]Hash{},
		canonical: map[uint64]Hash{
			0: ZeroHash,
		},
	}
}

// Head 返回当前规范链头区块（nil 表示仅有创世）。
func (c *Chain) Head() *Block {
	if n, ok := c.nodes[string(c.head)]; ok {
		return n.block
	}
	return nil
}

// State 返回当前规范链头对应的账本状态（调用方只读使用；修改请先 Copy）。
func (c *Chain) State() *StateDB {
	if n, ok := c.nodes[string(c.head)]; ok {
		return n.state
	}
	return c.genesis
}

// BlockWeight 定义单个区块对分支权重的贡献：1 + 块内交易数。
// 交易越多的有效分支累计权重越高（简化版工作量证明）。
func BlockWeight(b *Block) uint64 {
	if b == nil {
		return 0
	}
	return 1 + uint64(len(b.Txs))
}

// MakeBlock 由节点在打包后构造新区块（哈希在首次调用 Hash 时计算并缓存）。
func MakeBlock(parent Hash, height uint64, txs []*Transaction) *Block {
	return &Block{
		Height:     height,
		ParentHash: append(Hash(nil), parent...),
		Txs:        txs,
	}
}

// parentHashOrZero 把任意空表示统一成 ZeroHash(nil)，
// 保证“创世之父”这一哨兵在各处 len 判定一致。
func parentHashOrZero(h Hash) Hash {
	if len(h) == 0 {
		return ZeroHash
	}
	return append(Hash(nil), h...)
}

// AddBlock 导入一个新区块。
//
// 执行模型：在“父区块的结果状态”副本上确定性执行全部交易并保存收据，
// 因此任意分支上的每个区块结论都只依赖其父状态，与导入顺序无关。
//
// 分支选择：累计权重（逐块 BlockWeight 求和）严格大于当前规范链才切换；
// 平手时保留当前链，保证选择确定、无抖动。
//
// 重组：找到新旧链的最近共同祖先，沿旧链逐块 RevertBlock 回滚到分叉点，
// 再沿新链逐块 ExecuteBlock 重放。余额、nonce、唯一性索引、收据全部
// 由效果日志恢复/重建，杜绝重复扣款、漏扣与已确认交易静默丢弃。
func (c *Chain) AddBlock(b *Block) (*ReorgReport, error) {
	if b == nil {
		return nil, ErrUnknownParent
	}
	h := b.Hash()
	if _, exists := c.nodes[string(h)]; exists {
		return nil, rejectTx(ReasonBlockExists)
	}
	parent, ok := c.nodes[string(b.ParentHash)]
	if !ok {
		// 空父哈希仅允许高度为 1 的区块（创世之子）。
		if !(len(b.ParentHash) == 0 && b.Height == 1) {
			return nil, rejectTx(ReasonUnknownParent)
		}
		if b.Height != 1 {
			return nil, rejectTx(ReasonUnknownParent)
		}
	}
	if parent != nil && b.Height != parent.block.Height+1 {
		return nil, rejectTx(ReasonAncestorGap)
	}

	// 在父结果状态副本上执行，得到该分支上此区块的确定结论。
	base := c.genesis
	if parent != nil {
		base = parent.state
	}
	execState := base.Copy()
	receipts := execState.ExecuteBlock(h, b.Txs)

	weight := BlockWeight(b)
	if parent != nil {
		weight += parent.weight
	}
	node := &blockNode{
		block:    b,
		parent:   parentHashOrZero(b.ParentHash),
		receipts: receipts,
		state:    execState,
		weight:   weight,
	}
	c.nodes[string(h)] = node
	c.children[string(b.ParentHash)] = append(c.children[string(b.ParentHash)], h)

	// 分支选择。
	var curWeight uint64
	if cur, ok := c.nodes[string(c.head)]; ok {
		curWeight = cur.weight
	}
	// 成为新头的条件：当前无规范头（创世延伸），或累计权重严格更大。
	// 平手时保留现有头，保证选择确定、无抖动。
	becomesHead := len(c.head) == 0 || weight > curWeight
	if !becomesHead {
		// 更轻或平手：只保存，不切换规范链。
		return &ReorgReport{
			Triggered:      false,
			OldHead:        c.head,
			NewHead:        c.head,
			OldChainWeight: curWeight,
			NewChainWeight: weight,
		}, nil
	}

	// 需要重组：求最近共同祖先。
	oldChain := c.chainFromHead(c.head)
	newChain := c.chainFromHead(h)
	ancestor := c.commonAncestor(oldChain, newChain)

	report := &ReorgReport{
		Triggered:      !ancestor.Equal(h) && len(c.head) != 0,
		OldHead:        append(Hash(nil), c.head...),
		NewHead:        append(Hash(nil), h...),
		CommonAncestor: append(Hash(nil), ancestor...),
		OldChainWeight: curWeight,
		NewChainWeight: weight,
	}

	// 1) 回滚：旧链从头向下到分叉点（不含分叉点），逐块逆序恢复。
	rollbackTo := c.stateAt(ancestor)
	rollState := rollbackTo.Copy()
	for i := len(oldChain) - 1; i >= 0; i-- {
		bh := oldChain[i]
		if bh.Equal(ancestor) {
			break
		}
		rollState.RevertBlock(bh)
		report.RolledBack = append(report.RolledBack, append(Hash(nil), bh...))
	}

	// 2) 重放：新链从分叉点之后到新头，逐块执行。
	for _, bh := range newChain {
		if len(bh) == 0 || bh.Equal(ancestor) {
			continue
		}
		nn := c.nodes[string(bh)]
		rollState.ExecuteBlock(bh, nn.block.Txs)
		report.Replayed = append(report.Replayed, append(Hash(nil), bh...))
		for _, r := range nn.receipts {
			if r.Status != TxSuccess {
				report.RejectedOnReorg = append(report.RejectedOnReorg, r)
			}
		}
	}

	// 3) 一致性自检：基于效果日志回滚+重放得到的状态指纹，
	//    必须与分支导入时在父状态副本上直接执行得到的指纹一致。
	if rs := rollState.Root(); !rs.Equal(node.state.Root()) {
		// 状态机不变量被破坏：宁可显式失败，也不静默错乱。
		panic("ledger: reorg state root mismatch — rollback/replay not convergent")
	}

	// 4) 切换规范链。
	c.head = h
	c.headHeight = b.Height
	c.canonical = map[uint64]Hash{0: ZeroHash}
	for _, bh := range c.chainFromHead(h) {
		if len(bh) == 0 {
			continue
		}
		c.canonical[c.nodes[string(bh)].block.Height] = bh
	}
	return report, nil
}

// chainFromHead 返回从给定头块向上到创世（含创世空哈希）的哈希序列，顺序为创世→头。
func (c *Chain) chainFromHead(head Hash) []Hash {
	var rev []Hash
	cur := head
	for {
		rev = append(rev, cur)
		if len(cur) == 0 {
			break
		}
		n, ok := c.nodes[string(cur)]
		if !ok {
			break
		}
		cur = n.parent
	}
	out := make([]Hash, len(rev))
	for i, h := range rev {
		out[len(rev)-1-i] = h
	}
	return out
}

// commonAncestor 返回两条从创世到各自头部的哈希链的最近共同祖先。
// 两条链共享创世（空哈希），必然存在分叉点。
func (c *Chain) commonAncestor(a, b []Hash) Hash {
	set := map[string]bool{}
	for _, h := range a {
		set[string(h)] = true
	}
	for i := len(b) - 1; i >= 0; i-- {
		if set[string(b[i])] {
			return b[i]
		}
	}
	return ZeroHash
}

// stateAt 返回某区块对应的结果状态（创世返回创世状态）。
func (c *Chain) stateAt(h Hash) *StateDB {
	if len(h) == 0 {
		return c.genesis
	}
	return c.nodes[string(h)].state
}

// CanonicalHashes 返回规范链从创世之后到头部的区块哈希（高度升序）。
func (c *Chain) CanonicalHashes() []Hash {
	var out []Hash
	for h := uint64(1); h <= c.headHeight; h++ {
		if bh, ok := c.canonical[h]; ok {
			out = append(out, bh)
		}
	}
	return out
}

// Receipt 在规范链上查询某笔交易的最新收据（从头部向下，先找到的为准）。
func (c *Chain) Receipt(txHash Hash) *Receipt {
	for h := c.headHeight; h >= 1; h-- {
		bh, ok := c.canonical[h]
		if !ok {
			continue
		}
		for _, r := range c.nodes[string(bh)].receipts {
			if r.TxHash.Equal(txHash) {
				return r
			}
		}
	}
	return nil
}
