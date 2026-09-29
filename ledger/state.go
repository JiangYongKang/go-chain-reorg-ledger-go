package ledger

import "sort"

// Effect 记录一笔成功交易对状态的影响，用于重组时精确回滚。
type Effect struct {
	TxHash   Hash
	From     Address
	To       Address
	Nonce    uint64
	Amount   uint64
	FromPrev uint64 // From 执行前余额
	ToPrev   uint64 // To 执行前余额
}

// accountState 是单个账户的可变状态。
type accountState struct {
	balance uint64
	nonce   uint64 // 下一个期望使用的 nonce
}

// StateDB 是内存账本：账户余额/nonce、交易唯一性索引以及区块效果日志。
type StateDB struct {
	accounts map[Address]*accountState
	// spent 记录“账户 -> nonce -> 交易哈希”，保证同一 nonce 的交易只生效一次。
	spent map[Address]map[uint64]Hash
	// effectsByBlock 记录每个已应用区块产生的效果（按执行顺序）。
	effectsByBlock map[string][]*Effect
}

// NewGenesisState 依据创世账户创建初始状态，并登记各自的签名密钥。
func NewGenesisState(accounts ...GenesisAccount) *StateDB {
	s := &StateDB{
		accounts:       map[Address]*accountState{},
		spent:          map[Address]map[uint64]Hash{},
		effectsByBlock: map[string][]*Effect{},
	}
	for _, g := range accounts {
		s.accounts[g.Address] = &accountState{balance: g.Balance}
		if g.Secret != nil {
			RegisterKey(g.Address, g.Secret)
		}
	}
	return s
}

// account 返回（或惰性创建）账户状态。调用方需保证语义正确：
// 仅在交易真正生效时才应创建收款方账户。
func (s *StateDB) account(addr Address) *accountState {
	a, ok := s.accounts[addr]
	if !ok {
		a = &accountState{}
		s.accounts[addr] = a
	}
	return a
}

// Copy 深拷贝状态，用于分叉点快照与分支试算。
func (s *StateDB) Copy() *StateDB {
	cp := &StateDB{
		accounts:       make(map[Address]*accountState, len(s.accounts)),
		spent:          make(map[Address]map[uint64]Hash, len(s.spent)),
		effectsByBlock: make(map[string][]*Effect, len(s.effectsByBlock)),
	}
	for addr, a := range s.accounts {
		cp.accounts[addr] = &accountState{balance: a.balance, nonce: a.nonce}
	}
	for addr, m := range s.spent {
		nm := make(map[uint64]Hash, len(m))
		for n, h := range m {
			nm[n] = h
		}
		cp.spent[addr] = nm
	}
	for k, effs := range s.effectsByBlock {
		cp.effectsByBlock[k] = append([]*Effect(nil), effs...)
	}
	return cp
}

// BalanceOf 返回账户余额（未知账户为 0）。
func (s *StateDB) BalanceOf(addr Address) uint64 {
	if a, ok := s.accounts[addr]; ok {
		return a.balance
	}
	return 0
}

// NonceOf 返回账户下一个期望 nonce（未知账户为 0）。
func (s *StateDB) NonceOf(addr Address) uint64 {
	if a, ok := s.accounts[addr]; ok {
		return a.nonce
	}
	return 0
}

// Validate 只做无副作用的交易校验。所有入口（交易池、单笔执行、区块批量
// 执行、分叉重放）共用本函数，保证同一前置状态下结论一致。
//
// 判定顺序固定，失败原因互斥可区分：
//  1. 签名无效（含空交易/无签名/账户密钥不匹配）
//  2. nonce 重复：完全相同的交易再次提交
//  3. nonce 过低：该 nonce 已被另一笔交易占用（编号乱序）
//  4. nonce 超前：出现编号空洞
//  5. 余额不足
func (s *StateDB) Validate(tx *Transaction) *TxError {
	if tx == nil {
		return rejectTx(ReasonInvalidSignature)
	}
	if !validSig(tx) {
		return rejectTx(ReasonInvalidSignature)
	}
	expected := s.NonceOf(tx.From)
	switch {
	case tx.Nonce < expected:
		if existing, ok := s.spent[tx.From][tx.Nonce]; ok && existing.Equal(tx.Hash()) {
			return rejectTx(ReasonDuplicate)
		}
		return rejectTx(ReasonNonceTooLow)
	case tx.Nonce > expected:
		return rejectTx(ReasonNonceTooHigh)
	}
	if s.BalanceOf(tx.From) < tx.Amount {
		return rejectTx(ReasonInsufficientFunds)
	}
	return nil
}

// newReceipt 依据错误构造收据；成功时 amount/余额字段由调用方补齐。
func newReceipt(tx *Transaction, status TxStatus, reason Reason) *Receipt {
	r := &Receipt{
		TxHash: tx.Hash(),
		Status: status,
		Reason: reason,
		From:   tx.From,
		To:     tx.To,
		Nonce:  tx.Nonce,
		Amount: tx.Amount,
	}
	return r
}

// receiptForError 把校验错误映射为“硬拒绝”收据。
func receiptForError(tx *Transaction, e *TxError) *Receipt {
	return newReceipt(tx, TxRejected, e.Reason)
}

