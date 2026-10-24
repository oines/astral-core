# astral-core

An embeddable Go SDK for durable agent sessions.

`astral-core` 把输入接收、模型调用、工具执行、审批、历史分支和恢复集中到一个 `Session`。Host 提供模型、Store、TokenEstimator 和工具，并管理业务资源与执行所有权。Memory 与 SQLite 使用同一个执行实现。

## 快速开始

需要 Go 1.24+。以下示例使用离线模型，运行时不需要模型账号：

```sh
go run ./examples/basic
go run ./examples/approval
go run ./examples/fork
go run ./examples/endrun
# 以下两个示例使用真实临时目录和 Unix Shell / PTY：
go run ./examples/tools
go run ./examples/environments
```

接入自己的模型和工具：

```go
import (
    "context"
    core "github.com/oines/astral-core"
    "github.com/oines/astral-core/store/sqlite"
)

store, err := sqlite.Open("history.db")
if err != nil { return err }
defer store.Close()

config := core.Config{
    Model: model,
    Store: store,
    EstimateTokens: estimateTokens,
    Tools: tools,
}
chat, err := core.NewSession(ctx, config, core.SessionOptions{ID: "conversation"})
if err != nil { return err }
defer chat.Close(context.Background())

submission, err := chat.Prompt(ctx, core.Input{
    ID: "request-001", // 重试同一提交时复用此 ID
    Message: core.User("检查项目并运行测试"),
})
if err != nil { return err }
if submission.Run != nil {
    result, err := submission.Run.Wait(ctx)
    if err != nil { return err }
    _ = result
}
// 重复提交返回原输入回执，Run 为 nil，不再次启动。
// 关闭旧句柄后：core.OpenSession(ctx, config, "conversation")。
```

`Model` 实现 `Spec` / `Stream`；`EstimateTokens` 计算目标模型请求的输入 Token。测试和临时会话可使用 `store/memory.New()`，依赖与执行契约相同。

## 输入与生命周期

| API | 行为 |
| --- | --- |
| `Prompt(ctx, Input)` | 空闲且工具协议完整时接收并启动；忙时返回 `ErrBusy`，不接收新输入 |
| `Steer(ctx, Input)` | 只入队，在下一完整工具批次后的安全边界优先应用 |
| `FollowUp(ctx, Input)` | 只入队，在当前任务自然结束时应用 |
| `Continue(ctx)` | 显式处理队列或恢复未完成执行；无工作返回 `ErrNotContinuable` |
| `InjectContext(ctx, Input)` | 接收被动上下文；空闲时应用并按预算自动摘要，不启动主模型循环 |
| `LookupInput(ctx, id)` | 按索引读取冻结载荷、首次接收时间与 `pending/applied/cleared` 状态 |
| `UpdateQueuedInput(ctx, id, kind, message)` | 修改尚未消费的驱动输入；空 kind 或 nil message 保留该部分 |
| `MoveQueuedInput(ctx, id, beforeID)` | 将驱动输入移到另一条之前；空 beforeID 表示队尾 |
| `ClearQueuedInput(ctx, id)` | 删除一条尚未消费的驱动输入 |
| `Cancel()` / `Run.Cancel()` | 取消执行并保留未消费输入 |
| `Compact(ctx, instructions)` | 空闲时显式压缩，不消费驱动输入、不生成普通回答 |
| `Snapshot()` / `Subscribe()` | 读取执行状态、尝试进度并订阅事件 |
| `History()` / `InspectContext()` | 读取历史与有效上下文，不启动执行或收集环境 |
| `Attempts().Get/Scan/Request` | 读取独立的模型尝试记录，或按需重建实际请求 |
| `Fork(ctx, entryID, options)` | 在完整历史边界创建共享或独立 Rollout 的 Session |

所有输入都要求稳定 ID，包括第一条 Prompt。相同 ID、相同 Digest 返回既有回执；不同 Digest 返回冲突。Host 可提供原始业务提交的 Digest，并在重新生成时间戳或读取附件前先查询回执。重试不替换已保存载荷。

队列编辑和重排复用输入 ID、首次接收时间和接收 Digest；Contexts 保持冻结。重排与载荷更新在同一提交中完成，以新队列记录的顺序为准。如果上下文准备期间选中的队列记录被更新，准备过程会根据新记录重新开始，不提交旧内容或旧顺序。已消费输入不能通过迟到的 UI 操作重新入队。Host 编辑文字时保留原消息中的附件块，无需重新读取附件来源。

