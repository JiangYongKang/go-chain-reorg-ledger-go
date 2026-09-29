package ledger

import (
	"fmt"
	"sync"
	"testing"
)

func TestPoolStaticRejections(t *testing.T) {
	p := NewPool(100, testGenesis())

	// 签名无效
	badSig := SignTx(&Transaction{From: "alice", To: "bob", Nonce: 0, Amount: 1}, []byte("nope"))
	if r := p.Submit(badSig); r.Accepted || r.Reason != ReasonInvalidSignature {
		t.Fatalf("签名无效应拒绝: %+v", r)
	} else {
		t.Logf("拒绝原因=%q 收据=%s", r.Reason, receiptBrief(r.Receipt))
	}

	// 余额不足
	if r := p.Submit(signedTx("alice", "bob", 0, 999, 1)); r.Accepted || r.Reason != ReasonInsufficientFunds {
		t.Fatalf("余额不足应拒绝: %+v", r)
	}

	// 合法交易
	if r := p.Submit(signedTx("alice", "bob", 0, 10, 1)); !r.Accepted || r.Reason != ReasonOK {
		t.Fatalf("合法交易应接受: %+v", r)
	}

	// 完全重复提交：幂等成功，原因标记 duplicate_tx，池大小不增长
	before := p.Len()
	if r := p.Submit(signedTx("alice", "bob", 0, 10, 1)); !r.Accepted || r.Reason != ReasonDuplicate {
		t.Fatalf("重复交易应幂等成功: %+v", r)
	}
	if p.Len() != before {
		t.Fatalf("重复交易被重复入池: before=%d after=%d", before, p.Len())
	}
	t.Logf("重复提交判定: reason=%q 池大小保持=%d", ReasonDuplicate, p.Len())
}

func TestPoolFullIsDeterministic(t *testing.T) {
	p := NewPool(2, testGenesis())
	// alice 余额 100，可容纳两笔小额
	if r := p.Submit(signedTx("alice", "bob", 0, 1, 1)); !r.Accepted {
		t.Fatal(r)
	}
	if r := p.Submit(signedTx("alice", "bob", 1, 1, 1)); !r.Accepted {
		t.Fatal(r)
	}
	r := p.Submit(signedTx("alice", "bob", 2, 1, 1))
	if r.Accepted || r.Reason != ReasonPoolFull {
		t.Fatalf("池满必须以 pool_full 拒绝: %+v", r)
	}
	t.Logf("池满判定: reason=%q（容量=2，不淘汰旧交易）", r.Reason)
}

func TestPoolNonceGapPolicy(t *testing.T) {
	p := NewPool(100, testGenesis())

	// nonce=1 先到（编号空洞）：允许排队但不参与打包
	r := p.Submit(signedTx("alice", "bob", 1, 10, 1))
	if !r.Accepted || r.Reason != ReasonNonceTooHigh {
		t.Fatalf("超前 nonce 应排队并标注: %+v", r)
	}
	if packed := p.Pack(0); len(packed) != 0 {
		t.Fatalf("存在编号空洞时不应打包: %d", len(packed))
	}
	t.Logf("编号空洞策略: tx nonce=1 已排队（%q），Pack 输出 0 笔", r.Reason)

	// nonce=0 补齐后，两笔连续 nonce 都可打包，顺序确定
	if r := p.Submit(signedTx("alice", "bob", 0, 5, 1)); !r.Accepted {
		t.Fatal(r)
	}
	packed := p.Pack(0)
	if len(packed) != 2 || packed[0].Nonce != 0 || packed[1].Nonce != 1 {
		t.Fatalf("补齐空洞后应按 nonce 顺序打包两笔: %+v", packed)
	}
	t.Logf("空洞补齐后打包序列: nonce=%d,%d", packed[0].Nonce, packed[1].Nonce)
}

func TestPackOrderingByGasPrice(t *testing.T) {
	p := NewPool(100, testGenesis())
	must := func(r SubmitResult) {
		t.Helper()
		if !r.Accepted {
			t.Fatalf("提交被拒: %q", r.Reason)
		}
	}
	must(p.Submit(signedTx("alice", "bob", 0, 1, 1)))
	must(p.Submit(signedTx("alice", "bob", 1, 1, 1)))
	must(p.Submit(signedTx("bob", "alice", 0, 1, 9))) // 高 gasPrice
	packed := p.Pack(0)
	if packed[0].From != "bob" || packed[0].GasPrice != 9 {
		t.Fatalf("gasPrice 最高者应排首位: %+v", packed[0])
	}
	if packed[1].Nonce != 0 || packed[2].Nonce != 1 {
		t.Fatalf("同账户须保持 nonce 连续: %d,%d", packed[1].Nonce, packed[2].Nonce)
	}
	t.Logf("打包排序: %s(gas=%d) -> alice nonce=%d -> alice nonce=%d",
		packed[0].From, packed[0].GasPrice, packed[1].Nonce, packed[2].Nonce)
}

// TestConcurrentConflict 是核心并发测试：大量 goroutine 用不同交易体竞争
// 同一 (from, nonce)。要求：恰好 1 笔被接受，其余全部以“并发冲突”拒绝，
// 且 -race 下无数据竞争，重复运行结论稳定。
func TestConcurrentConflict(t *testing.T) {
	const n = 200
	for iter := 0; iter < 20; iter++ {
		p := NewPool(n, testGenesis())
		var wg sync.WaitGroup
		results := make([]SubmitResult, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				tx := signedTx("alice", "bob", 0, 1, 1)
				tx.Payload = fmt.Sprintf("racer-%d-%d", iter, i) // 不同交易体竞争同一 nonce
				tx = SignTx(tx, []byte("sk-alice"))
				<-start
				results[i] = p.Submit(tx)
			}(i)
		}
		close(start)
		wg.Wait()

		accepted, conflicts := 0, 0
		for _, r := range results {
			switch {
			case r.Accepted:
				accepted++
			case r.Reason == ReasonConflict:
				conflicts++
			default:
				t.Fatalf("出现非预期结论: %+v", r)
			}
		}
		if accepted != 1 || conflicts != n-1 {
			t.Fatalf("iter=%d 并发冲突结论不确定: accepted=%d conflicts=%d (want 1/%d)",
				iter, accepted, conflicts, n-1)
		}
		if p.Len() != 1 {
			t.Fatalf("冲突交易不得同时入池: len=%d", p.Len())
		}
	}
	t.Logf("并发冲突测试: 200 个竞争者 x20 轮，每轮恰好 1 笔接受、199 笔 conflict，池大小恒为 1")
}

func receiptBrief(r *Receipt) string {
	if r == nil {
		return "<nil receipt>"
	}
	return fmt.Sprintf("tx=%s status=%d reason=%q", r.TxHash.Hex(), r.Status, r.Reason)
}
