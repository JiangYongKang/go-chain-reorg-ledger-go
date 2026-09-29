package ledger

import (
	"log"
	"sort"
	"sync"
)

// Pool 是线程安全的待打包交易池。
//
// 策略：
//   - 每个发送方按 nonce 排队；nonce 必须等于链上当前 nonce 才进入 ready 队列，
//     领先的交易进入有限容量的 future 队列，出现编号空洞时后续交易等待而不会提前打包。
//   - nonce 小于链上已用 nonce（或同 nonce 已在池中）视为重复，直接拒绝。
//   - 提交时立即校验签名并全池去重，失败原因可区分。
//   - ready/future 各自有确定容量，满时拒绝并返回 ErrPoolFull/ErrFutureFull，不静默丢弃。
type Pool struct {
	mu        sync.Mutex
	chain     *Chain
	cap       int
	futureCap int
	pending   map[Address]map[uint64]*Transaction // ready：nonce 连续
	future    map[Address]map[uint64]*Transaction // 有 nonce 空洞，暂存等待
	seen      map[string]struct{}                 // 全池去重（含两队列）
	logger    *log.Logger
}

// NewPool 创建交易池。
func NewPool(chain *Chain, cap, futureCap int, logger *log.Logger) *Pool {
	return &Pool{
		chain:     chain,
		cap:       cap,
		futureCap: futureCap,
		pending:   make(map[Address]map[uint64]*Transaction),
		future:    make(map[Address]map[uint64]*Transaction),
		seen:      make(map[string]struct{}),
		logger:    logger,
	}
}

