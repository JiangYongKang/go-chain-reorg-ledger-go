// Package ledger 实现一条简化链的账本状态转移、分叉重组以及并发待打包交易管理。
//
// 本文件集中定义跨模块共享的核心类型：账户、交易、区块、收据、错误以及
// 简化版的签名/哈希工具。签名仅做教学用途，不具备真实密码学安全性。
package ledger

import (
	"errors"
	"fmt"
)

// Address 是账户地址，这里直接用账户标识字符串表示。
type Address string

// Hash 是交易或区块的内容标识。
type Hash []byte

// ZeroHash 是空 Hash。
var ZeroHash = Hash(nil)

// Hex 返回 Hash 的十六进制短表示，便于日志判读。
func (h Hash) Hex() string {
	if len(h) == 0 {
		return "0x"
	}
	const short = 8
	if len(h) < short {
		return fmt.Sprintf("%x", []byte(h))
	}
	return fmt.Sprintf("%x…", []byte(h[:short]))
}

// Equal 判断两个 Hash 是否相同（nil 与空切片视为相等）。
func (h Hash) Equal(other Hash) bool {
	if len(h) == 0 || len(other) == 0 {
		return len(h) == len(other)
	}
	return string(h) == string(other)
}

// Transaction 描述一笔账户间转账。
type Transaction struct {
	From     Address
	To       Address
	Nonce    uint64
	Amount   uint64
	GasPrice uint64 // 打包排序依据：越高越优先
	Payload  string // 任意附加数据，参与哈希
	Sig      []byte // 签名：简化为账户密钥的副本
}

// Block 是一批交易的有序容器，并通过 ParentHash 构成链/树。
type Block struct {
	Height     uint64
	ParentHash Hash
	Txs        []*Transaction

	hash Hash // 缓存哈希
}

// Receipt 是单笔交易执行结论。任何入口（交易池提交、区块执行、重放）
// 对同一交易在同一前置状态下都必须得到同构的收据。
type Receipt struct {
	TxHash  Hash
	Status  TxStatus
	Reason  Reason // 被拒绝/跳过/成功时的可区分原因
	From    Address
	To      Address
	Nonce   uint64
	Amount  uint64
	Balance uint64 // 执行后 From 的余额（成功时）
}

// TxStatus 表示交易执行结论。
type TxStatus int

const (
	TxSuccess  TxStatus = iota
	TxRejected          // 验证失败，永远不应该生效
	TxSkipped           // 区块内占位：前序交易导致其在本批次无法执行
)

// Reason 是可区分的判定原因，用于日志与跨入口一致性校验。
type Reason string

const (
	ReasonOK                Reason = "ok"
	ReasonDuplicate         Reason = "duplicate_tx"         // 同一账户相同 nonce 重复
	ReasonNonceTooLow       Reason = "nonce_too_low"        // nonce 已被使用
	ReasonNonceTooHigh      Reason = "nonce_too_high"       // nonce 超前（编号空洞）
	ReasonInsufficientFunds Reason = "insufficient_funds"   // 余额不足
	ReasonInvalidSignature  Reason = "invalid_signature"    // 签名无效
	ReasonPoolFull          Reason = "pool_full"            // 交易池已满
	ReasonConflict          Reason = "conflict"             // 并发冲突：同账户同 nonce 竞争
	ReasonBlockExists       Reason = "block_exists"         // 区块重复导入
	ReasonUnknownParent     Reason = "unknown_parent"       // 父区块未知
	ReasonAncestorGap       Reason = "ancestor_gap"         // 祖先链不完整
	ReasonSkippedNonceGap   Reason = "skipped_nonce_gap"    // 打包/执行时 nonce 空洞占位
	ReasonSkippedBalance    Reason = "skipped_insufficient" // 同批前序扣款导致余额不足占位
	ReasonInvalidAmount     Reason = "invalid_amount"       // 非法金额
	ReasonSelfTransfer      Reason = "self_transfer"        // 自转：允许，仅作记录保留位
)

// GenesisAccount 是创世账户配置。
type GenesisAccount struct {
	Address Address
	Balance uint64
	Secret  []byte // 该账户持有的简化签名密钥
}

// ReorgReport 描述一次链重组的关键判定依据。
type ReorgReport struct {
	Triggered       bool
	OldHead         Hash
	NewHead         Hash
	CommonAncestor  Hash
	RolledBack      []Hash // 从旧头向下到分叉点（不含分叉点）
	Replayed        []Hash // 从分叉点（不含）到新头
	OldChainWeight  uint64
	NewChainWeight  uint64
	RejectedOnReorg []*Receipt // 在新分支重放后仍然失败的交易收据
}

// 预定义的哨兵错误，均可用 errors.Is 判定。
var (
	ErrDuplicateTx       = errors.New("duplicate transaction")
	ErrNonceTooLow       = errors.New("nonce too low")
	ErrNonceTooHigh      = errors.New("nonce too high")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrInvalidSignature  = errors.New("invalid signature")
	ErrPoolFull          = errors.New("transaction pool full")
	ErrConflict          = errors.New("concurrent transaction conflict")
	ErrBlockExists       = errors.New("block already exists")
	ErrUnknownParent     = errors.New("unknown parent block")
	ErrAncestorGap       = errors.New("incomplete ancestor chain")
	ErrInvalidAmount     = errors.New("invalid amount")
)

