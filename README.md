# go-chain-reorg-ledger

一个用本地内存状态模拟简化区块链账本与出块流程的教学项目。目标是让三类结论在
**任何入口下都一致且可解释**：

1. 账户余额与 nonce 的状态转移（重复 / 乱序 / 余额不足 / 签名无效必须被区分原因地拒绝）；
2. 分叉与链重组（回滚已应用区块、重放高权重分支，余额与收据自洽）；
3. 并发提交待打包交易并选出无冲突打包集合（无数据竞争、池满与编号空洞有确定策略）。

## 目录结构

| 文件 | 职责 |
| --- | --- |
| `ledger/types.go` | 账户、交易、区块、收据、错误类型；简化签名（密钥比对）与 FNV 内容哈希 / 区块 Merkle 根 |
| `ledger/state.go` | `StateDB`：余额/nonce、交易唯一性索引、统一校验入口、确定性批量执行、效果日志回滚、状态指纹 |
| `ledger/pool.go` | `Pool`：互斥锁保护的并发安全交易池、确定性打包排序、池满 / 空洞 / 冲突策略 |
| `ledger/chain.go` | `Chain`：区块树、累计权重分支选择、回滚-重放重组、收据索引 |
| `ledger/node.go` | `Node`：把交易池与链组合成统一入口（提交 / 出块 / 接收区块） |
| `cmd/demo` | 可运行的端到端演示 |

## 一、状态转移规则

### 1.1 统一校验入口

所有入口——交易池 `Pool.Submit`、单笔 `StateDB.Apply`、区块批量
`StateDB.ExecuteBlock`、分叉重放——都复用同一套校验判定
（`StateDB.Validate`），判定顺序固定、原因互斥：

| 顺序 | 判定 | 失败原因 `Reason` | 哨兵错误 |
| --- | --- | --- | --- |
| 1 | 空交易 / 无签名 / 密钥不匹配 | `invalid_signature` | `ErrInvalidSignature` |
| 2 | 同账户同 nonce 且交易哈希完全相同（再次提交） | `duplicate_tx` | `ErrDuplicateTx` |
| 3 | nonce 已被**另一笔**交易占用（编号乱序/倒退） | `nonce_too_low` | `ErrNonceTooLow` |
| 4 | nonce 超前于账户当前期望值（编号空洞） | `nonce_too_high` | `ErrNonceTooHigh` |
| 5 | 付款方余额 < 金额 | `insufficient_funds` | `ErrInsufficientFunds` |

被拒绝的交易**不产生任何状态变化**，返回带 `Reason` 的 `TxError`（可用
`errors.Is` 匹配哨兵错误）和同构 `Receipt`，绝不静默生效。

### 1.2 收据

每笔交易产出一条 `Receipt`：`TxSuccess` / `TxRejected`（硬拒绝）/
`TxSkipped`（仅在区块内占位）。`TxSkipped` 只用于两种情况：

- `skipped_nonce_gap`：块内出现编号空洞，该交易本块无法执行，保留待后续区块重试；
- `skipped_insufficient`：同一账户在本块已有前序交易成功扣款，累积后余额才不足。

  注意：若一笔交易在**当前前置状态下自身**就余额不足，属于硬拒绝
  `insufficient_funds`，与单笔入口结论一致，不会被当作跳过。

### 1.3 批量执行顺序无关性（commutativity）

区块执行时，先对交易按 **(From 地址升序, nonce 升序, 交易哈希升序)**
做确定性规范化排序，再逐笔执行。因此同一批交易无论以何种顺序打包进块，
最终余额、nonce、唯一性索引、每笔收据结论都完全一致
（见 `TestBlockExecutionOrderIndependent`，以状态指纹 `StateDB.Root()` 比对）。

成功交易的每次效果（双方旧余额、nonce）记入该区块的**效果日志**，
重组时按严格逆序恢复。

## 二、分叉与链重组

### 2.1 区块树与权重

- 区块通过 `ParentHash` 构成树；导入新区块时在“**父区块结果状态的副本**”上执行，
  因此每个区块的结论只依赖父状态，与区块到达顺序无关。
- 单块权重 `BlockWeight = 1 + 块内交易数`，分支权重为沿链累计之和（简化工作量）。
- **分支选择**：新分支累计权重 **严格大于** 当前规范链才切换；
  平手或更轻时只保存区块、不切换，保证无抖动、结论确定。

### 2.2 重组过程（`Chain.AddBlock` 返回 `ReorgReport`）

1. 求新旧链的最近共同祖先（分叉点）；
2. **回滚**：从旧头向下到分叉点（不含分叉点），对每个块调用
   `RevertBlock`，按效果日志逆序恢复余额、nonce 并清除该块写入的唯一性索引；
