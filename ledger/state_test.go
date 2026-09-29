package ledger

import (
	"testing"
)

// testGenesis 构造常用的三账户创世状态：alice 100，bob 50，carol 0。
func testGenesis() *StateDB {
	return NewGenesisState(
		GenesisAccount{Address: "alice", Balance: 100, Secret: []byte("sk-alice")},
		GenesisAccount{Address: "bob", Balance: 50, Secret: []byte("sk-bob")},
		GenesisAccount{Address: "carol", Balance: 0, Secret: []byte("sk-carol")},
	)
}

// signedTx 用登记密钥签名构造一笔交易。
func signedTx(from Address, to Address, nonce, amount, gas uint64) *Transaction {
	secret, _ := LookupKey(from)
	return SignTx(&Transaction{From: from, To: to, Nonce: nonce, Amount: amount, GasPrice: gas}, secret)
}

// assertReceipt 校验收据状态与原因，并打印关键判定依据。
func assertReceipt(t *testing.T, r *Receipt, want TxStatus, wantReason Reason) {
	t.Helper()
	if r.Status != want || r.Reason != wantReason {
		t.Fatalf("receipt mismatch: got status=%d reason=%q, want status=%d reason=%q; tx=%s",
			r.Status, r.Reason, want, wantReason, r.TxHash.Hex())
	}
	t.Logf("判定: tx=%s %s->%s nonce=%d amount=%d => status=%d reason=%q",
		r.TxHash.Hex(), r.From, r.To, r.Nonce, r.Amount, r.Status, r.Reason)
}

func TestApplyHappyPath(t *testing.T) {
	s := testGenesis()
	r := s.Apply(signedTx("alice", "bob", 0, 30, 1))
	assertReceipt(t, r, TxSuccess, ReasonOK)
	if s.BalanceOf("alice") != 70 || s.BalanceOf("bob") != 80 {
		t.Fatalf("余额错误: alice=%d bob=%d", s.BalanceOf("alice"), s.BalanceOf("bob"))
	}
	if s.NonceOf("alice") != 1 {
		t.Fatalf("nonce 未推进: %d", s.NonceOf("alice"))
	}
	t.Logf("转账成功后余额: alice=%d bob=%d carol=%d, stateRoot=%s",
		s.BalanceOf("alice"), s.BalanceOf("bob"), s.BalanceOf("carol"), s.Root().Hex())
}

func TestRejectReasonsAreDistinguishable(t *testing.T) {
	s := testGenesis()

	// 1) 签名无效：用 bob 的密钥签 alice 的交易
	badSig := SignTx(&Transaction{From: "alice", To: "bob", Nonce: 0, Amount: 1}, []byte("sk-bob"))
	assertReceipt(t, s.Apply(badSig), TxRejected, ReasonInvalidSignature)

	// 2) 正常一笔，建立 nonce=0 已用
	good := signedTx("alice", "bob", 0, 10, 1)
	assertReceipt(t, s.Apply(good), TxSuccess, ReasonOK)

	// 3) 完全相同的交易重复 => duplicate_tx，且不二次扣款
	r := s.Apply(good)
	assertReceipt(t, r, TxRejected, ReasonDuplicate)

	// 4) nonce 乱序（倒退/已占用但交易不同）=> nonce_too_low
	other := signedTx("alice", "carol", 0, 5, 1)
	other.Payload = "different-body"
	other = SignTx(other, []byte("sk-alice"))
	assertReceipt(t, s.Apply(other), TxRejected, ReasonNonceTooLow)

	// 5) nonce 超前（编号空洞）=> nonce_too_high
	assertReceipt(t, s.Apply(signedTx("alice", "bob", 5, 1, 1)), TxRejected, ReasonNonceTooHigh)

	// 6) 余额不足
	assertReceipt(t, s.Apply(signedTx("alice", "bob", 1, 10_000, 1)), TxRejected, ReasonInsufficientFunds)

	// 失败交易均不得改变状态
	if s.BalanceOf("alice") != 90 || s.NonceOf("alice") != 1 {
		t.Fatalf("失败交易污染了状态: alice balance=%d nonce=%d", s.BalanceOf("alice"), s.NonceOf("alice"))
	}
	if s.BalanceOf("carol") != 0 {
		t.Fatalf("失败交易发生了扣款: carol=%d", s.BalanceOf("carol"))
	}
	t.Logf("全部异常交易被拒绝且状态未变: alice=%d/%d carol=%d",
		s.BalanceOf("alice"), s.NonceOf("alice"), s.BalanceOf("carol"))
}

