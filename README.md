# 简化链的账本与出块流程（含分叉重组与并发交易池）

一个用**本地状态**模拟的简化区块链账本：账户模型 + nonce 交易、确定性状态转移、
基于累计权重的分叉选择与链重组、线程安全的待打包交易池。所有关键判定（分支选择、
回滚范围、拒绝原因）都有结构化结果与日志输出，保证不同入口下结论一致且可解释。

## 包结构

| 文件 | 职责 |
| --- | --- |
| `types.go` | `Address/Transaction/Receipt/Block` 类型；交易规范化哈希与区块哈希 |
| `crypto.go` | ed25519 账户、签名/验签；地址 = `sha256(pub)[:20]` |
| `state.go` | 账户状态库：单笔/批量状态转移、失败原因、快照恢复、反向转移、状态根 |
| `chain.go` | 分叉存储、区块验证、累计权重分支选择、回滚 + 重放重组 |
| `pool.go` | 并发安全交易池：ready/future 队列、容量与 nonce 空洞策略、确定性选择 |
| `node.go` | 出块入口：选交易 → 预执行过滤 → 构块 → 上链 → 交易池同步 |

## 1. 状态转移规则

### 1.1 交易与身份

- 交易字段：`From, To, Nonce, Amount, Fee, Pub(32B 公钥), Signature(64B ed25519 签名)`。
- 签名内容是交易的**规范化哈希** `sha256(From‖To‖Nonce‖Amount‖Fee)`；
  验签同时要求 `AddressFromPub(Pub) == From`，因此交易自包含、不可篡改。
- 任何字段被篡改都会导致 `ErrInvalidSig`。

### 1.2 校验顺序与可区分的失败原因

`StateDB.Apply` 严格按以下顺序校验，失败时返回**不修改任何状态**的失败收据：

1. 签名无效 / 身份不匹配 → `ErrInvalidSig`
2. `nonce < 账户当前 nonce`（重放、重复打包）→ `ErrDuplicateTx`
3. `nonce > 账户当前 nonce`（编号乱序 / 空洞）→ `ErrInvalidNonce`
4. `balance < amount + fee`（含加法溢出保护）→ `ErrInsufficientBal`

成功转移：`from -= amount+fee`、`from.nonce++`、`to += amount`、`coinbase += fee`。

### 1.3 同批交易与打包顺序无关（确定性）

- `StateDB.ApplyBlock` 与区块验证都强制交易按规范键排序：
  `(From 地址, Nonce, To 地址, Amount, TxHash)` 升序。
- 同一批交易无论以什么切片顺序提交，执行序列完全相同，最终状态根一致。
  见 `TestStateTransitionDeterminism`（5 种随机洗牌得到相同状态根）。
- 直接提交给链的区块若交易不是规范顺序，会被 `ErrInvalidBlock` 拒绝，
  从协议层杜绝“同批不同序”的歧义。
- 失败交易**不允许被静默打包进区块**：构块时在状态快照上预执行过滤，
  直连接口若包含执行失败的交易会整体拒绝该块。

### 1.4 状态根与可回滚性

- 状态根 = 账户按地址排序后对 `地址‖nonce‖余额` 序列求 sha256。
- 每笔成功转移都有精确的逆操作（`StateDB.reverse`）：退收款、退手续费、
  nonce 回退、余额恢复；归零账户条目会被删除，使状态表示与交易前逐字节一致。

## 2. 分叉与链重组

### 2.1 区块验证（`Chain.AcceptBlock`）

- 父块必须存在，否则 `ErrOrphan`；高度必须 = 父高度 + 1；区块哈希必须自洽。
- 在**父块状态快照**上按规范顺序重放全部交易，全部必须成功；
  计算状态根并与 `StateRoot` 比对，不一致 → `ErrStateRootMismatch`。
- 验证通过后，区块及其状态快照、收据被保存（分叉块也保留，可按哈希查询历史）。

### 2.2 分支选择

- 权重规则：沿父链累加各块 `Weight`（简化版 PoW 累计权重）。
- 仅当新分支累计权重**严格大于**当前规范链时才切换；**权重相等保留旧链**
  （先见者确定规则），保证结论与时序无关。比较过程有 `branch compare` 日志。

### 2.3 回滚 + 重放（`reorgLocked`，持链锁原子完成）

1. 求新旧 tip 的共同祖先；
2. 从旧 tip 向下逐块、块内交易倒序执行**反向状态转移**；
3. **不变量校验**：回滚后的状态根必须与共同祖先快照完全一致，
   不一致直接 `REORG INVARIANT BROKEN`（杜绝重复扣款/漏扣）；
4. 在祖先快照上重新正向执行新链区块，逐块比对状态根与既有快照；
5. 原子切换 tip 与状态。

