package ledger

import (
	"errors"
	"math/rand"
	"testing"
)

// 多笔交易同一批，无论打包顺序如何，最终状态根必须一致。
func TestStateTransitionDeterminism(t *testing.T) {
	accs := make([]*Account, 4)
	for i := range accs {
		accs[i] = GenerateAccount()
	}
	// 构造一批合法交易：每个账户若干笔 nonce 连续的交易。
	var txs []*Transaction
	for i, a := range accs {
		to := accs[(i+1)%len(accs)].Addr
		for n := uint64(0); n < 3; n++ {
			txs = append(txs, NewTx(a, to, n, 10+uint64(i), 1))
		}
	}

	alloc := map[Address]uint64{}
	for _, a := range accs {
		alloc[a.Addr] = 1000
	}

	var roots []string
	for trial := 0; trial < 5; trial++ {
		st := NewStateDB()
		for addr, bal := range alloc {
			st.credit(addr, bal)
		}
		shuffled := make([]*Transaction, len(txs))
		copy(shuffled, txs)
		rand.New(rand.NewSource(int64(trial))).Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		rs := st.ApplyBlock(shuffled, accs[0].Addr)
		for _, r := range rs {
			if !r.Success {
				t.Fatalf("trial %d: unexpected failed tx %s: %v", trial, r.TxHash[:12], r.Reason)
			}
		}
		roots = append(roots, st.RootHash())
		t.Logf("trial=%d shuffled_order_root=%s", trial, st.RootHash()[:16])
	}
	for i := 1; i < len(roots); i++ {
		if roots[i] != roots[0] {
			t.Fatalf("non-deterministic state: trial0=%s trial%d=%s", roots[0], i, roots[i])
		}
	}
}

// 各类非法交易必须被拒绝且原因可区分。
func TestRejectReasons(t *testing.T) {
	alice := GenerateAccount()
	bob := GenerateAccount()
	coinbase := GenerateAccount()

	st := NewStateDB()
	st.credit(alice.Addr, 100)

	// 1) 签名无效：篡改金额后不重签。
	bad := NewTx(alice, bob.Addr, 0, 10, 1)
	bad.Amount = 999 // 破坏签名
	if r := st.Apply(bad, coinbase.Addr); !errors.Is(r.Reason, ErrInvalidSig) {
		t.Fatalf("expected ErrInvalidSig, got %v", r.Reason)
	} else {
		t.Logf("reject invalid-sig tx: reason=%v", r.Reason)
	}

	// 2) 编号乱序：nonce 空洞。
	gap := NewTx(alice, bob.Addr, 5, 10, 1)
	if r := st.Apply(gap, coinbase.Addr); !errors.Is(r.Reason, ErrInvalidNonce) {
		t.Fatalf("expected ErrInvalidNonce, got %v", r.Reason)
	} else {
		t.Logf("reject nonce-gap tx: reason=%v", r.Reason)
	}

	// 3) 余额不足。
	poor := NewTx(alice, bob.Addr, 0, 200, 1)
	if r := st.Apply(poor, coinbase.Addr); !errors.Is(r.Reason, ErrInsufficientBal) {
		t.Fatalf("expected ErrInsufficientBal, got %v", r.Reason)
	} else {
		t.Logf("reject insufficient-balance tx: reason=%v", r.Reason)
	}

	// 4) 正常交易成功，随后重放同 nonce 交易 -> 重复。
	ok := NewTx(alice, bob.Addr, 0, 10, 1)
	if r := st.Apply(ok, coinbase.Addr); !r.Success {
		t.Fatalf("expected success, got %v", r.Reason)
	}
	dup := NewTx(alice, bob.Addr, 0, 10, 1) // 同 nonce 重放
	if r := st.Apply(dup, coinbase.Addr); !errors.Is(r.Reason, ErrDuplicateTx) {
		t.Fatalf("expected ErrDuplicateTx, got %v", r.Reason)
	} else {
		t.Logf("reject duplicate tx: reason=%v", r.Reason)
	}

	// 状态必须自洽：alice=100-11=89, bob=10, coinbase=1
	if got := st.Balance(alice.Addr); got != 89 {
		t.Fatalf("alice balance=%d want 89", got)
	}
	if got := st.Balance(bob.Addr); got != 10 {
		t.Fatalf("bob balance=%d want 10", got)
	}
	if got := st.Balance(coinbase.Addr); got != 1 {
		t.Fatalf("coinbase balance=%d want 1", got)
	}
	if got := st.Nonce(alice.Addr); got != 1 {
		t.Fatalf("alice nonce=%d want 1", got)
	}
}

// 非法交易不得静默修改状态。
func TestFailedTxDoesNotMutateState(t *testing.T) {
	alice := GenerateAccount()
	bob := GenerateAccount()
	st := NewStateDB()
	st.credit(alice.Addr, 50)
	before := st.RootHash()

	bad := NewTx(alice, bob.Addr, 0, 1000, 0) // 余额不足
	r := st.Apply(bad, bob.Addr)
	if r.Success {
		t.Fatal("expected failure")
	}
	if st.RootHash() != before {
		t.Fatalf("failed tx mutated state: before=%s after=%s", before[:16], st.RootHash()[:16])
	}
	t.Logf("failed tx left state untouched: root=%s", before[:16])
}
