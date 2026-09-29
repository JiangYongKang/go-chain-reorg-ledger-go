package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// accountState 是单个账户的链上状态。
type accountState struct {
	Nonce   uint64
	Balance uint64
}

// StateDB 是可回滚的账户状态库。
type StateDB struct {
	accounts map[Address]*accountState
}

// NewStateDB 创建空状态库。
func NewStateDB() *StateDB {
	return &StateDB{accounts: make(map[Address]*accountState)}
}

// Snapshot 返回状态库的深拷贝快照。
func (s *StateDB) Snapshot() *StateDB {
	cp := &StateDB{accounts: make(map[Address]*accountState, len(s.accounts))}
	for k, v := range s.accounts {
		c := *v
		cp.accounts[k] = &c
	}
	return cp
}

// Restore 用快照覆盖当前状态（深拷贝，避免别名）。
func (s *StateDB) Restore(snap *StateDB) {
	s.accounts = make(map[Address]*accountState, len(snap.accounts))
	for k, v := range snap.accounts {
		c := *v
		s.accounts[k] = &c
	}
}

// Balance 返回账户余额。
func (s *StateDB) Balance(addr Address) uint64 {
	if a, ok := s.accounts[addr]; ok {
		return a.Balance
	}
	return 0
}

// Nonce 返回账户已用 nonce（账户不存在时为 0，首笔交易 nonce 必须为 0）。
func (s *StateDB) Nonce(addr Address) uint64 {
	if a, ok := s.accounts[addr]; ok {
		return a.Nonce
	}
	return 0
}

// credit 向账户加钱。
func (s *StateDB) credit(addr Address, amount uint64) {
	a := s.accounts[addr]
	if a == nil {
		a = &accountState{}
		s.accounts[addr] = a
	}
	a.Balance += amount
}

// failedReceipt 构造不改变状态的失败收据。
func failedReceipt(t *Transaction, reason error) *Receipt {
	return &Receipt{TxHash: t.TxHash(), Success: false, Reason: reason,
		From: t.From, To: t.To, Nonce: t.Nonce}
}

// Apply 校验并执行单笔交易，返回收据；非法交易原样返回失败收据，不修改状态。
//
// 校验顺序（失败原因可区分）：
//  1. 签名无效 -> ErrInvalidSig
//  2. nonce 不等于账户当前 nonce（重放/乱序）-> ErrInvalidNonce 或 ErrDuplicateTx
//  3. 余额不足（amount+fee）-> ErrInsufficientBal
func (s *StateDB) Apply(t *Transaction, coinbase Address) *Receipt {
	if !VerifyTxSig(t) {
		return failedReceipt(t, ErrInvalidSig)
	}
	from := s.accounts[t.From]
	want := s.Nonce(t.From)
	if t.Nonce < want {
		// nonce 已被使用：重放/重复交易
		return failedReceipt(t, ErrDuplicateTx)
	}
	if t.Nonce > want {
		// 编号乱序 / nonce 空洞
		return failedReceipt(t, ErrInvalidNonce)
	}
	bal := uint64(0)
	if from != nil {
		bal = from.Balance
	}
	cost := t.Amount + t.Fee
	if cost < t.Amount { // 溢出保护
		return failedReceipt(t, ErrInsufficientBal)
	}
	if bal < cost {
		return failedReceipt(t, ErrInsufficientBal)
	}
	// 执行：先扣发送方（amount+fee），再增加接收方与矿工手续费。
	from.Balance -= cost
	from.Nonce++
	s.credit(t.To, t.Amount)
	s.credit(coinbase, t.Fee)
	return &Receipt{
		TxHash:   t.TxHash(),
		Success:  true,
		From:     t.From,
		To:       t.To,
		Nonce:    t.Nonce,
		GasUsed:  t.Fee,
		PostFrom: s.Balance(t.From),
		PostTo:   s.Balance(t.To),
	}
}

// canonicalOrder 返回按确定性规则排序后的交易：
// (From 地址, Nonce, To 地址, Amount, TxHash) 升序。
// 这样同一批交易无论传入/打包顺序如何，状态转移结果完全一致。
func canonicalOrder(txs []*Transaction) []*Transaction {
	out := make([]*Transaction, len(txs))
	copy(out, txs)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if c := bytes.Compare(a.From[:], b.From[:]); c != 0 {
			return c < 0
		}
		if a.Nonce != b.Nonce {
			return a.Nonce < b.Nonce
		}
		if c := bytes.Compare(a.To[:], b.To[:]); c != 0 {
			return c < 0
		}
		if a.Amount != b.Amount {
			return a.Amount < b.Amount
		}
		return a.TxHash() < b.TxHash()
	})
	return out
}

// ApplyBlock 按确定性顺序执行一批交易，返回与排序后顺序对应的逐笔收据。
func (s *StateDB) ApplyBlock(txs []*Transaction, coinbase Address) []*Receipt {
	ordered := canonicalOrder(txs)
	rs := make([]*Receipt, len(ordered))
	for i, t := range ordered {
		rs[i] = s.Apply(t, coinbase)
	}
	return rs
}

// reverse 回滚单笔成功交易的效果：退回收款、退回手续费、回退 nonce、恢复余额。
// 仅用于区块回滚；调用方必须保证传入的是在该状态下成功执行过的交易。
func (s *StateDB) reverse(t *Transaction, coinbase Address) {
	// 扣除矿工曾获得的手续费
	s.accounts[coinbase].Balance -= t.Fee
	// 扣除接收方曾收到的金额（自转时与发送方是同一账户，逐笔做即可保持代数一致）
	s.accounts[t.To].Balance -= t.Amount
	// 恢复发送方：nonce 回退，余额加回 amount+fee
	from := s.accounts[t.From]
	from.Balance += t.Amount + t.Fee
	from.Nonce--

	// 对所有被触及的不同地址做零值清理（注意可能互为同一地址）：
	// 余额与 nonce 均归零即删除账户条目，使状态与交易发生前严格一致，
	// 回滚后的状态根才能逐字节还原祖先快照。
	touched := map[Address]struct{}{coinbase: {}, t.To: {}, t.From: {}}
	for addr := range touched {
		if a, ok := s.accounts[addr]; ok && a.Balance == 0 && a.Nonce == 0 {
			delete(s.accounts, addr)
		}
	}
}

// RootHash 计算状态根：账户按地址排序后，对 (地址||nonce||余额) 序列做 sha256。
func (s *StateDB) RootHash() string {
	addrs := make([]Address, 0, len(s.accounts))
	for a := range s.accounts {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return bytes.Compare(addrs[i][:], addrs[j][:]) < 0
	})
	h := sha256.New()
	var u [8]byte
	for _, a := range addrs {
		st := s.accounts[a]
		_, _ = h.Write(a[:])
		putU64(u[:], st.Nonce)
		_, _ = h.Write(u[:])
		putU64(u[:], st.Balance)
		_, _ = h.Write(u[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func putU64(b []byte, v uint64) {
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
}