`ReorgResult` 明确给出：是否重组、共同祖先、回滚区块（高→低）、
重放区块（低→高）、新旧 tip。重组日志会打印每块的回滚/重放范围。

- 两条分支包含同一笔交易时，该交易在新链上仍只生效一次
  （`TestReorgSharedTxAppliedOnce`）。
- 被回滚但不在新链上的交易不会静默消失：`Pool.OnReorg` 会把它们重新提交入池，
  并在新链状态上重新校验（无效则记录丢弃原因）。
- 旧分叉的区块与收据仍可按区块哈希查询；规范链交易收据通过 `TxReceipt` 查询。

## 3. 并发交易池与打包集合

`Pool` 用单把 `sync.Mutex` 保护全部内部 map，无数据竞争（`go test -race` 验证）。

### 3.1 队列与编号空洞

- **ready**：nonce 从“链上 nonce + 已排队连续 nonce”开始连续的交易，可打包。
- **future**：nonce 超前（存在编号空洞）的交易，有限容量暂存；
  空洞被补齐时自动级联晋升（`promoteLocked`）。空洞交易绝不会提前打包。
- 下一笔期望 nonce 的计算同时考虑链上状态与 ready 队列
  （`expectedNonceLocked`），因此乱序并发到达时行为确定。

### 3.2 去重、冲突与容量（确定且可解释，绝不静默丢弃）

- 全池按交易哈希去重：重复提交 → `ErrDuplicateTx`。
- 同 `(发送方, nonce)` 的另一笔交易（双花冲突）→ `ErrDuplicateTx`。
  高并发下恰好 1 笔入池，其余全部拿到相同拒绝原因（`TestPoolConcurrent`，32 goroutine）。
- ready 满 → `ErrPoolFull`；future 满 → `ErrFutureFull`。
- 签名问题 → `ErrInvalidSig`；空交易 → `ErrMalformedTx`。

### 3.3 确定性打包集合（`Select`）

- 排序键 `(From, Nonce, TxHash)` 升序，取前 `max` 笔；
  结果只取决于池内集合，与提交顺序/并发时序无关
  （`TestPoolSelectDeterministic`：4 种提交顺序选出完全相同的序列）。
- ready 中每个发送方 nonce 连续，因此选出的集合天然不存在同账户冲突。

### 3.4 出块流程（`Node.ProduceBlock`）

`Select` → 在链状态快照上逐笔预执行（重组后可能失效的交易在此被剔除并记录原因）
→ 按规范顺序写块、算状态根 → `AcceptBlock` → 移除已打包交易 →
若触发重组则执行 `OnReorg` 同步交易池。

## 4. 验证方法

```bash
go build ./...
go vet ./...
go test ./...                 # 全部单测（日志打印关键判定依据）
go test -race ./...           # 数据竞争检测
go test -count=N ./...        # 重复运行确认稳定复现
go test -v -run TestReorg ./...        # 只看重组场景
go test -v -run TestPoolConcurrent ./... # 只看并发冲突场景
go test -run Example .                 # 端到端可运行示例
```

### 测试覆盖一览

| 测试 | 覆盖点 |
| --- | --- |
| `TestStateTransitionDeterminism` | 同批交易任意洗牌，状态根一致 |
| `TestRejectReasons` | 四种可区分失败原因；失败不改状态；最终余额/nonce 自洽 |
| `TestFailedTxDoesNotMutateState` | 非法交易零副作用 |
| `TestReorgRollbackAndReplay` | 2 块旧链回滚、3 块分叉重放；余额/手续费/nonce/收据全部自洽 |
| `TestReorgSharedTxAppliedOnce` | 两分支共享交易重组后只扣款一次 |
| `TestNoReorgOnEqualWeight` | 等权重不切换（确定性先见规则） |
| `TestRejectInvalidBlocks` | 孤儿块 / 错误状态根 / 错误高度 / 非规范交易顺序 |
| `TestPoolRejectReasons` | 签名、重复、冲突、池满、future 满、空洞不打包 |
| `TestPoolSelectDeterministic` | 选择结果与提交顺序无关 |
| `TestPoolConcurrent` | 32 goroutine 冲突提交：唯一赢家 + 无重复打包（-race） |
| `TestPoolNonceGapPromotion` | nonce 空洞暂存与补齐级联晋升 |
| `TestNodeProduceAndReorgIntegration` | 并发提交→出块→重组→回滚交易回收→再次打包，端到端自洽 |

所有测试日志通过 `testing.T.Log` 输出，使用 `go test -v` 可看到
分支权重比较、回滚/重放区块范围、每笔交易的拒绝或入队原因等关键判定依据。
