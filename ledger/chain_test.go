package ledger

import (
	"errors"
	"testing"
)

func TestForkAndReorg(t *testing.T) {
	genesis := testGenesis()
	ch := NewChain(genesis)

	// 共同基础块 B1：alice -20 -> bob（alice 80 / bob 70，nonce a=1）
	b1 := MakeBlock(ZeroHash, 1, []*Transaction{
		signedTx("alice", "bob", 0, 20, 1),
	})
	if rep, err := ch.AddBlock(b1); err != nil || !b1.Hash().Equal(rep.NewHead) {
		t.Fatalf("B1 导入失败: err=%v", err)
	}
	t.Logf("[分支选择] B1 成为头: %s weight=%d", b1.Hash().Hex(), ch.nodes[string(b1.Hash())].weight)

	// A 分支 B2A：bob -10 -> carol（bob 60，nonce b=1）
	b2a := MakeBlock(b1.Hash(), 2, []*Transaction{
		signedTx("bob", "carol", 0, 10, 1),
	})
	if _, err := ch.AddBlock(b2a); err != nil {
		t.Fatal(err)
	}
	t.Logf("[分支选择] A 延伸 B2A 成为头: %s", b2a.Hash().Hex())

	// A 再出 B3A：alice -10 -> carol（alice 70，A 链累计权重更高）
	b3a := MakeBlock(b2a.Hash(), 3, []*Transaction{
		signedTx("alice", "carol", 1, 10, 1),
	})
	if _, err := ch.AddBlock(b3a); err != nil {
		t.Fatal(err)
	}
	if got := ch.State().BalanceOf("alice"); got != 70 {
		t.Fatalf("A 链状态错误: alice=%d", got)
	}
	t.Logf("[分支选择] B3A 成为头: %s, A 链余额 alice=70 bob=60 carol=20", b3a.Hash().Hex())

	// B 分叉块 B2B（高度2，父 B1）：bob -5 -> carol。
	// 权重 2，低于当前头 B3A（权重 4），只保存不切换。
	b2b := MakeBlock(b1.Hash(), 2, []*Transaction{
		signedTx("bob", "carol", 0, 5, 1),
	})
	rep, err := ch.AddBlock(b2b)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Triggered || ch.Head().Hash().Equal(b2b.Hash()) {
		t.Fatalf("低权重分支不得切换规范链")
	}
	t.Logf("[分支选择] B2B(weight=%d) 低于当前头(weight=%d)，保留为分叉，不重组",
		rep.NewChainWeight, rep.OldChainWeight)

	// B 分支再出 B3B：两笔交易，权重 3，累计 5 > A 的 4 => 触发重组。
	b3b := MakeBlock(b2b.Hash(), 3, []*Transaction{
		signedTx("alice", "bob", 1, 10, 1),
		signedTx("bob", "carol", 1, 20, 1), // bob: 70-5=65(本块首笔后) 再付20 => 45
	})
	rep, err = ch.AddBlock(b3b)
	if err != nil {
		t.Fatalf("重组失败: %v", err)
	}
	if !rep.Triggered {
		t.Fatalf("高权重分支必须触发重组")
	}

	t.Logf("[重组] 旧头=%s 新头=%s 分叉点=%s", rep.OldHead.Hex(), rep.NewHead.Hex(), rep.CommonAncestor.Hex())
	if !rep.CommonAncestor.Equal(b1.Hash()) {
		t.Fatalf("分叉点应为 B1: got %s", rep.CommonAncestor.Hex())
	}
	wantRollback := []Hash{b3a.Hash(), b2a.Hash()}
	if len(rep.RolledBack) != len(wantRollback) ||
		!rep.RolledBack[0].Equal(wantRollback[0]) || !rep.RolledBack[1].Equal(wantRollback[1]) {
		t.Fatalf("[回滚范围] 错误: got %s, want %s,%s",
			hashesHex(rep.RolledBack), wantRollback[0].Hex(), wantRollback[1].Hex())
	}
	t.Logf("[回滚范围] %d 块（自旧头向下）: %s", len(rep.RolledBack), hashesHex(rep.RolledBack))

	wantReplay := []Hash{b2b.Hash(), b3b.Hash()}
	if len(rep.Replayed) != 2 || !rep.Replayed[0].Equal(wantReplay[0]) || !rep.Replayed[1].Equal(wantReplay[1]) {
		t.Fatalf("[重放范围] 错误: %s", hashesHex(rep.Replayed))
	}
	t.Logf("[重放范围] %d 块（分叉点之后）: %s, 新链权重 %d > 旧链权重 %d",
		len(rep.Replayed), hashesHex(rep.Replayed), rep.NewChainWeight, rep.OldChainWeight)

	// 重组后余额必须严格等于“在 B1 分叉点直接执行 B 分支”的结果：
	// alice = 100-20-10 = 70；bob = 50+20-5+10-20 = 55；carol = 5+20 = 25。
	st := ch.State()
	if st.BalanceOf("alice") != 70 || st.BalanceOf("bob") != 55 || st.BalanceOf("carol") != 25 {
		t.Fatalf("[余额自洽] 重组后余额错误: alice=%d bob=%d carol=%d", st.BalanceOf("alice"), st.BalanceOf("bob"), st.BalanceOf("carol"))
	}
	if st.NonceOf("alice") != 2 || st.NonceOf("bob") != 2 {
		t.Fatalf("[nonce 自洽] 重组后 nonce 错误: alice=%d bob=%d", st.NonceOf("alice"), st.NonceOf("bob"))
	}
	t.Logf("[余额自洽] 重组后 alice=%d bob=%d carol=%d nonce(a,b)=(%d,%d) root=%s",
		st.BalanceOf("alice"), st.BalanceOf("bob"), st.BalanceOf("carol"),
		st.NonceOf("alice"), st.NonceOf("bob"), st.Root().Hex())

	// A 链独有交易 b3a 必须从规范链收据中消失（未被静默保留），
	// 且其效果已被回滚（carol 没有那 10）。
	if r := ch.Receipt(b3a.Txs[0].Hash()); r != nil {
		t.Fatalf("已回滚交易不应保留在规范链收据中: %+v", r)
	}
	// B 链交易收据可查且成功。
	if r := ch.Receipt(b3b.Txs[0].Hash()); r == nil || r.Status != TxSuccess {
		t.Fatalf("新链交易收据应可查: %+v", r)
	}
	t.Logf("[收据自洽] A 链独占交易 %s 已随回滚移除；B 链交易收据成功", b3a.Txs[0].Hash().Hex())
}

