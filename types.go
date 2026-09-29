package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

// Address 是链上账户地址（长度为 20 字节）。
type Address [20]byte

// 交易校验失败时返回的可区分错误。
var (
	ErrDuplicateTx     = errors.New("duplicate transaction")
	ErrInvalidNonce    = errors.New("invalid nonce: nonce out of order")
	ErrInsufficientBal = errors.New("insufficient balance")
	ErrInvalidSig      = errors.New("invalid signature")
)

// 交易池相关错误。
var (
	ErrPoolFull    = errors.New("mempool is full")
	ErrFutureFull  = errors.New("mempool future queue is full")
	ErrMalformedTx = errors.New("malformed transaction")
)

// Transaction 描述一笔账户间的价值转移交易。
type Transaction struct {
	From      Address
	To        Address
	Nonce     uint64
	Amount    uint64
	Fee       uint64
	Pub       []byte // 发送方 ed25519 公钥（32 字节），必须满足 AddressFromPub(Pub)==From
	Signature []byte // 64 字节 ed25519 签名，签名内容为交易规范化哈希
}

// Receipt 是一笔交易执行后的收据。
type Receipt struct {
	TxHash   string
	Success  bool
	Reason   error // 失败原因（仅 Success=false 时有意义）
	From     Address
	To       Address
	Nonce    uint64
	GasUsed  uint64
	PostFrom uint64
	PostTo   uint64
}

// Block 描述一个区块。
type Block struct {
	Height       uint64
	ParentHash   string
	Coinbase     Address
	Weight       uint64 // 区块自身权重（简化版 PoW，累计权重越大分支越重）
	Transactions []*Transaction
	Hash         string
	StateRoot    string // 执行完本区块后的状态根
}

// writeStr 以 8 字节大端长度前缀写入字符串，保证编码无歧义。
func writeStr(h interface{ Write([]byte) (int, error) }, s string) {
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(s)))
	_, _ = h.Write(l[:])
	_, _ = h.Write([]byte(s))
}

// TxHash 返回交易的规范化哈希（签名内容本身），十六进制字符串。
func (t *Transaction) TxHash() string {
	return hex.EncodeToString(canonicalTxHash(t))
}

// canonicalTxHash 计算交易的规范化哈希：
// From(20) || To(20) || Nonce(8) || Amount(8) || Fee(8)。
// 刻意不含签名字段，签名与身份验证使用同一份摘要。
func canonicalTxHash(t *Transaction) []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, t.From[:]...)
	buf = append(buf, t.To[:]...)
	var u [8]byte
	binary.BigEndian.PutUint64(u[:], t.Nonce)
	buf = append(buf, u[:]...)
	binary.BigEndian.PutUint64(u[:], t.Amount)
	buf = append(buf, u[:]...)
	binary.BigEndian.PutUint64(u[:], t.Fee)
	buf = append(buf, u[:]...)
	sum := sha256.Sum256(buf)
	return sum[:]
}

// BlockHash 返回区块规范化哈希；不含 Hash/StateRoot 字段，
// 内容为 Height || ParentHash(带长度前缀) || Coinbase || Weight || 各交易哈希顺序拼接。
func (b *Block) BlockHash() string {
	h := sha256.New()
	var u [8]byte
	binary.BigEndian.PutUint64(u[:], b.Height)
	_, _ = h.Write(u[:])
	writeStr(h, b.ParentHash)
	_, _ = h.Write(b.Coinbase[:])
	binary.BigEndian.PutUint64(u[:], b.Weight)
	_, _ = h.Write(u[:])
	for _, t := range b.Transactions {
		_, _ = h.Write([]byte(t.TxHash())) // 定长十六进制，顺序即内容
	}
	return hex.EncodeToString(h.Sum(nil))
}
