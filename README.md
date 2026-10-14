# astral-core

可嵌入 Go 项目的 Agent SDK。`astral` 提供独立执行器；`session` 在同一执行器上增加持久历史、恢复、审批、上下文预算和压缩。仅提供 Model 和可选 Tools 即可运行 Core。

## 包与边界

| 包 | 职责 |
| --- | --- |
| `github.com/oines/astral-core`，包名 `astral` | 唯一 Agent Loop、Model / Tool 协议、Retry / Fallback、Run、取消、输入边界和事件 |
| `session` | Manager、Session、审批、分页历史、Context、预算、摘要、精确状态与恢复、Store 契约 |
| `environment` | 可选 Snapshot、Provider、Binding，以及 opaque 绑定的编解码 |
| `store/memory` | 进程内存储，适合测试与临时会话 |
| `store/jsonl` | 版本化校验日志、原子提交与同步落盘；首次打开回放历史 |
| `store/sqlite` | 纯 Go SQLite、WAL、完整同步、索引、增量状态和分页 Fork |
| `standardtools`、`standardtools/local` | 可选文件、命令、进程会话和本地 Shell / PTY 后端 |

Core 不导入 Session、Store、Environment、Token Estimator 或 Provider SDK。Runtime 连接、Workspace、凭据、权限和 UI 由宿主管理；不内置 Subagent / Team / Workflow 框架。

## 独立 Core

Model 只实现 `Spec` 和 `Stream`。`ModelFunc`、`ToolFunc` 可直接连接现有函数：

```go
agent, err := astral.New(astral.Config{
    Model: hostModel,
    Tools: []astral.Tool{lookupOrder},
})
if err != nil { return err }
defer agent.Close(context.Background())

run, err := agent.Start(ctx, astral.User("查询订单 order-42"))
if err != nil { return err }
result, err := run.Wait(ctx)
```

默认 Driver 保存进程内消息和待处理输入，提供进程内执行语义，没有持久化或重启恢复保证。`Start`、`Continue`、`Steer`、`FollowUp`、`Cancel`、`Subscribe` 可直接在 Agent 上使用。可运行代码见 [basic](examples/basic/main.go)。

## 持久 Session

```go
store, err := sqlite.Open("astral.db")
if err != nil { return err }
defer store.Close() // 宿主拥有 Store

manager, err := session.New(session.Config{
    Model: hostModel,
    Tools: []astral.Tool{lookupOrder},
    Store: store,
    EstimateTokens: hostEstimateTokens,
})
if err != nil { return err }
defer manager.Close(context.Background())

conversation, err := manager.NewSession(ctx, session.SessionOptions{ID: "order-support"})
if err != nil { return err }
run, err := conversation.Start(ctx, astral.User("查询订单 order-42"))
if err != nil { return err }
result, err := run.Wait(ctx)
```

`EstimateTokens` 是独立依赖，签名为 `func(context.Context, astral.Model, astral.Request) (int, error)`，计算目标模型的实际输入开销。下次通过 `manager.OpenSession(ctx, id)` 打开。Memory、JSONL、SQLite 遵守同一提交契约，只有持久后端保证进程重启后保留数据。

Manager 缓存已打开的 Session。`Session.Close` 取消并等待自身工作、释放 Context、移出缓存，之后可以重新打开。`Agent.Close`、`Session.Close`、`Manager.Close` 都不会关闭宿主的 Model、Store、Runtime 或 ToolSet。

## 常用 Session API

| 需求 | API |
| --- | --- |
| 新输入 / 恢复工作 | `Start` / `Continue` |
| 下一安全边界 / 当前工作完成后输入 | `Steer` / `FollowUp` |
| 停止与等待 | `run.Cancel()`、`conversation.Cancel()`、`run.Wait(ctx)` |
| 实时输出 | `Subscribe(buffer)` |
| 分页历史 | `History().Scan`、`Get`、`Children`、`Path` |
| 有效请求 / 精确状态 / 保留规模 | `InspectContext(ctx)` / `State()` / `Stats()` |
| 压缩 / 配置 / 模型 | `Compact` / `Configure` / `SelectModel` |
| 当前执行位置 / 历史所属 Rollout | `Head()` / `RolloutID()` |
| 从历史另起执行 | `Fork(ctx, nodeID, session.ForkOptions{ID: "child"})` |
| 审批 | `PendingApprovals` / `DecideApproval` |
| 确认未知工具效果 | `UnresolvedTools` / `ResolveTool` |

History 面向整个 Rollout，包含兄弟 Session 的历史。`Query.SessionID` 筛选写入者，`Kind` / `HistoryOnly` 筛选记录类型；恢复和待办始终按 Session 隔离。历史接口需要 Context。默认每页 256 条、4 MiB；把 `NextCursor` 放入下一次 `Query.Cursor` 继续。`Path` 从 Head 向祖先倒序读取；没有全量 `Rollout/Entries/Tree` 或 `Branch` 同义入口。`Wait` 的 Context 只限制等待，`Start/Continue` 的 Context 控制 Run 生命周期。

## 执行保证

两种入口共用模型生成、工具调度、Retry、取消和公共事件。Session 为相同执行阶段提供持久确认：

```text
参数校验 → 固定目标 → prepared → 审批 / Hook → authorized
→ started 确认 → Execute → completed 确认 → 结果 Hook
→ 交付预算检查 → 最终结果与环境变更原子提交
```

