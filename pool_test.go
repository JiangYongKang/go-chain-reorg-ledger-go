package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 交易池的各类拒绝原因必须可区分；容量满与 future 满要有确定策略。
func TestPoolRejectReasons(t *testing.T) {
	logger := newLogger(t)
	alice := GenerateAccount()
	bob := GenerateAccount()
	chain := NewChain(map[Address]uint64{alice.Addr: 1000}, logger)

	// ready 容量 2，future 容量 2。
	pool := NewPool(chain, 2, 2, logger)

	// 签名无效
	bad := NewTx(alice, bob.Addr, 0, 10, 1)
	bad.Amount = 11 // 篡改金额破坏签名
	if err := pool.Add(bad); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("want ErrInvalidSig got %v", err)
	}

	// nonce 0,1 进入 ready（容量 2）
	if err := pool.Add(NewTx(alice, bob.Addr, 0, 10, 1)); err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(NewTx(alice, bob.Addr, 1, 10, 1)); err != nil {
		t.Fatal(err)
	}

	// 同哈希重复提交
	same := NewTx(alice, bob.Addr, 0, 10, 1)
	if err := pool.Add(same); !errors.Is(err, ErrDuplicateTx) {
		t.Fatalf("want ErrDuplicateTx got %v", err)
	}

	// 同发送方同 nonce 但内容不同 = 冲突交易，拒绝
	conflict := NewTx(alice, GenerateAccount().Addr, 0, 20, 2)
	if err := pool.Add(conflict); !errors.Is(err, ErrDuplicateTx) {
		t.Fatalf("conflict tx: want ErrDuplicateTx got %v", err)
	}
	t.Log("same-sender same-nonce conflict rejected deterministically")

	// ready 已满：nonce 连续的下一笔（nonce 2）应被 ErrPoolFull 拒绝（不静默丢弃）
	if err := pool.Add(NewTx(alice, bob.Addr, 2, 10, 1)); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("want ErrPoolFull got %v", err)
	}
	t.Logf("ready full -> ErrPoolFull (ready=%d cap=2)", pool.PendingSize())

	// 另一个账户：nonce 0 时链上 nonce=0，本应进 ready，但 ready 满 -> ErrPoolFull
	carol := GenerateAccount()
	if err := pool.Add(NewTx(carol, bob.Addr, 0, 1, 0)); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("carol ready want ErrPoolFull got %v", err)
	}

	// 编号空洞：carol nonce 5 进入 future（future 容量 2）
	if err := pool.Add(NewTx(carol, bob.Addr, 5, 1, 0)); err != nil {
		t.Fatalf("future add: %v", err)
	}
	if pool.FutureSize() != 1 {
		t.Fatalf("future size=%d want 1", pool.FutureSize())
	}
	if err := pool.Add(NewTx(carol, bob.Addr, 6, 1, 0)); err != nil {
		t.Fatalf("future add: %v", err)
	}
	if err := pool.Add(NewTx(carol, bob.Addr, 7, 1, 0)); !errors.Is(err, ErrFutureFull) {
		t.Fatalf("want ErrFutureFull got %v", err)
	}
	t.Logf("nonce gap txs queued in future; future full -> ErrFutureFull (future=%d)", pool.FutureSize())

	// 空洞交易绝不进入 ready/打包集合
	if got := pool.Select(100); len(got) != 2 {
		t.Fatalf("Select must not include future txs, got %d", len(got))
	}
}

// Select 的结果只取决于集合本身，与提交顺序无关；冲突交易不会同时被打包。
func TestPoolSelectDeterministic(t *testing.T) {
	logger := newLogger(t)
	senders := make([]*Account, 4)
	for i := range senders {
		senders[i] = GenerateAccount()
	}
	bob := GenerateAccount()
	alloc := map[Address]uint64{}
	for _, a := range senders {
		alloc[a.Addr] = 1000
	}

	canonical := make([]string, 0)
	for trial := 0; trial < 4; trial++ {
		chain := NewChain(alloc, logger)
		pool := NewPool(chain, 100, 100, logger)

		var txs []*Transaction
		for _, a := range senders {
			for n := uint64(0); n < 3; n++ {
				txs = append(txs, NewTx(a, bob.Addr, n, 5, 1))
			}
		}
		// 每轮用不同顺序提交。
		perm := newRand(int64(trial)).Perm(len(txs))
		for _, i := range perm {
			if err := pool.Add(txs[i]); err != nil {
				t.Fatal(err)
			}
		}
		got := pool.Select(100)
		key := ""
		seen := map[string]bool{}
		for _, tx := range got {
			k := fmt.Sprintf("%x:%d", tx.From[:4], tx.Nonce)
			if seen[k] {
				t.Fatalf("conflicting txs both selected: %s", k)
			}
			seen[k] = true
			key += k + "|"
		}
		if len(got) != len(txs) {
			t.Fatalf("trial %d selected %d want %d", trial, len(got), len(txs))
		}
		if trial == 0 {
			canonical = append(canonical, key)
		} else if key != canonical[0] {
			t.Fatalf("trial %d selection order differs from trial 0", trial)
		}
		t.Logf("trial=%d selected=%d deterministic-order=%q", trial, len(got), key[:40])
	}
}