func TestReorgRejectsInvalidTxDeterministically(t *testing.T) {
	genesis := testGenesis()
	ch := NewChain(genesis)
	b1 := MakeBlock(ZeroHash, 1, []*Transaction{signedTx("alice", "bob", 0, 20, 1)})
	if _, err := ch.AddBlock(b1); err != nil {
		t.Fatal(err)
	}
	// B2A 一笔合法交易，占住头。
	b2a := MakeBlock(b1.Hash(), 2, []*Transaction{signedTx("alice", "bob", 1, 10, 1)})
	if _, err := ch.AddBlock(b2a); err != nil {
		t.Fatal(err)
	}
	// 竞争分支 B2B 含一个签名伪造交易 + 两笔合法交易（权重更高）。
	badSig := SignTx(&Transaction{From: "carol", To: "bob", Nonce: 0, Amount: 1}, []byte("forged"))
	b2b := MakeBlock(b1.Hash(), 2, []*Transaction{
		signedTx("alice", "bob", 1, 5, 1),
		signedTx("bob", "alice", 0, 5, 1),
		badSig,
	})
	rep, err := ch.AddBlock(b2b)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Triggered {
		t.Fatalf("B2B 含 3 笔交易，权重 4 > B2A 权重 2，应触发重组")
	}
	if len(rep.RejectedOnReorg) != 1 || rep.RejectedOnReorg[0].Reason != ReasonInvalidSignature {
		t.Fatalf("重放时伪造签名交易必须被拒绝并上报: %+v", rep.RejectedOnReorg)
	}
	t.Logf("[重放拒绝] %d 笔交易未生效: %q（不扣款、不增 nonce）",
		len(rep.RejectedOnReorg), rep.RejectedOnReorg[0].Reason)
	// carol 从未有过有效交易，状态保持创世。
	if ch.State().BalanceOf("carol") != 0 || ch.State().NonceOf("carol") != 0 {
		t.Fatalf("无效交易不得生效: carol=%d/%d", ch.State().BalanceOf("carol"), ch.State().NonceOf("carol"))
	}
}