- 失败或截断的模型回复不会执行工具。Retry 只针对模型；Fallback 重新准备目标模型的请求和预算。
- 稳定 Commit ID、预期版本和内容摘要用于恢复丢失的确认。确认未知或另一所有者已推进版本时，不继续依赖该提交的副作用。
- 已 started 但结果未知的工具阻止自动重跑。已 completed 但缺少最终结果时追加 `result_unavailable`，不重跑工具或结果 Hook。
- 输入消费与用户消息原子提交；队列检查和 Run 收尾共用串行边界。工具结果按原调用顺序进入模型请求。
- 事件非阻塞。慢订阅者溢出后关闭，持久客户端通过分页历史重建已提交内容。

宿主必须保证同一 Session 只有一个执行所有者；CAS 不代替分布式执行租约。JSONL 目录要求单进程所有权。

## 长会话与 Fork

Session 只保存当前 Context、精确 RuntimeState 和当前批次的工具事实，队列正文通过索引读取。模型摘要与运行 Checkpoint 分离。SQLite 普通 Commit 只处理新增批次；OpenSession 只加载当前状态、有效窗口及必要引用；请求准备使用缓存窗口。压缩提交前移读取边界并释放旧 Context。

**Rollout owns history. Session owns execution.** NewSession 创建新 Rollout；Rollout 是历史的存储命名空间和未来整体删除单位。Entry 通过 ParentID 形成 Rollout 内的树，每个 Session 拥有独立 Head、Version、队列、审批和恢复状态。

```go
// 默认 Shared：同一 Rollout 中创建新的执行状态，共享已有历史。
child, err := parent.Fork(ctx, nodeID, session.ForkOptions{ID: "edit-1"})
// Detached：复制所选路径，建立不依赖源 Rollout 的历史与执行状态。
copy, err := parent.Fork(ctx, nodeID, session.ForkOptions{
    ID: "independent-1", Mode: session.ForkDetached,
})
```

目标 ID 必填，也是创建幂等身份。边界包含 nodeID；若要排除某条消息，传入其 ParentID。同 ID、同参数重试返回已有 Child，包括已经推进的 Child；参数不同返回 ErrConflict。浏览用 History，恢复执行用 OpenSession，从历史另起执行用 Fork。

运行中的 Parent 可以从较早的合法节点 Fork。Child 不继承父 Session 的授权、运行、审批或队列。完整工具批次的历史结果可继续使用，不重跑工具和 Hook，并保留终止语义；不完整批次或未解决效果在创建前拒绝。环境与 Context 预算仍需校验。

Shared 仅新增 Session 和创建记录，成本取决于有效 Context 和必要索引读取。Detached 按路径复制，时间、磁盘与累计分配随复制量增长。SQLite 以分页短事务构建不可见目标，JSONL 使用临时目标；两者最终原子发布，未完成复制在核对身份后清理重试。Parent 的执行状态不受影响，Child 的版本从自身创建批次独立开始。

Close 只释放执行资源，保留历史与持久状态。父子等待、取消联动、汇总、并发配额和外部资源隔离由宿主负责。本轮没有 Rollout / Session 删除 API 或 GC。

Memory 与 JSONL 的历史索引保存在内存中，不提供 SQLite 的长历史内存保证。JSONL 首次打开允许全量回放，后续 Commit 只处理新增尾部。实测数据见 [长会话 Benchmark](docs/benchmarks.md)。

## 默认策略与示例

模型默认最多尝试 3 次，退避从 250 ms 到 10 s；配置 Fallback 回调后默认最多切换一次。默认并行工具数为 8；任何顺序工具使整批串行。Core 默认输出预留与 Session 的零值输出预算都是 `min(16384, model.MaxOutputTokens)`。

Session Request 默认字节上限为 32 MiB，独立于 Token 预算，可通过 `MaxContextBytes` 配置。Context 记录缓存允许约两倍字节预算容纳信封、引用和当前批次；这不是整个进程的内存上限。默认压缩保留约 20000 Tokens 的完整消息 / 工具批次，摘要输出最多 4096 Tokens。`Compaction.Disabled` 关闭自动压缩，手动 `Compact` 仍有效。无法满足预算时明确报错。

模块要求 Go 1.23。五个示例使用确定性离线 Model，无需 API Key：

```sh
go test -race ./...
go vet ./...
go run ./examples/basic
go run ./examples/approval
go run ./examples/environments
go run ./examples/tools
go run ./examples/fork
```

[basic](examples/basic/main.go) 演示独立 Core；[approval](examples/approval/main.go) 演示持久审批和 JSONL 重开；[environments](examples/environments/main.go) 演示环境切换和进程目标固定；[tools](examples/tools/main.go) 执行真实文件及 Shell 操作。environments 和 tools 需要 Unix。[fork](examples/fork/main.go) 演示共享历史的重新生成、独立复制及父子生命周期。工具集由宿主调用 `Set.Close` 清理，详见 [标准工具](docs/standardtools.md)。

本次直接替换旧 API 和存储格式。格式版本为 3，不提供旧 API 兼容层或日志迁移；旧格式被拒绝且原文件保持不变。真实 Chat Completions / Responses 适配器、精确 Token 计数和远程 Runtime 接入留待后续。

- [宿主和适配器契约](docs/contracts.md)
- [代码导航与验证](docs/development.md)
- [第三方来源](THIRD_PARTY_NOTICES.md)