// 高并发提交：只有一笔同 (发送方,nonce) 冲突交易能入池；
// 选择集合唯一、无数据竞争（配合 `go test -race`）。
func TestPoolConcurrent(t *testing.T) {
	logger := newLogger(t)
	alice := GenerateAccount()
	bob := GenerateAccount()
	carol := GenerateAccount()
	chain := NewChain(map[Address]uint64{alice.Addr: 1_000_000, bob.Addr: 1_000_000}, logger)
	pool := NewPool(chain, 1000, 1000, logger)

	const writers = 32
	var wg sync.WaitGroup
	var okN, dupN int64
	var statMu sync.Mutex

	// 每个 writer 都提交 alice nonce 0（互相冲突）以及自己的独立交易（bob nonce i）。
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raceTx := NewTx(alice, carol.Addr, 0, uint64(1+i), 1) // 同 nonce 不同内容
			err := pool.Add(raceTx)
			statMu.Lock()
			if err == nil {
				okN++
			} else if errors.Is(err, ErrDuplicateTx) {
				dupN++
			}
			statMu.Unlock()

			// 独立 nonce 的合法交易，全部应被接受。
			indep := NewTx(bob, carol.Addr, uint64(i), 1, 0)
			if err := pool.Add(indep); err != nil {
				t.Errorf("independent tx rejected: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if okN != 1 || dupN != writers-1 {
		t.Fatalf("conflict outcome: accepted=%d duplicate=%d want 1/%d", okN, dupN, writers-1)
	}
	t.Logf("concurrent conflict: exactly 1 accepted, %d rejected as duplicate", dupN)

	got := pool.Select(1000)
	seen := map[string]bool{}
	for _, tx := range got {
		k := fmt.Sprintf("%x:%d", tx.From[:8], tx.Nonce)
		if seen[k] {
			t.Fatalf("conflicting tx both selected: %s", k)
		}
		seen[k] = true
	}
	if len(got) != 1+writers { // 1 个 alice 冲突赢家 + writers 个独立交易
		t.Fatalf("selected=%d want %d", len(got), 1+writers)
	}
	t.Logf("concurrent submit: selected unique set size=%d (no double-pack)", len(got))
}

// 空洞补齐：future 中的交易在前序 nonce 被补齐后自动晋升并可打包。
func TestPoolNonceGapPromotion(t *testing.T) {
	logger := newLogger(t)
	alice := GenerateAccount()
	bob := GenerateAccount()
	chain := NewChain(map[Address]uint64{alice.Addr: 1000}, logger)
	pool := NewPool(chain, 100, 100, logger)

	if err := pool.Add(NewTx(alice, bob.Addr, 2, 10, 1)); err != nil {
		t.Fatal(err)
	}
	if pool.PendingSize() != 0 || pool.FutureSize() != 1 {
		t.Fatalf("nonce-2 tx must wait in future: ready=%d future=%d", pool.PendingSize(), pool.FutureSize())
	}
	// 补齐 nonce 0，应级联晋升：0 进 ready 后 2 仍因缺 1 留在 future。
	if err := pool.Add(NewTx(alice, bob.Addr, 0, 10, 1)); err != nil {
		t.Fatal(err)
	}
	if pool.PendingSize() != 1 || pool.FutureSize() != 1 {
		t.Fatalf("after nonce0: ready=%d future=%d", pool.PendingSize(), pool.FutureSize())
	}
	// 补齐 nonce 1，2 应自动晋升。
	if err := pool.Add(NewTx(alice, bob.Addr, 1, 10, 1)); err != nil {
		t.Fatal(err)
	}
	if pool.PendingSize() != 3 || pool.FutureSize() != 0 {
		t.Fatalf("after nonce1 all promote: ready=%d future=%d", pool.PendingSize(), pool.FutureSize())
	}
	t.Log("nonce gap filled: future txs promoted in deterministic cascade")
}