3. **重放**：从分叉点之后到新头，逐块 `ExecuteBlock`；
   重放中失败的交易收据收集到 `ReorgReport.RejectedOnReorg`；
4. **收敛自检**：回滚+重放得到的状态指纹，必须与分支导入时直接在父状态副本上
   执行的指纹一致，否则 `panic`（显式失败，绝不静默错乱）；
5. 切换规范链与收据索引。

由此保证：不重复扣款、不漏扣、已确认交易若在新链不存在则其收据从规范链移除
（不静默保留），重放失败交易不生效。`ReorgReport` 携带分支权重、分叉点、
回滚/重放哈希范围，测试日志会打印这些关键判定依据。

## 三、并发与打包策略

### 3.1 并发提交（无数据竞争）

`Pool` 用互斥锁串行化提交。同一 `(账户, nonce)` 的多个不同交易体并发竞争时：

- 恰好 1 笔被接受；
- 其余全部以 `conflict` 原因拒绝（`ErrConflict`），带收据，不静默丢弃；
- 完全相同的交易重复提交幂等成功（`duplicate_tx`），不重复入池。

### 3.2 池满与编号空洞

- **池满**：容量达到上限后新交易以 `pool_full` 确定拒绝，不淘汰旧交易；
- **编号空洞**：nonce 超前的交易**允许排队**（提交原因标注 `nonce_too_high`），
  但 `Pack` 时每个账户从期望 nonce 起连续选取，遇到空洞立即停止，
  空洞之后的交易本轮一律不打包，待前序补齐后自动可打包；
- 即使存在空洞，签名错误、余额不足等硬错误仍优先判定，不会被空洞掩盖。

### 3.3 确定性打包集合

`Pool.Pack`：先按账户取连续 nonce 段，再跨账户按
**gasPrice 降序、(from, nonce) 升序**排序，取前 `maxTxs` 笔。
打包集合中不可能出现互相冲突的交易。

### 3.4 出块与重组后的池状态

- `Node.ProduceBlock`：打包 → 以当前头为父出块并导入；被规范链接受后，
  成功与硬拒绝交易移出交易池，**被跳过（空洞/累积不足）的交易保留重试**；
- `Node.ReceiveBlock`：若触发重组，用新规范链状态 `Pool.Reset` 刷新校验基准，
  清理在新链已确认 nonce 的残留交易，避免跨分叉状态错乱。

## 四、验证方法

```bash
# 编译与静态检查
go build ./...
go vet ./...

# 全部单元测试（并发场景建议带 -race）
go test -race ./... -v

# 稳定性复现：并发冲突与重组用例重复执行 N 次
go test -race -count=20 ./...

# 端到端演示
go run ./cmd/demo
```

### 测试覆盖

| 测试 | 覆盖点 |
| --- | --- |
| `TestApplyHappyPath` | 正常转账余额/nonce 推进 |
| `TestRejectReasonsAreDistinguishable` | 五类拒绝原因可区分、失败交易不污染状态 |
| `TestBlockExecutionOrderIndependent` | 两种打包顺序状态指纹与收据完全一致 |
| `TestNonceGapInBlockIsSkipped` | 块内编号空洞占位跳过、不生效 |
| `TestPoolStaticRejections` / `TestPoolFullIsDeterministic` / `TestPoolNonceGapPolicy` | 池入口校验、池满、空洞补齐 |
| `TestPackOrderingByGasPrice` | 打包确定性排序 |
| `TestConcurrentConflict` | 200 竞争者 × 20 轮：恰好 1 接受 / 199 conflict，`-race` 无竞争 |
| `TestForkAndReorg` | 分叉选择、回滚范围、重放范围、余额/nonce/收据自洽 |
| `TestReorgRejectsInvalidTxDeterministically` | 重放中伪造签名交易被拒绝、不生效 |
| `TestDuplicateAndUnknownParent` | 重复块 / 孤块显式拒绝 |
| `TestReorgConvergesWithDirectForkExecution` | 深度回滚重放与分叉点直接执行指纹一致 |
| `TestCrossEntryConsistency` | 三入口（池/单笔/区块）结论一致 |
| `TestProduceBlockAndPoolCleanup` / `TestReceiveBlockReorgResetsPool` | 出块清理、重组后池基准重置 |

测试日志使用 `-v` 时会打印分支选择依据、回滚/重放范围、每笔交易的拒绝原因等
关键判定信息，便于审计与复现。

## 说明

本项目的“签名”为密钥副本比对、哈希为 FNV-1a，仅用于演示判定流程，
**不具备真实密码学安全性**，不可用于任何生产场景。
