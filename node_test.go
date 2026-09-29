package ledger

import (
	"sync"
	"testing"
)

// 端到端：并发提交 -> 确定性出块 -> 接收更高权重分叉触发重组 ->
// 被回滚的交易重新入池并再次打包；最终账本与收据自洽。
func TestNodeProduceAndReorgIntegration(t *testing.T) {
	logger := newLogger(t)
	alice := GenerateAccount()
	bob := GenerateAccount()
	carol := GenerateAccount()

	chain := NewChain(map[Address]uint64{alice.Addr: 10_000}, logger)
	node := NewNode(chain, carol.Addr, 1000, 1000, logger)

	// 并发提交 alice 的 6 笔连续 nonce 交易（不同金额）。
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	var txs [n]*Transaction
	for i := 0; i < n; i++ {
		txs[i] = NewTx(alice, bob.Addr, uint64(i), uint64(10*(i+1)), 1)
	}
	for _, tx := range txs {
		wg.Add(1)
		go func(tx *Transaction) { defer wg.Done(); errs <- node.Submit(tx) }(tx)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	// 出两个块，把 6 笔全部打包（每块最多 3 笔，Select 顺序确定）。
	blkA1, err := node.ProduceBlock(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	blkA2, err := node.ProduceBlock(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(blkA1.Transactions) != 3 || len(blkA2.Transactions) != 3 {
		t.Fatalf("packed %d/%d want 3/3", len(blkA1.Transactions), len(blkA2.Transactions))
	}
	if node.Pool().PendingSize() != 0 {
		t.Fatalf("pool should drain after packing, ready=%d", node.Pool().PendingSize())
	}

	// 手工构造一条从创世分叉的更高权重链：只包含 nonce 0,1 两笔（其余交易将被回滚回收）。
	genesis := chain.blocks[GenesisHash]
	keep0 := txs[0]
	keep1 := txs[1]
	fb1 := buildBlock(t, chain, genesis, carol.Addr, 2, keep0)
	mustAccept(t, chain, fb1) // 权重 2 == 旧链 2，不重组
	fb2 := buildBlock(t, chain, fb1, carol.Addr, 2, keep1)
	res := mustAccept(t, chain, fb2) // 4 > 2，重组
	if !res.Forked {
		t.Fatal("expected reorg")
	}
	t.Logf("integration reorg: rolled=%d reapply=%d", len(res.RolledBack), len(res.Reapplied))

	// 通知交易池：新链已含 nonce0/1，旧块中的 nonce2..5 应重新入池。
	node.Pool().OnReorg(res)
	if got := node.Pool().PendingSize(); got != n-2 {
		t.Fatalf("requeued ready=%d want %d", got, n-2)
	}
	t.Logf("reorg pool recovery: %d rolled-back txs re-queued", node.Pool().PendingSize())

	// 再出一块把回收的 4 笔打包到新规范链上。
	blkB3, err := node.ProduceBlock(1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(blkB3.Transactions) != n-2 {
		t.Fatalf("re-packed=%d want %d", len(blkB3.Transactions), n-2)
	}

	// 最终账本：alice 转出 sum(amount)+6 笔手续费，bob 收到 sum(amount)，carol 得 6 手续费。
	var amountSum uint64
	for i := 0; i < n; i++ {
		amountSum += uint64(10 * (i + 1))
	}
	if got := chain.Balance(alice.Addr); got != 10_000-amountSum-n {
		t.Fatalf("alice=%d want %d", got, 10_000-amountSum-n)
	}
	if got := chain.Balance(bob.Addr); got != amountSum {
		t.Fatalf("bob=%d want %d", got, amountSum)
	}
	if got := chain.Balance(carol.Addr); got != n {
		t.Fatalf("carol fees=%d want %d", got, n)
	}
	if got := chain.State().Nonce(alice.Addr); got != n {
		t.Fatalf("alice nonce=%d want %d", got, n)
	}
	t.Logf("final ledger after reorg+repack: alice=%d bob=%d carol(fee)=%d nonce=%d",
		chain.Balance(alice.Addr), chain.Balance(bob.Addr), chain.Balance(carol.Addr), n)

	// 每笔原始交易在最终规范链上恰好有一张成功收据（无重复扣款/静默丢弃）。
	for i, tx := range txs {
		_, r, ok := chain.TxReceipt(tx.TxHash())
		if !ok || !r.Success {
			t.Fatalf("tx nonce=%d missing successful receipt after reorg", i)
		}
	}
	t.Log("all 6 txs have exactly one successful receipt on final canonical chain")
}
