package ledger

import (
	"errors"
	"testing"
)

// buildBlock 在指定父块上构造一个合法区块（交易按规范顺序执行）。
func buildBlock(t *testing.T, c *Chain, parent *Block, coinbase Address, weight uint64, txs ...*Transaction) *Block {
	t.Helper()
	c.mu.RLock()
	st := c.snaps[parent.Hash].Snapshot()
	c.mu.RUnlock()
	exec := canonicalOrder(txs)
	for _, tx := range exec {
		r := st.Apply(tx, coinbase)
		if !r.Success {
			t.Fatalf("buildBlock: tx %s invalid: %v", tx.TxHash()[:12], r.Reason)
		}
	}
	b := &Block{
		Height:       parent.Height + 1,
		ParentHash:   parent.Hash,
		Coinbase:     coinbase,
		Weight:       weight,
		Transactions: exec,
	}
	b.Hash = b.BlockHash()
	b.StateRoot = st.RootHash()
	return b
}

func mustAccept(t *testing.T, c *Chain, b *Block) *ReorgResult {
	t.Helper()
	res, err := c.AcceptBlock(b)
	if err != nil {
		t.Fatalf("AcceptBlock(%s) failed: %v", b.Hash[:12], err)
	}
	return res
}

// 核心重组场景：旧链两笔转账已确认，更高权重分叉出现后回滚并重放，
// 重组前后余额与收据必须自洽，不允许重复扣款或静默丢交易。
func TestReorgRollbackAndReplay(t *testing.T) {
	logger := newLogger(t)
	alice := GenerateAccount()
	bob := GenerateAccount()
	carol := GenerateAccount()
	miner := GenerateAccount()

	chain := NewChain(map[Address]uint64{alice.Addr: 1000}, logger)
	genesis := chain.Tip()

	// --- 旧链 A：alice->bob 两笔 (nonce 0,1) ---
	a1 := buildBlock(t, chain, genesis, miner.Addr, 1, NewTx(alice, bob.Addr, 0, 100, 10))
	mustAccept(t, chain, a1)
	a2 := buildBlock(t, chain, a1, miner.Addr, 1, NewTx(alice, bob.Addr, 1, 50, 10))
	mustAccept(t, chain, a2)

	// 重组前状态：alice=1000-110-60=830, bob=150, miner=20
	if got := chain.Balance(alice.Addr); got != 830 {
		t.Fatalf("pre-reorg alice=%d want 830", got)
	}
	if got := chain.Balance(bob.Addr); got != 150 {
		t.Fatalf("pre-reorg bob=%d want 150", got)
	}
	t.Logf("pre-reorg: alice=%d bob=%d miner=%d tip=%s",
		chain.Balance(alice.Addr), chain.Balance(bob.Addr), chain.Balance(miner.Addr), chain.Tip().Hash[:12])

	// --- 分叉 B：从创世出发，alice->carol 三笔，累计权重到第三块才超过旧链 ---
	b1 := buildBlock(t, chain, genesis, miner.Addr, 1, NewTx(alice, carol.Addr, 0, 200, 10))
	if res := mustAccept(t, chain, b1); res.Forked {
		t.Fatal("b1 must not reorg: branch weight 1 <= 2")
	}
	b2 := buildBlock(t, chain, b1, miner.Addr, 1, NewTx(alice, carol.Addr, 1, 100, 10))
	if res := mustAccept(t, chain, b2); res.Forked {
		t.Fatal("b2 must not reorg: branch weight 2 == 2, tie keeps canonical")
	}
	b3 := buildBlock(t, chain, b2, miner.Addr, 1, NewTx(alice, carol.Addr, 2, 50, 10))
	res := mustAccept(t, chain, b3)
	if !res.Forked {
		t.Fatal("expected reorg when accepting b3 (3 > 2)")
	}

	// 重组范围检查
	if res.Ancestor.Hash != GenesisHash {
		t.Fatalf("ancestor=%s want genesis", res.Ancestor.Hash[:12])
	}
	if len(res.RolledBack) != 2 || len(res.Reapplied) != 3 {
		t.Fatalf("rollback=%d reapply=%d want 2/3", len(res.RolledBack), len(res.Reapplied))
	}
	t.Logf("reorg: rolled_back=%d blocks, reapplied=%d blocks", len(res.RolledBack), len(res.Reapplied))

	// 重组后状态：alice=1000-210-110-60=620, carol=350, bob=0, miner=30
	if got := chain.Balance(alice.Addr); got != 620 {
		t.Fatalf("post-reorg alice=%d want 620 (double-charge or missed refund?)", got)
	}
	if got := chain.Balance(bob.Addr); got != 0 {
		t.Fatalf("post-reorg bob=%d want 0 (rolled-back tx not refunded)", got)
	}
	if got := chain.Balance(carol.Addr); got != 350 {
		t.Fatalf("post-reorg carol=%d want 350", got)
	}
	if got := chain.Balance(miner.Addr); got != 30 {
		t.Fatalf("post-reorg miner=%d want 30", got)
	}
	if got := chain.State().Nonce(alice.Addr); got != 3 {
		t.Fatalf("post-reorg alice nonce=%d want 3", got)
	}
	t.Logf("post-reorg: alice=%d bob=%d carol=%d miner=%d tip=%s",
		chain.Balance(alice.Addr), chain.Balance(bob.Addr), chain.Balance(carol.Addr),
		chain.Balance(miner.Addr), chain.Tip().Hash[:12])

	// 收据自洽：旧链交易在规范链上查不到，新链交易查得到。
	if _, _, ok := chain.TxReceipt(a1.Transactions[0].TxHash()); ok {
		t.Fatal("rolled-back tx still has receipt on canonical chain")
	}
	if _, r, ok := chain.TxReceipt(b3.Transactions[0].TxHash()); !ok || !r.Success {
		t.Fatal("reapplied tx missing receipt on canonical chain")
	}
	// 但旧区块的收据仍可按区块哈希查询（历史保留，可解释）。
	if rs, ok := chain.ReceiptsOf(a1.Hash); !ok || len(rs) != 1 {
		t.Fatal("rolled-back block receipts should remain queryable by block hash")
	}
}

