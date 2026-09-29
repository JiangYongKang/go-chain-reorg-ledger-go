package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
)

// Account 保存一个测试账户的签名密钥与地址。
type Account struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
	Addr Address
}

// GenerateAccount 生成一个新的随机账户。
func GenerateAccount() *Account {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	acc := &Account{Priv: priv, Pub: pub}
	acc.Addr = AddressFromPub(pub)
	return acc
}

// AddressFromPub 由公钥派生地址：sha256(pub) 的前 20 字节。
func AddressFromPub(pub ed25519.PublicKey) Address {
	sum := sha256.Sum256(pub)
	var addr Address
	copy(addr[:], sum[:20])
	return addr
}

// SignTx 用私钥对交易的规范化哈希做 ed25519 签名（64 字节 R||S）。
func SignTx(priv ed25519.PrivateKey, t *Transaction) []byte {
	digest := canonicalTxHash(t)
	return ed25519.Sign(priv, digest)
}

// VerifyTxSig 自包含校验交易签名：
// 公钥长度合法、AddressFromPub(Pub)==From，且 ed25519 签名对规范化哈希有效。
func VerifyTxSig(t *Transaction) bool {
	if t == nil {
		return false
	}
	if len(t.Pub) != ed25519.PublicKeySize || len(t.Signature) != ed25519.SignatureSize {
		return false
	}
	derived := AddressFromPub(ed25519.PublicKey(t.Pub))
	if derived != t.From {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(t.Pub), canonicalTxHash(t), t.Signature)
}

// NewTx 构造并签名一笔交易。
func NewTx(from *Account, to Address, nonce, amount, fee uint64) *Transaction {
	t := &Transaction{
		From:   from.Addr,
		To:     to,
		Nonce:  nonce,
		Amount: amount,
		Fee:    fee,
		Pub:    append([]byte(nil), from.Pub...),
	}
	t.Signature = SignTx(from.Priv, t)
	return t
}