`Snapshot().CanContinue` 使用与 `Continue` 相同的执行判断，覆盖队列和已开始但未完成的工作；Host 可以直接据此展示恢复操作。

输入的 `Contexts` 与 `Message` 是不可拆分的投递组。Steer 默认每次一组，`MaxSteeringPerTurn` 可设为 1–16；FollowUp 每次一组。准备开始时冻结有限选择范围；预算不够时整批不消费。取消和失败不自动重启，Host 可以显式 Continue。已接受的 Run 使用 Session 生命周期，不因提交请求的 Context 或浏览器断开而取消；`Run.Wait` 的 Context 只控制等待。

## 原子上下文准备

普通模型轮次先预览队列，并同步调用 `Config.CollectContext` 把最新环境加入完整候选，再决定是否压缩。必要时仅对已提交历史生成摘要草稿，摘要完成后重新采集环境。最终顺序为：摘要与保留历史、选中的被动上下文、最新环境、驱动输入组。

完整候选请求必须同时满足 Token 与字节预算。摘要、环境、输入、消费记录和首个 ModelAttempt 的冻结输入引用通过一次 Store Commit 确认，然后调用模型。采集失败、预算失败或提交前冲突不会改变有效上下文；摘要草稿可以在下一次激活重新计算。每次准备最多自动压缩一次，摘要请求也独立检查预算。自动压缩的近期保留目标会为新输入预留空间。

```go
config.CollectContext = func(ctx context.Context, boundary core.ContextBoundary) ([]core.Message, error) {
    // Host 负责扫描、授权和相对当前分支的内容去重。
    // boundary.CompactionID 非空时，摘要草稿已完成，应重新采集完整环境。
    return []core.Message{{
        Role: core.RoleUser,
        ContextKind: "environment",
        Content: core.Text("当前 Computer 与 CWD 的实际状态"),
    }}, nil
}
```

回调只返回消息，不修改 Core。模型和 Computer I/O 在 Session 锁及数据库事务之外执行。摘要后新环境失败时没有半完成的压缩检查点。OpenSession、历史读取和 InspectContext 不运行此回调。

## 重试与模型记录

预算默认采用 Pi 的数值：`Compaction.ReserveTokens = 16384`，输入超过 `ContextWindow - ReserveTokens` 才触发提前压缩；`KeepRecentTokens = 20000`，按完整消息/工具批次保留；摘要输出上限为预留量的 80%（默认 13107），并受摘要模型最大输出限制。`Compaction.ModelBudgets` 可按普通模型 ID 覆盖预留量和保留量。

普通输出按 `min(期望输出或模型最大输出, ContextWindow - 输入 - SafetyMargin)` 计算，`SafetyMargin` 默认 4096，不叠加到提前压缩阈值或 `MaxInputTokens` 上。没有至少一个 token 的可用输出空间时拒绝发送。`MaxInputTokens` 和字节上限仍独立检查。

输入优先使用当前投影中最近有效普通生成的实报 `InputTokens + OutputTokens`，再加后续新增消息的估算；`InputTokens` 已包含缓存输入，缓存分项不重复相加。成功 assistant 的 `Generation` 记录明确关联 Attempt、冻结请求和上下文指纹，重开/Fork 可恢复，无需扫描归档尝试。压缩、配置/模型变化或回放修复改变前缀时，回退到整份内容估算。摘要用量、失败回复、未报告或全零用量不能作为基线；旧记录缺少关联信息时也回退，直到下一次成功普通生成。

`TokenEstimator` 必须接受完整请求、空请求和不带 System/Tools/Options 的消息尾段；后两者只用于估算，不发送给模型。可选的 `RequestValidator` 在最终输出长度确定后校验完整协议与参数，在输入和摘要提交之前失败。完整请求与尾段使用同一套内容估算，尾段减去空请求的固定开销。

普通生成和摘要共用一套尝试策略：默认总共 3 次，包含首次；从 1 秒指数退避到 5 秒，较长的有效 Retry-After 优先；每次请求超时 4 分钟。明确永久错误和用户取消直接停止。网络重试复用冻结请求，不重新消费输入、扫描环境、摘要或执行工具。

`ModelRequestRecoverer` 可以对明确拒绝的 Provider 回放状态修复一次请求副本，占用同一个总尝试预算，并重新检查协议及 Token/字节预算。没有自动模型 fallback。

