package ledger

// Node 把交易池与链组合成统一入口，保证提交、出块、重组三条路径
// 复用同一套校验与执行逻辑，结论一致且可解释。
type Node struct {
	chain *Chain
	pool  *Pool
}

// NewNode 基于创世状态创建节点。
func NewNode(genesis *StateDB, poolCapacity int) *Node {
	ch := NewChain(genesis)
	return &Node{
		chain: ch,
		pool:  NewPool(poolCapacity, ch.State()),
	}
}

// Chain 返回底层链。
func (n *Node) Chain() *Chain { return n.chain }

// Pool 返回底层交易池。
func (n *Node) Pool() *Pool { return n.pool }

// SubmitTx 提交待打包交易（委托给交易池，走与区块执行完全相同的校验入口）。
func (n *Node) SubmitTx(tx *Transaction) SubmitResult {
	return n.pool.Submit(tx)
}

// ProduceBlock 从池中确定性选出交易、构造新区块并导入规范链：
//  1. Pack 选出无冲突、无空洞的可打包集合；
//  2. 以当前头为父、头高度+1 出块并导入；
//  3. 仅当区块被规范链接受（未被重组抢走头部）时，从池中移除该批交易，
//     其中被“占位跳过”的交易保留以便重试；
//  4. 用新的规范链状态刷新交易池基准。
func (n *Node) ProduceBlock(maxTxs int) (*Block, []*Receipt, error) {
	txs := n.pool.Pack(maxTxs)
	head := n.chain.Head()
	var parent Hash
	height := uint64(1)
	if head != nil {
		parent = head.Hash()
		height = head.Height + 1
	}
	block := MakeBlock(parent, height, txs)

	report, err := n.chain.AddBlock(block)
	if err != nil {
		return nil, nil, err
	}
	if !block.Hash().Equal(report.NewHead) {
		// 区块未成为规范头（更轻或同权重竞争失败）：不移动池内交易，
		// 保留下一轮重新打包，结论确定可解释。
		return block, nil, nil
	}

	// 用规范链上该块的收据为准（ExecuteBlock 结论）。
	receipts := n.chain.nodes[string(block.Hash())].receipts
	var keep []*Transaction
	for i, r := range receipts {
		if r.Status == TxSkipped {
			keep = append(keep, txs[i])
		}
	}
	// 非跳过的交易（成功或硬拒绝）从池中移除；跳过后保留。
	removeSet := make([]*Transaction, 0, len(txs))
	keepSet := map[string]bool{}
	for _, tx := range keep {
		keepSet[string(tx.Hash())] = true
	}
	for _, tx := range txs {
		if !keepSet[string(tx.Hash())] {
			removeSet = append(removeSet, tx)
		}
	}
	n.pool.Remove(removeSet)
	n.pool.Reset(n.chain.State())
	return block, receipts, nil
}

// ReceiveBlock 接收对等节点区块。若触发重组，则用新规范链状态重置交易池：
// 基准余额/nonce 全部更新，已在新链确认的残留交易被清除，避免状态错乱。
func (n *Node) ReceiveBlock(b *Block) (*ReorgReport, error) {
	report, err := n.chain.AddBlock(b)
	if err != nil {
		return nil, err
	}
	if report.Triggered {
		n.pool.Reset(n.chain.State())
	}
	return report, nil
}
