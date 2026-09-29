package ledger

import (
	"testing"
)

// TestCrossEntryConsistency 验证同一交易在三条入口下结论一致：
// 交易池 Submit、单笔 StateDB.Apply、区块 ExecuteBlock（含重放）。
func TestCrossEntryConsistency(t *testing.T) {
	gen := testGenesis()
	node := NewNode(gen, 100)

	cases := []struct {
		name   string
		tx     *Transaction
		reason Reason
	}{
		{"签名无效", SignTx(&Transaction{From: "alice", To: "bob", Nonce: 0, Amount: 1}, []byte("x")), ReasonInvalidSignature},
		{"余额不足", signedTx("alice", "bob", 0, 999, 1), ReasonInsufficientFunds},
		{"正常交易", signedTx("bob", "carol", 0, 7, 5), ReasonOK},
	}
	for _, c := range cases {
		// 入口 1：交易池
		res := node.SubmitTx(c.tx)
		if c.reason == ReasonOK {
			if !res.Accepted {
				t.Fatalf("%s: 交易池应接受，实际 %q", c.name, res.Reason)
			}
		} else {
			if res.Accepted || res.Reason != c.reason {
				t.Fatalf("%s: 交易池结论错误: %+v", c.name, res)
			}
		}
		// 入口 2：独立状态的单笔 Apply
		applyR := testGenesis().Apply(c.tx)
		// 入口 3：独立状态的区块执行
		blk := MakeBlock(ZeroHash, 1, []*Transaction{c.tx})
		blockR := testGenesis().ExecuteBlock(blk.Hash(), blk.Txs)[0]

		if applyR.Reason != blockR.Reason || (c.reason != ReasonOK && applyR.Reason != c.reason) {
			t.Fatalf("%s: 入口结论不一致 pool=%q apply=%q block=%q",
				c.name, res.Reason, applyR.Reason, blockR.Reason)
		}
		t.Logf("[三入口一致] %-8s tx=%s => pool=%q apply=%q block=%q",
			c.name, c.tx.Hash().Hex(), res.Reason, applyR.Reason, blockR.Reason)
	}
}

// TestProduceBlockAndPoolCleanup 验证出块、池清理与跳过交易保留。
func TestProduceBlockAndPoolCleanup(t *testing.T) {
	node := NewNode(testGenesis(), 100)
	// 合法两笔（连续 nonce）+ 一笔编号空洞（nonce=5）+ 一笔余额不足（被池拒绝）
	node.SubmitTx(signedTx("alice", "bob", 0, 10, 1))
	node.SubmitTx(signedTx("alice", "bob", 1, 20, 2))
	gap := node.SubmitTx(signedTx("alice", "bob", 5, 1, 1))
	poor := node.SubmitTx(signedTx("alice", "bob", 2, 10_000, 1))
	if !gap.Accepted || gap.Reason != ReasonNonceTooHigh {
		t.Fatalf("空洞交易应排队: %+v", gap)
	}
	if poor.Accepted || poor.Reason != ReasonInsufficientFunds {
		t.Fatalf("余额不足应拒绝: %+v", poor)
	}

	block, receipts, err := node.ProduceBlock(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 2 {
		t.Fatalf("应只打包 2 笔可执行交易（空洞暂缓）: %d", len(receipts))
	}
	for _, r := range receipts {
		if r.Status != TxSuccess {
			t.Fatalf("打包交易应成功: %+v", r)
		}
	}
	// 空洞交易仍在池中等待补齐
	if node.Pool().Len() != 1 {
		t.Fatalf("空洞交易应保留在池中: len=%d", node.Pool().Len())
	}
	if got := node.Chain().State().BalanceOf("alice"); got != 70 {
		t.Fatalf("出块后余额错误: %d", got)
	}
	t.Logf("[出块] 块=%s 打包 2 笔成功，池保留 1 笔空洞交易，alice 余额 100->70", block.Hash().Hex())
}

// TestReceiveBlockReorgResetsPool 验证对端区块触发重组后，
// 交易池以新链状态为基准：旧链已确认 nonce 的残留被清理，
// 未确认交易仍可在下一块继续打包。
func TestReceiveBlockReorgResetsPool(t *testing.T) {
	node := NewNode(testGenesis(), 100)

	// 本地先在 A 链出块：alice nonce=0 转出 10。
	node.SubmitTx(signedTx("alice", "bob", 0, 10, 1))
	localB1, _, err := node.ProduceBlock(10)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("[本地出块] B1=%s", localB1.Hash().Hex())

	// 池中放入依赖 A 链状态的交易（alice nonce=1）。
	r := node.SubmitTx(signedTx("alice", "carol", 1, 5, 1))
	if !r.Accepted {
		t.Fatalf("nonce=1 在 A 链基准下应可排队: %+v", r)
	}

	// 对端 B 分叉：同高度但 3 笔交易（更高权重），且包含 alice nonce=0 的
	// 另一笔不同交易（冲突体），重组后本地 alice 的 nonce=0 被对端交易占用。
	competing := signedTx("alice", "carol", 0, 30, 1)
	competing.Payload = "competitor"
	competing = SignTx(competing, []byte("sk-alice"))
	remoteB1 := MakeBlock(ZeroHash, 1, []*Transaction{
		competing,
		signedTx("bob", "carol", 0, 5, 1),
		signedTx("bob", "carol", 1, 5, 1),
	})
	rep, err := node.ReceiveBlock(remoteB1)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Triggered {
		t.Fatalf("对端块权重 4 > 本地 2，必须重组")
	}
	t.Logf("[接收重组] 回滚 %s，重放 %s，分叉点为创世",
		hashesHex(rep.RolledBack), hashesHex(rep.Replayed))

	// 重组后状态以对端块为准：alice=70, carol=40, bob=40
	st := node.Chain().State()
	if st.BalanceOf("alice") != 70 || st.BalanceOf("carol") != 40 || st.BalanceOf("bob") != 40 {
		t.Fatalf("重组后余额错误: alice=%d bob=%d carol=%d", st.BalanceOf("alice"), st.BalanceOf("bob"), st.BalanceOf("carol"))
	}
	// 原池中 alice nonce=1 的交易仍可继续打包（对端块未占用 nonce=1），
	// 且现在基于新余额 70，5 元转账合法。
	packed := node.Pool().Pack(10)
	if len(packed) != 1 || packed[0].Nonce != 1 {
		t.Fatalf("重组后 nonce=1 的交易应仍可打包: %+v", packed)
	}
	block, _, err := node.ProduceBlock(10)
	if err != nil {
		t.Fatal(err)
	}
	if st2 := node.Chain().State(); st2.BalanceOf("alice") != 65 {
		t.Fatalf("重组后继续出块状态错误: alice=%d", st2.BalanceOf("alice"))
	}
	t.Logf("[池自洽] 重组后池基准刷新，残留 nonce=1 交易继续打包进 %s，alice=65", block.Hash().Hex())

	// 旧链 alice 交易（本地 B1）的收据已不在规范链。
	if rec := node.Chain().Receipt(localB1.Txs[0].Hash()); rec != nil {
		t.Fatalf("被重组掉的交易收据不得残留: %+v", rec)
	}
	// 对端竞争交易收据可查且成功。
	if rec := node.Chain().Receipt(competing.Hash()); rec == nil || rec.Status != TxSuccess {
		t.Fatalf("竞争获胜交易收据应成功可查: %+v", rec)
	}
	t.Logf("[收据自洽] 旧链交易收据移除，竞争获胜交易 %s 成功", competing.Hash().Hex())
}