func TestDuplicateAndUnknownParent(t *testing.T) {
	ch := NewChain(testGenesis())
	b1 := MakeBlock(ZeroHash, 1, []*Transaction{signedTx("alice", "bob", 0, 1, 1)})
	if _, err := ch.AddBlock(b1); err != nil {
		t.Fatal(err)
	}
	_, err := ch.AddBlock(b1)
	if !errors.Is(err, ErrBlockExists) {
		t.Fatalf("重复导入应返回 block_exists: %v", err)
	}
	orphan := MakeBlock(hashBytes([]byte("ghost")), 2, []*Transaction{signedTx("alice", "bob", 1, 1, 1)})
	_, err = ch.AddBlock(orphan)
	if !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("未知父块应返回 unknown_parent: %v", err)
	}
	t.Logf("[导入校验] 重复块=>%q，孤块=>%q，均被显式拒绝", ReasonBlockExists, ReasonUnknownParent)
}

func TestReorgConvergesWithDirectForkExecution(t *testing.T) {
	// 构造“先沿 A 走到很深，再让 B 反超”的场景，
	// 验证回滚+重放的状态指纹与直接在分叉点执行 B 完全一致。
	txs := func(from, to Address, startNonce uint64, n int, amount uint64) []*Transaction {
		var out []*Transaction
		for i := 0; i < n; i++ {
			out = append(out, signedTx(from, to, startNonce+uint64(i), amount, 1))
		}
		return out
	}
	// A 链 3 块各 1 笔（alice 共转出 30）。
	ch := NewChain(testGenesis())
	b1 := MakeBlock(ZeroHash, 1, txs("alice", "bob", 0, 1, 10))
	_, _ = ch.AddBlock(b1)
	b2a := MakeBlock(b1.Hash(), 2, txs("alice", "bob", 1, 1, 10))
	_, _ = ch.AddBlock(b2a)
	b3a := MakeBlock(b2a.Hash(), 3, txs("alice", "bob", 2, 1, 10))
	_, _ = ch.AddBlock(b3a)

	// B 链两块：B2B 一笔（权重 2，累计 4 < A 头 6，不切换），
	// B3B 八笔（权重 9，累计 13 > 6，反超并回滚 B3A/B2A 两块）。
	b2b := MakeBlock(b1.Hash(), 2, txs("bob", "carol", 0, 1, 1))
	rep, err := ch.AddBlock(b2b)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Triggered {
		t.Fatalf("B2B 权重不足，不应重组")
	}
	b3b := MakeBlock(b2b.Hash(), 3, txs("bob", "carol", 1, 8, 1))
	rep, err = ch.AddBlock(b3b)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Triggered {
		t.Fatalf("B 链权重应反超")
	}
	if len(rep.RolledBack) != 2 {
		t.Fatalf("应回滚旧链 B3A,B2A 两块，实际: %s", hashesHex(rep.RolledBack))
	}
	t.Logf("[权重对比] B2B 累计 4 < A 头 6 不切换；B3B 累计 13 > 6 反超")

	// 直接在 B1 结果状态副本上执行 B 两快，比对指纹。
	direct := ch.nodes[string(b1.Hash())].state.Copy()
	direct.ExecuteBlock(b2b.Hash(), b2b.Txs)
	direct.ExecuteBlock(b3b.Hash(), b3b.Txs)
	if !direct.Root().Equal(ch.State().Root()) {
		t.Fatalf("重组结果与分叉点直接执行不一致: direct=%s reorg=%s", direct.Root().Hex(), ch.State().Root())
	}
	t.Logf("[收敛性] 深度回滚 %s 后重放 %s，状态指纹与直接执行一致: %s",
		hashesHex(rep.RolledBack), hashesHex(rep.Replayed), ch.State().Root().Hex())
}

func hashesHex(hs []Hash) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Hex()
	}
	return out
}