// Add 并发安全地提交一笔交易，返回可区分的拒绝原因。
func (p *Pool) Add(t *Transaction) error {
	if t == nil {
		return ErrMalformedTx
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if !VerifyTxSig(t) {
		p.logger.Printf("[mempool] REJECT tx=%s reason=%v (signature/from mismatch)", short(t.TxHash()), ErrInvalidSig)
		return ErrInvalidSig
	}
	h := t.TxHash()
	if _, dup := p.seen[h]; dup {
		p.logger.Printf("[mempool] REJECT tx=%s reason=%v", h[:12], ErrDuplicateTx)
		return ErrDuplicateTx
	}
	currentNonce := p.chain.State().Nonce(t.From)
	expected := p.expectedNonceLocked(t.From, currentNonce)
	if t.Nonce < currentNonce {
		p.logger.Printf("[mempool] REJECT tx=%s from=%x nonce=%d reason=%v (chain nonce=%d)",
			h[:12], t.From[:6], t.Nonce, ErrDuplicateTx, currentNonce)
		return ErrDuplicateTx
	}

	// 同一发送方同 nonce 的另一笔交易 = 冲突交易（双花），拒绝。
	if q := p.pending[t.From]; q != nil {
		if _, exists := q[t.Nonce]; exists {
			p.logger.Printf("[mempool] REJECT tx=%s from=%x nonce=%d reason=%v (same nonce already queued)",
				h[:12], t.From[:6], t.Nonce, ErrDuplicateTx)
			return ErrDuplicateTx
		}
	}
	if q := p.future[t.From]; q != nil {
		if _, exists := q[t.Nonce]; exists {
			p.logger.Printf("[mempool] REJECT tx=%s from=%x nonce=%d reason=%v (same nonce already queued in future)",
				h[:12], t.From[:6], t.Nonce, ErrDuplicateTx)
			return ErrDuplicateTx
		}
	}

	if t.Nonce == expected {
		if p.sizeLocked() >= p.cap {
			p.logger.Printf("[mempool] REJECT tx=%s reason=%v (pending=%d cap=%d)", h[:12], ErrPoolFull, p.sizeLocked(), p.cap)
			return ErrPoolFull
		}
		p.putPendingLocked(t)
		p.promoteLocked()
		p.logger.Printf("[mempool] ACCEPT tx=%s from=%x nonce=%d -> ready (ready=%d future=%d)",
			h[:12], t.From[:6], t.Nonce, p.sizeLocked(), p.futureCountLocked())
		return nil
	}

	// t.Nonce > expected：编号空洞，暂存到 future。
	if p.futureCountLocked() >= p.futureCap {
		p.logger.Printf("[mempool] REJECT tx=%s nonce=%d reason=%v (expected=%d, future=%d cap=%d)",
			h[:12], t.Nonce, ErrFutureFull, expected, p.futureCountLocked(), p.futureCap)
		return ErrFutureFull
	}
	q := p.future[t.From]
	if q == nil {
		q = make(map[uint64]*Transaction)
		p.future[t.From] = q
	}
	q[t.Nonce] = t
	p.seen[h] = struct{}{}
	p.logger.Printf("[mempool] ACCEPT tx=%s from=%x nonce=%d -> FUTURE (gap: expected=%d, future=%d)",
		h[:12], t.From[:6], t.Nonce, expected, p.futureCountLocked())
	return nil
}

// putPendingLocked 放入 ready 队列（调用方持锁）。
func (p *Pool) putPendingLocked(t *Transaction) {
	q := p.pending[t.From]
	if q == nil {
		q = make(map[uint64]*Transaction)
		p.pending[t.From] = q
	}
	q[t.Nonce] = t
	p.seen[t.TxHash()] = struct{}{}
}

// expectedNonceLocked 返回该发送方下一笔合法交易应有的 nonce：
// 链上 nonce 之后，还要连续跳过 ready 队列中已排队的 nonce。
func (p *Pool) expectedNonceLocked(from Address, chainNonce uint64) uint64 {
	want := chainNonce
	if rq := p.pending[from]; rq != nil {
		for {
			if _, ok := rq[want]; ok {
				want++
			} else {
				break
			}
		}
	}
	return want
}

// promoteLocked 把因 nonce 空洞暂存、目前已连续的 future 交易移入 ready（受 ready 容量限制）。
func (p *Pool) promoteLocked() {
	// 只要存在 future 发送方，就反复尝试推进，直到没有可晋升项。
	for {
		promoted := false
		for from, q := range p.future {
			want := p.expectedNonceLocked(from, p.chain.State().Nonce(from))
			if t, ok := q[want]; ok && p.sizeLocked() < p.cap {
				delete(q, want)
				if len(q) == 0 {
					delete(p.future, from)
				}
				p.putPendingLocked(t)
				promoted = true
			}
		}
		if !promoted {
			return
		}
	}
}

// sizeLocked 返回 ready 交易总数（调用方持锁）。
func (p *Pool) sizeLocked() int {
	n := 0
	for _, q := range p.pending {
		n += len(q)
	}
	return n
}

// PendingSize 返回 ready 队列交易数。
func (p *Pool) PendingSize() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sizeLocked()
}

// FutureSize 返回 future 队列交易数。
func (p *Pool) FutureSize() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, q := range p.future {
		n += len(q)
	}
	return n
}

// Select 确定性地选出最多 max 笔互不冲突的 ready 交易。
// 排序键：(From, Nonce, TxHash) 升序，因此与提交顺序/并发时序无关；
// ready 中每个发送方的 nonce 连续，故不会选出互相冲突的交易。
func (p *Pool) Select(max int) []*Transaction {
	p.mu.Lock()
	defer p.mu.Unlock()
	var all []*Transaction
	for _, q := range p.pending {
		for _, t := range q {
			all = append(all, t)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if c := cmpAddr(a.From, b.From); c != 0 {
			return c < 0
		}
		if a.Nonce != b.Nonce {
			return a.Nonce < b.Nonce
		}
		return a.TxHash() < b.TxHash()
	})
	if max >= 0 && len(all) > max {
		all = all[:max]
	}
	return all
}

// Remove 按交易哈希从池中移除（ready 或 future），并在产生空洞时把超纲的 ready 退回 future。
func (p *Pool) Remove(txHash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeLocked(txHash)
}

func (p *Pool) removeLocked(txHash string) bool {
	for from, q := range p.pending {
		for n, t := range q {
			if t.TxHash() == txHash {
				delete(q, n)
				if len(q) == 0 {
					delete(p.pending, from)
				}
				delete(p.seen, txHash)
				// 该发送方 nonce 出现空洞：把更大 nonce 的 ready 交易退回 future。
				p.demoteLocked(from)
				return true
			}
		}
	}
	for from, q := range p.future {
		for n, t := range q {
			if t.TxHash() == txHash {
				delete(q, n)
				if len(q) == 0 {
					delete(p.future, from)
				}
				delete(p.seen, txHash)
				return true
			}
		}
	}
	return false
}