// reasonSentinel 给出原因到哨兵错误的映射。
var reasonSentinel = map[Reason]error{
	ReasonDuplicate:         ErrDuplicateTx,
	ReasonNonceTooLow:       ErrNonceTooLow,
	ReasonNonceTooHigh:      ErrNonceTooHigh,
	ReasonInsufficientFunds: ErrInsufficientFunds,
	ReasonInvalidSignature:  ErrInvalidSignature,
	ReasonPoolFull:          ErrPoolFull,
	ReasonConflict:          ErrConflict,
	ReasonBlockExists:       ErrBlockExists,
	ReasonUnknownParent:     ErrUnknownParent,
	ReasonAncestorGap:       ErrAncestorGap,
	ReasonInvalidAmount:     ErrInvalidAmount,
}

// TxError 携带可区分原因的交易错误。
type TxError struct {
	Reason Reason
	Err    error
}

func (e *TxError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("tx rejected: %s: %v", e.Reason, e.Err)
}

func (e *TxError) Unwrap() error { return e.Err }

// rejectTx 构造一个带原因的交易错误。
func rejectTx(reason Reason) *TxError {
	return &TxError{Reason: reason, Err: reasonSentinel[reason]}
}

// keyRegistry 是简化签名方案下的“地址 -> 密钥”登记表。
var keyRegistry = map[string][]byte{}

// RegisterKey 登记一个账户的签名密钥。
func RegisterKey(addr Address, secret []byte) {
	keyRegistry[string(addr)] = append([]byte(nil), secret...)
}

// LookupKey 查询已登记密钥。
func LookupKey(addr Address) ([]byte, bool) {
	s, ok := keyRegistry[string(addr)]
	return s, ok
}

// SignTx 用账户密钥对交易签名，返回签名后的新交易。
// 简化签名 = 密钥本身副本（足以演示“密钥不匹配即无效”的判定，非真实密码学）。
func SignTx(tx *Transaction, secret []byte) *Transaction {
	cp := *tx
	cp.Sig = append([]byte(nil), secret...)
	return &cp
}

// validSig 判断交易签名是否与登记的账户密钥一致。
func validSig(tx *Transaction) bool {
	if tx == nil || len(tx.Sig) == 0 {
		return false
	}
	secret, ok := LookupKey(tx.From)
	if !ok {
		return false
	}
	return string(secret) == string(tx.Sig)
}

// writeUint64 把 uint64 以大端定长 8 字节写入哈希缓冲，保证字段定界无歧义。
func writeUint64(buf *[]byte, v uint64) {
	*buf = append(*buf,
		byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// hashBytes 对一段字节做 FNV-1a 64 位哈希，返回 8 字节摘要。
// 该哈希只用于本地内容标识，不提供抗碰撞安全保证。
func hashBytes(data []byte) Hash {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for _, b := range data {
		h ^= uint64(b)
		h *= prime64
	}
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = byte(h)
		h >>= 8
	}
	return Hash(out)
}

// TxHash 计算交易哈希：由全部语义字段决定。
// 字段以“长度+内容/定长”方式拼接，保证不同交易不会因序列化歧义而碰撞。
func TxHash(tx *Transaction) Hash {
	if tx == nil {
		return ZeroHash
	}
	var buf []byte
	put := func(s string) {
		writeUint64(&buf, uint64(len(s)))
		buf = append(buf, s...)
	}
	put(string(tx.From))
	put(string(tx.To))
	writeUint64(&buf, tx.Nonce)
	writeUint64(&buf, tx.Amount)
	writeUint64(&buf, tx.GasPrice)
	put(tx.Payload)
	return hashBytes(buf)
}

// Hash 返回交易哈希。
func (tx *Transaction) Hash() Hash {
	if tx == nil {
		return ZeroHash
	}
	return TxHash(tx)
}

// txMerkleRoot 按块内交易顺序计算简化 Merkle 根：
// 逐层成对 FNV 合并，奇数个节点时复制最后一个；空列表哈希为 ZeroHash。
func txMerkleRoot(txs []*Transaction) Hash {
	if len(txs) == 0 {
		return ZeroHash
	}
	layer := make([]Hash, len(txs))
	for i, tx := range txs {
		layer[i] = tx.Hash()
	}
	for len(layer) > 1 {
		next := make([]Hash, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			right := layer[i]
			if i+1 < len(layer) {
				right = layer[i+1]
			}
			next = append(next, hashBytes(append(append([]byte(nil), layer[i]...), right...)))
		}
		layer = next
	}
	return layer[0]
}

// Hash 返回区块哈希：父哈希 + 高度 + 交易 Merkle 根，结果会缓存。
func (b *Block) Hash() Hash {
	if b == nil {
		return ZeroHash
	}
	if b.hash != nil {
		return b.hash
	}
	var buf []byte
	buf = append(buf, b.ParentHash...)
	writeUint64(&buf, b.Height)
	buf = append(buf, txMerkleRoot(b.Txs)...)
	b.hash = hashBytes(buf)
	return b.hash
}
