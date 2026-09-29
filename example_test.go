package ledger_test

import (
	"fmt"
	"io"
	"log"

	ledger "github.com/highcumontoa/go-chain-reorg-ledger-go"
)

// Example 演示：并发提交、确定性出块、分叉重组、回滚交易回收的完整流程。
func Example() {
	logger := log.New(io.Discard, "", 0)

	alice := ledger.GenerateAccount()
	bob := ledger.GenerateAccount()
	miner := ledger.GenerateAccount()

	chain := ledger.NewChain(map[ledger.Address]uint64{alice.Addr: 1000}, logger)
	node := ledger.NewNode(chain, miner.Addr, 100, 100, logger)

	// 提交两笔交易并出块（旧链）。
	_ = node.Submit(ledger.NewTx(alice, bob.Addr, 0, 100, 10))
	_ = node.Submit(ledger.NewTx(alice, bob.Addr, 1, 50, 10))
	oldBlock, _ := node.ProduceBlock(1, 10)
	fmt.Printf("old block txs=%d alice=%d bob=%d\n",
		len(oldBlock.Transactions), chain.Balance(alice.Addr), chain.Balance(bob.Addr))

	// 更高权重分叉：把同一发送方的 nonce 0 换成转给矿工，重组后旧交易被回滚。
	genesis, _ := chain.GetBlock(ledger.GenesisHash)
	forkTx := ledger.NewTx(alice, miner.Addr, 0, 300, 20)

	st, ok := chain.StateAtBlock(ledger.GenesisHash)
	if !ok {
		panic("genesis snapshot missing")
	}
	r := st.Apply(forkTx, miner.Addr)
	if !r.Success {
		panic(r.Reason)
	}
	fork := &ledger.Block{
		Height:       1,
		ParentHash:   genesis.Hash,
		Coinbase:     miner.Addr,
		Weight:       5,
		Transactions: []*ledger.Transaction{forkTx},
	}
	fork.Hash = fork.BlockHash()
	fork.StateRoot = st.RootHash()

	res, err := chain.AcceptBlock(fork)
	if err != nil {
		panic(err)
	}
	fmt.Printf("reorg=%v rolled_back=%d reapplied=%d\n",
		res.Forked, len(res.RolledBack), len(res.Reapplied))
	fmt.Printf("after reorg: alice=%d bob=%d miner=%d\n",
		chain.Balance(alice.Addr), chain.Balance(bob.Addr), chain.Balance(miner.Addr))

	// 被回滚的旧 nonce-0/1 交易中，与新链冲突的会被重新校验；这里演示明确的拒绝原因。
	err = node.Submit(ledger.NewTx(alice, bob.Addr, 0, 100, 10))
	fmt.Println("re-submit nonce0 after reorg rejected:", err)

	// Output:
	// old block txs=2 alice=830 bob=150
	// reorg=true rolled_back=1 reapplied=1
	// after reorg: alice=680 bob=0 miner=320
	// re-submit nonce0 after reorg rejected: duplicate transaction
}