// demoteLocked 若 ready 中该发送方出现 nonce 空洞，把空洞之后的交易移回 future。
func (p *Pool) demoteLocked(from Address) {
	q := p.pending[from]
	if q == nil {
		return
	}
	want := p.chain.State().Nonce(from)
	for {
		if _, ok := q[want]; ok {
			want++
			continue
		}
		break
	}
	// 所有 nonce >= want（空洞起点）的 ready 交易退回 future
	for n, t := range q {
		if n >= want {
			delete(q, n)
			fq := p.future[from]
			if fq == nil {
				fq = make(map[uint64]*Transaction)
				p.future[from] = fq
			}
			fq[n] = t
		}
	}
	if len(q) == 0 {
		delete(p.pending, from)
	}
}

// OnReorg 在链重组后清理交易池：
//   - 新链包含的交易全部移除；
//   - 旧链独有、被回滚的交易尝试重新入池（签名/nonce/余额由后续打包校验，无效则记录丢弃）。
func (p *Pool) OnReorg(res *ReorgResult) {
	if res == nil || !res.Forked {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// 1) 移除新链已包含的交易。
	for _, b := range res.Reapplied {
		for _, t := range b.Transactions {
			if p.removeLocked(t.TxHash()) {
				p.logger.Printf("[mempool] reorg: drop tx=%s included by new canonical chain", short(t.TxHash()))
			}
		}
	}
	// 2) 被回滚区块中的交易，若不在新链上则重新提交。
	newChainHas := make(map[string]struct{})
	for _, b := range res.Reapplied {
		for _, t := range b.Transactions {
			newChainHas[t.TxHash()] = struct{}{}
		}
	}
	for _, b := range res.RolledBack {
		for _, t := range b.Transactions {
			if _, inNew := newChainHas[t.TxHash()]; inNew {
				continue
			}
			switch err := p.addLocked(t); {
			case err == nil:
				p.logger.Printf("[mempool] reorg: re-queue rolled-back tx=%s", short(t.TxHash()))
			default:
				p.logger.Printf("[mempool] reorg: discard rolled-back tx=%s reason=%v", short(t.TxHash()), err)
			}
		}
	}
}

// addLocked 是 Add 的无锁内核，供 OnReorg 在持锁时复用。
func (p *Pool) addLocked(t *Transaction) error {
	if t == nil {
		return ErrMalformedTx
	}
	if !VerifyTxSig(t) {
		return ErrInvalidSig
	}
	h := t.TxHash()
	if _, dup := p.seen[h]; dup {
		return ErrDuplicateTx
	}
	currentNonce := p.chain.State().Nonce(t.From)
	expected := p.expectedNonceLocked(t.From, currentNonce)
	if t.Nonce < currentNonce {
		return ErrDuplicateTx
	}
	if q := p.pending[t.From]; q != nil {
		if _, exists := q[t.Nonce]; exists {
			return ErrDuplicateTx
		}
	}
	if q := p.future[t.From]; q != nil {
		if _, exists := q[t.Nonce]; exists {
			return ErrDuplicateTx
		}
	}
	if t.Nonce == expected {
		if p.sizeLocked() >= p.cap {
			return ErrPoolFull
		}
		p.putPendingLocked(t)
		p.promoteLocked()
		return nil
	}
	if p.futureCountLocked() >= p.futureCap {
		return ErrFutureFull
	}
	q := p.future[t.From]
	if q == nil {
		q = make(map[uint64]*Transaction)
		p.future[t.From] = q
	}
	q[t.Nonce] = t
	p.seen[h] = struct{}{}
	return nil
}

func (p *Pool) futureCountLocked() int {
	n := 0
	for _, q := range p.future {
		n += len(q)
	}
	return n
}

// cmpAddr 比较两个地址。
func cmpAddr(a, b Address) int {
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