func TestBlockExecutionOrderIndependent(t *testing.T) {
	// 同一批交易（跨多账户、多 nonce、含坏交易）以两种打包顺序执行，
	// 最终账本状态指纹必须完全一致。
	mkBatch := func() []*Transaction {
		return []*Transaction{
			signedTx("alice", "bob", 0, 20, 1),
			signedTx("bob", "alice", 0, 10, 2),
			signedTx("alice", "carol", 1, 5, 1),
			signedTx("bob", "carol", 1, 5, 3),
			// 余额不足的交易（alice 在两笔共扣 25 后只有 75，再付 80 失败）
			signedTx("alice", "bob", 2, 80, 1),
			// 签名伪造
			SignTx(&Transaction{From: "carol", To: "bob", Nonce: 0, Amount: 1}, []byte("wrong")),
		}
	}
	blockA := MakeBlock(ZeroHash, 1, mkBatch())
	blockB := MakeBlock(ZeroHash, 1, reverse(mkBatch()))

	sA := testGenesis()
	rA := sA.ExecuteBlock(blockA.Hash(), blockA.Txs)
	sB := testGenesis()
	rB := sB.ExecuteBlock(blockB.Hash(), blockB.Txs)

	if !sA.Root().Equal(sB.Root()) {
		t.Fatalf("打包顺序不同导致状态不一致: rootA=%s rootB=%s", sA.Root().Hex(), sB.Root().Hex())
	}
	t.Logf("两种打包顺序状态指纹一致: root=%s; alice=%d bob=%d carol=%d",
		sA.Root().Hex(), sA.BalanceOf("alice"), sA.BalanceOf("bob"), sA.BalanceOf("carol"))

	// 同一交易无论位置如何，收据结论必须一致。
	for i := range blockA.Txs {
		ha := blockA.Txs[i].Hash()
		var rb *Receipt
		for _, r := range rB {
			if r.TxHash.Equal(ha) {
				rb = r
				break
			}
		}
		if rb == nil || rA[i].Status != rb.Status || rA[i].Reason != rb.Reason {
			t.Fatalf("收据随打包位置变化: tx=%s", ha.Hex())
		}
	}
	for _, r := range rA {
		t.Logf("批量执行收据: tx=%s status=%d reason=%q", r.TxHash.Hex(), r.Status, r.Reason)
	}
}

func TestNonceGapInBlockIsSkipped(t *testing.T) {
	s := testGenesis()
	// alice 的 nonce=1 先进块但 nonce=0 缺失：占位跳过，不生效。
	txs := []*Transaction{
		signedTx("alice", "bob", 1, 10, 1),
		signedTx("bob", "carol", 0, 5, 1),
	}
	block := MakeBlock(ZeroHash, 1, txs)
	rs := s.ExecuteBlock(block.Hash(), txs)
	assertReceipt(t, rs[0], TxSkipped, ReasonSkippedNonceGap)
	assertReceipt(t, rs[1], TxSuccess, ReasonOK)
	if s.NonceOf("alice") != 0 || s.BalanceOf("alice") != 100 {
		t.Fatalf("空洞交易不应生效: alice nonce=%d balance=%d", s.NonceOf("alice"), s.BalanceOf("alice"))
	}
}

func reverse(in []*Transaction) []*Transaction {
	out := append([]*Transaction(nil), in...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