// 两条分支都包含同一笔交易时，重组后该交易只生效一次（不重复扣款）。
func TestReorgSharedTxAppliedOnce(t *testing.T) {
	logger := newLogger(t)
	alice := GenerateAccount()
	bob := GenerateAccount()
	miner := GenerateAccount()

	chain := NewChain(map[Address]uint64{alice.Addr: 500}, logger)
	genesis := chain.Tip()
	shared := NewTx(alice, bob.Addr, 0, 100, 5)

	a1 := buildBlock(t, chain, genesis, miner.Addr, 1, shared)
	mustAccept(t, chain, a1)

	// 分叉 B 也包含 shared，区块靠不同权重区分，再加一笔，累计权重更高。
	b1 := buildBlock(t, chain, genesis, miner.Addr, 2, shared)
	mustAccept(t, chain, b1)
	b2 := buildBlock(t, chain, b1, miner.Addr, 3, NewTx(alice, bob.Addr, 1, 10, 5))
	res := mustAccept(t, chain, b2)
	if !res.Forked {
		t.Fatal("expected reorg")
	}
	if got := chain.Balance(alice.Addr); got != 500-105-15 {
		t.Fatalf("alice=%d want %d (shared tx applied twice?)", got, 500-105-15)
	}
	if got := chain.Balance(bob.Addr); got != 110 {
		t.Fatalf("bob=%d want 110", got)
	}
	t.Logf("shared tx applied exactly once: alice=%d bob=%d", chain.Balance(alice.Addr), chain.Balance(bob.Addr))
}

// 累计权重相等时不重组（确定性规则）。
func TestNoReorgOnEqualWeight(t *testing.T) {
	logger := newLogger(t)
	alice := GenerateAccount()
	bob := GenerateAccount()
	miner := GenerateAccount()

	chain := NewChain(map[Address]uint64{alice.Addr: 500}, logger)
	genesis := chain.Tip()

	a1 := buildBlock(t, chain, genesis, miner.Addr, 5, NewTx(alice, bob.Addr, 0, 10, 1))
	mustAccept(t, chain, a1)

	b1 := buildBlock(t, chain, genesis, miner.Addr, 5, NewTx(alice, bob.Addr, 0, 20, 1))
	res := mustAccept(t, chain, b1)
	if res.Forked {
		t.Fatal("equal weight must not trigger reorg")
	}
	if chain.Tip().Hash != a1.Hash {
		t.Fatal("tip should stay on first-seen heavier-or-equal chain")
	}
	t.Logf("equal weight: tip stays at %s (deterministic first-seen rule)", chain.Tip().Hash[:12])
}

// 非法区块必须被拒绝：孤儿块、状态根错误、高度错误、交易顺序不规范。
func TestRejectInvalidBlocks(t *testing.T) {
	logger := newLogger(t)
	alice := GenerateAccount()
	bob := GenerateAccount()
	miner := GenerateAccount()

	chain := NewChain(map[Address]uint64{alice.Addr: 500}, logger)
	genesis := chain.Tip()

	// 孤儿块
	orphan := &Block{Height: 1, ParentHash: "deadbeef", Coinbase: miner.Addr, Weight: 1}
	orphan.Hash = orphan.BlockHash()
	if _, err := chain.AcceptBlock(orphan); !errors.Is(err, ErrOrphan) {
		t.Fatalf("expected ErrOrphan, got %v", err)
	}
	t.Logf("reject orphan block: %v", ErrOrphan)

	// 状态根错误
	bad := buildBlock(t, chain, genesis, miner.Addr, 1, NewTx(alice, bob.Addr, 0, 10, 1))
	bad.StateRoot = "bogus"
	bad.Hash = bad.BlockHash() // hash 不含 stateRoot，仍然合法
	if _, err := chain.AcceptBlock(bad); !errors.Is(err, ErrStateRootMismatch) {
		t.Fatalf("expected ErrStateRootMismatch, got %v", err)
	}
	t.Logf("reject bad state root: %v", ErrStateRootMismatch)

	// 高度错误
	wrong := buildBlock(t, chain, genesis, miner.Addr, 1)
	wrong.Height = 7
	wrong.Hash = wrong.BlockHash()
	if _, err := chain.AcceptBlock(wrong); !errors.Is(err, ErrInvalidBlock) {
		t.Fatalf("expected ErrInvalidBlock, got %v", err)
	}
	t.Logf("reject wrong height: %v", ErrInvalidBlock)

	// 交易顺序不规范（两笔同账户交易 nonce 逆序）
	t0 := NewTx(alice, bob.Addr, 0, 10, 1)
	t1 := NewTx(alice, bob.Addr, 1, 10, 1)
	blk := &Block{Height: 1, ParentHash: genesis.Hash, Coinbase: miner.Addr, Weight: 1,
		Transactions: []*Transaction{t1, t0}} // 逆序
	blk.Hash = blk.BlockHash()
	if _, err := chain.AcceptBlock(blk); !errors.Is(err, ErrInvalidBlock) {
		t.Fatalf("expected ErrInvalidBlock for non-canonical order, got %v", err)
	}
	t.Logf("reject non-canonical tx order: %v", ErrInvalidBlock)
}