// Apply 执行单笔交易：先校验，成功才落状态；任何失败都不改变状态。
func (s *StateDB) Apply(tx *Transaction) *Receipt {
	if e := s.Validate(tx); e != nil {
		return receiptForError(tx, e)
	}
	return s.commit(tx, ReasonOK)
}

// commit 在调用方已完成校验的前提下落状态，并把效果记入“未归属”日志之外
// 的指定区块（空 key 表示单笔临时执行，不保留效果日志——供测试/扩展使用）。
func (s *StateDB) commit(tx *Transaction, reason Reason) *Receipt {
	from := s.account(tx.From)
	to := s.account(tx.To)
	from.balance -= tx.Amount
	to.balance += tx.Amount
	from.nonce++
	if _, ok := s.spent[tx.From]; !ok {
		s.spent[tx.From] = map[uint64]Hash{}
	}
	s.spent[tx.From][tx.Nonce] = tx.Hash()

	r := newReceipt(tx, TxSuccess, reason)
	r.Balance = from.balance
	return r
}

// ExecuteBlock 对一个区块内的交易做“确定性规范化排序”后批量执行，
// 保证同一批交易无论打包顺序如何，最终账本状态完全一致。
//
// 规范执行顺序：(From 地址升序, nonce 升序, 交易哈希升序)。
// 返回与块内交易位置一一对应的收据；成功效果登记在该区块名下，
// 供 RevertBlock 在重组时精确反向回滚。
func (s *StateDB) ExecuteBlock(blockHash Hash, txs []*Transaction) []*Receipt {
	order := make([]int, len(txs))
	for i := range txs {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := txs[order[a]], txs[order[b]]
		if x.From != y.From {
			return x.From < y.From
		}
		if x.Nonce != y.Nonce {
			return x.Nonce < y.Nonce
		}
		return string(x.Hash()) < string(y.Hash())
	})

	receipts := make([]*Receipt, len(txs))
	var effects []*Effect
	key := string(blockHash)
	// appliedInBlock 记录本块内每个账户是否已成功执行过交易，
	// 用于区分“交易自身就非法（硬拒绝）”与“同批前序扣款后才余额不足（占位跳过）”。
	appliedInBlock := map[Address]bool{}

	for _, idx := range order {
		tx := txs[idx]
		if e := s.Validate(tx); e != nil {
			switch e.Reason {
			case ReasonNonceTooHigh:
				// 编号空洞：本块内占位跳过，不生效也不硬销毁，后续区块可重试。
				receipts[idx] = newReceipt(tx, TxSkipped, ReasonSkippedNonceGap)
			case ReasonInsufficientFunds:
				if appliedInBlock[tx.From] {
					// 同账户本块已有前序交易成功扣款：是批次内累积导致，占位跳过。
					receipts[idx] = newReceipt(tx, TxSkipped, ReasonSkippedBalance)
				} else {
					// 在当前前置状态下自身就余额不足：与单笔入口一致，硬拒绝。
					receipts[idx] = receiptForError(tx, e)
				}
			default:
				receipts[idx] = receiptForError(tx, e)
			}
			continue
		}
		from := s.account(tx.From)
		to := s.account(tx.To)
		eff := &Effect{
			TxHash:   tx.Hash(),
			From:     tx.From,
			To:       tx.To,
			Nonce:    tx.Nonce,
			Amount:   tx.Amount,
			FromPrev: from.balance,
			ToPrev:   to.balance,
		}
		r := s.commit(tx, ReasonOK)
		effects = append(effects, eff)
		appliedInBlock[tx.From] = true
		receipts[idx] = r
	}

	if len(effects) > 0 {
		s.effectsByBlock[key] = append(s.effectsByBlock[key], effects...)
	}
	return receipts
}

// RevertBlock 回滚指定区块产生的全部效果（严格逆序），
// 恢复余额/nonce 并清除该区块写入的唯一性索引。
func (s *StateDB) RevertBlock(blockHash Hash) {
	effs := s.effectsByBlock[string(blockHash)]
	for i := len(effs) - 1; i >= 0; i-- {
		e := effs[i]
		if from, ok := s.accounts[e.From]; ok {
			from.balance = e.FromPrev
			from.nonce = e.Nonce
		}
		if to, ok := s.accounts[e.To]; ok {
			to.balance = e.ToPrev
		}
		if m, ok := s.spent[e.From]; ok {
			delete(m, e.Nonce)
		}
	}
	delete(s.effectsByBlock, string(blockHash))
}

// Root 返回状态指纹：对所有账户（地址升序）的余额与 nonce 做确定性摘要，
// 用于重组前后、不同入口执行结果的一致性自检。
func (s *StateDB) Root() Hash {
	addrs := make([]string, 0, len(s.accounts))
	for addr := range s.accounts {
		addrs = append(addrs, string(addr))
	}
	sort.Strings(addrs)
	var buf []byte
	for _, addr := range addrs {
		a := s.accounts[Address(addr)]
		buf = append(buf, addr...)
		writeUint64(&buf, a.balance)
		writeUint64(&buf, a.nonce)
	}
	return hashBytes(buf)
}
