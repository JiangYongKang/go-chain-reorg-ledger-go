// Command demo 用本地状态演示一次完整的：提交交易 → 出块 → 分叉 → 高权重分支重组。
// 运行：go run ./cmd/demo
package main

import (
	"fmt"

	"github.com/highcumontoa/go-chain-reorg-ledger-go/ledger"
)

func main() {
	gen := ledger.NewGenesisState(
		ledger.GenesisAccount{Address: "alice", Balance: 100, Secret: []byte("k-alice")},
		ledger.GenesisAccount{Address: "bob", Balance: 50, Secret: []byte("k-bob")},
	)
	node := ledger.NewNode(gen, 100)
	tx := func(from, to string, nonce, amount, gas uint64) *ledger.Transaction {
		sk, _ := ledger.LookupKey(ledger.Address(from))
		return ledger.SignTx(&ledger.Transaction{
			From: ledger.Address(from), To: ledger.Address(to),
			Nonce: nonce, Amount: amount, GasPrice: gas,
		}, sk)
	}

	// 1) 异常交易在入口处被区分原因拒绝。
	for _, bad := range []*ledger.Transaction{
		tx("alice", "bob", 0, 9999, 1), // 余额不足
		ledger.SignTx(&ledger.Transaction{From: "alice", To: "bob", Nonce: 0, Amount: 1}, []byte("forged")),
	} {
		r := node.SubmitTx(bad)
		fmt.Printf("提交拒绝: tx=%s reason=%s\n", bad.Hash().Hex(), r.Reason)
	}

	// 2) 正常交易并发式提交并出块。
	for _, t := range []*ledger.Transaction{
		tx("alice", "bob", 0, 20, 1),
		tx("alice", "bob", 1, 10, 2),
	} {
		if r := node.SubmitTx(t); !r.Accepted {
			panic(r.Reason)
		}
	}
	b1, recs, err := node.ProduceBlock(10)
	if err != nil {
		panic(err)
	}
	fmt.Printf("本地出块 B1=%s，%d 笔成功，alice=%d bob=%d\n",
		b1.Hash().Hex(), len(recs), node.Chain().State().BalanceOf("alice"), node.Chain().State().BalanceOf("bob"))

	// 3) 对端在同一高度挖出含更多交易的竞争块 => 更高权重，触发重组。
	remote := ledger.MakeBlock(nil, 1, []*ledger.Transaction{
		tx("alice", "bob", 0, 30, 1),
		tx("bob", "alice", 0, 5, 1),
		tx("bob", "alice", 1, 5, 1),
	})
	rep, err := node.ReceiveBlock(remote)
	if err != nil {
		panic(err)
	}
	fmt.Printf("重组: triggered=%v 回滚=%v 重放=%v\n", rep.Triggered, hexes(rep.RolledBack), hexes(rep.Replayed))
	fmt.Printf("重组后余额: alice=%d bob=%d 状态指纹=%s\n",
		node.Chain().State().BalanceOf("alice"),
		node.Chain().State().BalanceOf("bob"),
		node.Chain().State().Root().Hex())
}

func hexes(hs []ledger.Hash) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Hex()
	}
	return out
}