提供商明确报告 `ModelError.ContextOverflow` 时，Core 在完整边界上最多再压缩并生成一次；不走原样 HTTP 重试。此次恢复不接收新队列输入，不重跑已完成的工具；第二次溢出直接停止。关闭自动压缩时也关闭该恢复。

每次外部调用前保存独立的 `ModelAttempt`。`RequestInput` 固定 Rollout、祖先路径 Head、Configuration、模型规格、消息选择和必要的准备/修复差异；完整 Request 只在内存中组装，不按轮次复制图片、历史消息或 opaque reasoning。`Attempts().Request` 只读取固定边界内的有效上下文并核对内容摘要，不重新采集环境或调用模型。

成功回复与 Attempt 完成通过一次提交确认，正文只存在于被采用的消息节点；Attempt 保存结果节点引用和用量。失败片段与未采用的摘要只存在于 Attempt，采用摘要时原子移入 Compaction 节点。失败、取消和 Length 回复不会触发工具，也不会进入后续普通请求或摘要。

进程在输入提交后、回复提交前退出，重开保留未完成 Attempt。显式 Continue 创建关联到原尝试的新 Attempt，复用同一份冻结输入；已经确认的回复不会重新生成。远端请求可能已执行，不能保证网络调用恰好一次。正常失败或明确取消会结束这次生成，之后 Continue 可应用排队输入与新配置。

事件和 Snapshot 提供 Run / Turn / Attempt ID、用途、当前次数、上限、下一次重试时间与安全错误代码。客户端按 Attempt 隔离临时输出；重连只恢复事件流和快照，不重发模型请求。

## 工具、审批与恢复

`Tool.Execute` / `ToolFunc.Run` 返回 `(ToolExecution, error)`。普通结果使用 `Result: &ToolResult{...}`。固定工具声明支持 JSON Schema、串行批次、并行上限、调用前审批和结果处理。可选标准工具为 `read`、`write`、`edit`、`exec_command`、`write_stdin`，详见 [标准工具](docs/standardtools.md)。

`TargetedTool.Bind` 产生 Host 私有的 `Invocation.Binding`，Core 在执行前保存并在恢复时复用。切换 Computer 不改变已绑定调用、旧进程或历史文件的目标。

工具依次记录 prepared、authorized、started、completed 和结果。已 started 但无法确认结果的操作必须由 Host 核对；Core 不重做可能有副作用的操作。审批决定、已完成的结果和未知效果记录可跨重启恢复。

声明 `MayEndRun` 的工具可以返回 `EndRun`，在同一提交中记录控制事实与 Run 完成。它不产生普通 ToolResult，模型投影省略完整控制 Assistant。Core 随后为已经排队的独立输入启动新的 Run；错误或取消不会触发这一步。Host 的 `no_reply` 示例见 [endrun](examples/endrun/main.go)。

Assistant 的 `Content` 按原顺序保存 Reasoning、Text、ToolCall、多模态及 Provider 扩展。并行工具可以乱序完成，下一请求仍按调用顺序排列结果。流式 delta 的 `content_index` 必须对应最终回复；完整回复提交后才派发工具。

## 存储与所有权

当前 Core 格式为 **10**，只支持本格式。没有旧接口、旧格式读取或运行时自动升级；已有旧数据库返回 `ErrUnsupportedFormat`。一次性数据维护在运行库之外完成。SQLite 是持久运行后端，Memory 用于临时会话；Host 可实现同一 Store 契约接入其他后端。不提供 JSONL 运行存储，Host 仍可导出 JSONL 历史。

历史树通过 ParentID 表达分支，Session 持有执行游标、待办和当前未完成 Attempt；三者具有独立身份。普通读取只分页加载所选祖先路径的有效上下文及必要引用，内存不随归档历史总长度增长。Shared Fork 共享祖先节点，Detached Fork 分页复制所选祖先路径后原子发布；两种 Fork 都不继承父 Session 的 Attempt，消息、摘要和用量元数据不依赖父执行记录才能读取。

Host 对每个 Session ID 保留一个活动句柄，并保证进程间执行所有权。Store 的 CAS 不能撤销已发生的外部副作用。Close 取消并等待 Session 的执行、维护与 Fork，释放缓存；Model、Store 和工具环境仍由 Host 关闭。Fork 复制或共享历史，不复制工作文件或活动进程。

更多实现约束见 [宿主契约](docs/contracts.md)，验证入口见 [开发与验证](docs/development.md)。[性能报告](docs/benchmarks.md) 保留早期版本的历史测量，不代表格式 10 的最新性能结果。
