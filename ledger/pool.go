package ledger

import (
	"sort"
	"sync"
)

// SubmitResult 是交易提交结论。
type SubmitResult struct {
	Accepted bool
	TxHash   Hash
	Reason   Reason
	Receipt  *Receipt // 被硬拒绝时给出与执行入口同构的收据
}

// poolKey 唯一标识一笔排队交易（同账户同 nonce 即冲突/重复）。
func poolKey(from Address, nonce uint64) string {
	return string(from) + "#" + uintToKey(nonce)
}

// uintToKey 把 nonce 编成定长字符串，避免 "1#2" 之类的拼接歧义。
func uintToKey(v uint64) string {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	return string(b[:])
}

// Pool 是并发安全的待打包交易池。
type Pool struct {
	mu       sync.Mutex
	capacity int
	// queued 为通过校验（或仅 nonce 超前）的交易，键为账户+nonce。
	queued map[string]*Transaction
	// state 用于静态校验（余额、nonce、签名）；重组后由 Chain 替换。
	state *StateDB
}

// NewPool 创建指定容量的交易池。
func NewPool(capacity int, state *StateDB) *Pool {
	if capacity <= 0 {
		capacity = 1
	}
	return &Pool{
		capacity: capacity,
		queued:   map[string]*Transaction{},
		state:    state,
	}
}

// validateForPool 复用 StateDB.Validate，但区分两类结论：
//   - 硬错误（签名无效、重复、nonce 过低、余额不足）：拒绝入池；
//   - nonce 超前（编号空洞）：允许排队，等待前序交易补齐后再打包。
//
// 即使存在编号空洞，签名与余额等硬错误仍然优先判定，绝不被掩盖。
func validateForPool(s *StateDB, tx *Transaction) (Reason, bool) {
	// 1) 签名：最优先，任何情况下无效即拒绝。
	if !validSig(tx) {
		return ReasonInvalidSignature, false
	}
	// 2) nonce 判定。
	expected := s.NonceOf(tx.From)
	nonceGap := false
	switch {
	case tx.Nonce < expected:
		if existing, ok := s.spent[tx.From][tx.Nonce]; ok && existing.Equal(tx.Hash()) {
			return ReasonDuplicate, false
		}
		return ReasonNonceTooLow, false
	case tx.Nonce > expected:
		nonceGap = true
	}
	// 3) 余额：即使 nonce 超前也要判定当前余额，硬错误优先。
	if s.BalanceOf(tx.From) < tx.Amount {
		return ReasonInsufficientFunds, false
	}
	if nonceGap {
		return ReasonNonceTooHigh, true
	}
	return ReasonOK, true
}

// Submit 并发安全地提交一笔待打包交易。判定顺序：
//  1. 加串行锁，保证同账户同 nonce 的并发提交只有一笔成功，其余得到
//     可区分的“并发冲突/重复”结论，绝不静默丢弃；
//  2. 静态校验（与区块执行同一入口）：签名、nonce、余额；
//  3. 池满则以 pool_full 拒绝（确定性，不淘汰旧交易）；
//  4. nonce 超前允许排队（accepted=true，reason=nonce_too_high 标注空洞）。
func (p *Pool) Submit(tx *Transaction) SubmitResult {
	p.mu.Lock()
	defer p.mu.Unlock()

	h := tx.Hash()
	key := poolKey(tx.From, tx.Nonce)
	if existing, ok := p.queued[key]; ok {
		if existing.Hash().Equal(h) {
			// 同一笔交易重复提交：幂等成功，不重复入池。
			return SubmitResult{Accepted: true, TxHash: h, Reason: ReasonDuplicate}
		}
		// 同账户同 nonce 的不同交易：并发冲突，确定性拒绝后来者。
		return SubmitResult{
			Accepted: false,
			TxHash:   h,
			Reason:   ReasonConflict,
			Receipt:  receiptForError(tx, rejectTx(ReasonConflict)),
		}
	}

	reason, allow := validateForPool(p.state, tx)
	if !allow {
		return SubmitResult{
			Accepted: false,
			TxHash:   h,
			Reason:   reason,
			Receipt:  receiptForError(tx, rejectTx(reason)),
		}
	}
	if len(p.queued) >= p.capacity {
		return SubmitResult{
			Accepted: false,
			TxHash:   h,
			Reason:   ReasonPoolFull,
			Receipt:  receiptForError(tx, rejectTx(ReasonPoolFull)),
		}
	}

	p.queued[key] = tx
	return SubmitResult{Accepted: true, TxHash: h, Reason: reason}
}

// Pack 选出当前可打包交易集合（不修改池状态）：
//  1. 每个账户按地址升序，从状态期望 nonce 起连续选取；遇到编号空洞立即
//     停止，空洞之后的交易本次不打包（确定、可解释）；
//  2. 候选交易跨账户按 gasPrice 降序、再按 (from,nonce) 升序排列；
//  3. 取前 maxTxs 笔（<=0 表示不限）。
//
// 由于提交阶段同账户同 nonce 已互斥，打包集合中不可能出现并发冲突交易。
func (p *Pool) Pack(maxTxs int) []*Transaction {
	p.mu.Lock()
	defer p.mu.Unlock()

	bySender := map[Address][]*Transaction{}
	for _, tx := range p.queued {
		bySender[tx.From] = append(bySender[tx.From], tx)
	}
	senders := make([]string, 0, len(bySender))
	for from := range bySender {
		senders = append(senders, string(from))
	}
	sort.Strings(senders)

	var candidates []*Transaction
	for _, fs := range senders {
		from := Address(fs)
		txs := bySender[from]
		sort.Slice(txs, func(i, j int) bool { return txs[i].Nonce < txs[j].Nonce })
		expected := p.state.NonceOf(from)
		have := map[uint64]bool{}
		for _, tx := range txs {
			if tx.Nonce < expected {
				continue // 已被确认的残留，跳过
			}
			have[tx.Nonce] = true
		}
		for _, tx := range txs {
			if tx.Nonce < expected {
				continue
			}
			if tx.Nonce != expected {
				break // 编号空洞：本账户后续全部暂缓
			}
			candidates = append(candidates, tx)
			expected++
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.GasPrice != b.GasPrice {
			return a.GasPrice > b.GasPrice
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.Nonce < b.Nonce
	})

	if maxTxs > 0 && len(candidates) > maxTxs {
		candidates = candidates[:maxTxs]
	}
	return candidates
}

// Remove 打包成功（成功或硬拒绝）后移除对应交易；被占位跳过的交易保留，
// 以便后续区块重试。
func (p *Pool) Remove(txs []*Transaction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tx := range txs {
		delete(p.queued, poolKey(tx.From, tx.Nonce))
	}
}

// Reset 在重组后用新规范链状态替换校验基准，并清除所有已确认 nonce 的残留
// 交易（在新链上这些交易无效或已被其他交易占用 nonce）。
func (p *Pool) Reset(state *StateDB) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
	for key, tx := range p.queued {
		if tx.Nonce < state.NonceOf(tx.From) {
			delete(p.queued, key)
		}
	}
}

// Len 返回池中交易数量。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queued)
}
