# 宿主和适配器契约

## Core 与持久 Session

`astral.New(Config)` 创建一个独立执行器，Model 和可选 Tools 足够启动。默认 Driver 保存进程内消息、工具阶段和输入队列；它没有重启恢复、持久审批或自动压缩能力。

`session.New(Config)` 创建 Manager。每个 Session 用 `sessionDriver` 组合同一个 Core，通过 `Snapshot/Prepare/Commit/Boundary/Finish/AcceptResult` 接入状态、上下文策略和提交确认；没有第二套 Agent Loop。普通生成与摘要使用同一个内部 Retry / Backoff 实现。

Core 的 `Driver.Commit` 必须在事实被采用、依赖动作开始之前确认成功。`Boundary` 负责在同一串行边界内消费输入或完成 Run；自定义 Driver 不得把队列为空检查与完成动作拆开。事件只用于观察，不能代替提交确认。

## 所有权与生命周期

一个 Manager 内，同一 ID 的 OpenSession 返回同一已打开对象。一个 Session 同时只能有一个 Run 或独占维护操作，冲突返回 `astral.ErrBusy`。Compact、恢复预检、环境重新发现和效果确认属于维护操作。只读 Fork 不占用 Parent 的维护状态。

宿主必须保证跨 Manager、进程和主机的独占执行所有权。Store CAS 只能拒绝冲突写入，不能撤销外部操作；SDK 不实现分布式租约或调度。

- Agent.Close 取消并等待自己的 Run，关闭订阅，释放默认 Driver 的消息和队列。
- Session.Close 取消并等待 Run、维护与 Fork，释放 Context、移出 Manager 缓存；持久历史仍可通过 HistoryReader 读取或重新打开。
- Manager.Close 取消并等待已加载的 Session，以及正在进行的新建和打开操作。
- 三者都不关闭宿主拥有的 Model、Store、Runtime、Backend 或 ToolSet。宿主先停止执行，再关闭这些依赖。

宿主回调必须响应 Context，Go 不能强制终止不配合取消的回调。Close 的 Context 限制等待时间，超时后可以再次调用。`Run.Wait(ctx)` 仅停止等待；`Run.Cancel()` 才取消运行。

## Store 格式与提交

Store 位于 session 包：

```go
type Store interface {
    LoadState(context.Context, string) (RuntimeState, error)
    Get(context.Context, string, string) (Record, error)
    Scan(context.Context, string, Query) (Page, error)
    Pending(context.Context, string, PendingQuery) (Page, error)
    Commit(context.Context, string, Commit) (Receipt, error)
    LookupCommit(context.Context, string, string) (Receipt, error)
    Fork(context.Context, ForkRequest) (RuntimeState, error)
}
```

格式版本为 2。旧 API 和旧日志没有兼容层或自动迁移；不支持的格式返回 `ErrUnsupportedFormat`，不改写原文件。

Commit 包含稳定 ID、ExpectedVersion、已分配标识 / Sequence / Version 的 Entry，以及选择历史节点时的 Projection。初始版本为 0；一次非空原子批次把版本加 1。Entry ID 在 Session 内唯一，提交后不可变。Sequence 在一个会话中单调递增，Fork 复制掉操作记录后允许出现空隙。

一个成功 Commit 原子保存：

1. 新 Entry 与历史节点的 branch Projection。
2. 通过 `session.Reduce` 计算的精确 RuntimeState。
3. 历史、父子、待办等索引更新。
4. Commit ID、版本和内容摘要组成的 Receipt。

所有读取返回隔离副本。不存在返回 ErrNotFound，版本不符返回 ErrConflict。同一 Commit ID、相同内容的重试返回原始回执；同一 ID 改变内容是冲突。State 更新和 JSONL 回放使用同一个 Reduce；未确认的提交不能推进 Session 内存状态。

提交返回错误时，Session 使用独立、最多 5 秒的确认 Context 调用 LookupCommit，核对 Commit ID、摘要、提交版本以及当前版本。只有完全匹配才继续。即使目标提交存在，当前版本已被其他所有者推进也会停止。核对不读取完整历史。宿主重新建立独占所有权后，才可调用空闲 Session 的 Reload。

持久 Store 成功确认后必须能在重新打开时读到数据。Backend 的 WAL / fsync 保证仍依赖操作系统和文件系统。Memory 提供相同的原子性与隔离，但没有进程退出后的持久性。

### SQLite

`store/sqlite` 使用 Go 1.23 兼容的纯 Go modernc 驱动，不需要 CGO。连接使用 WAL、synchronous=FULL、busy_timeout=5000，并设置有限页缓存。一个 Commit 的记录、状态、索引、回执在同一事务里提交。

entries 按 Session / ID、Session / Sequence、父节点、类型 / 引用建立索引；pending 为待处理输入及审批建立索引。当前状态单独存储。普通 Commit 处理新增批次和当前工作状态，不回放或复制全部历史。历史路径通过受限的递归查询、固定连接顺序和主键查找读取；查询计划测试防止退化为全 Session 扫描。

`Stats().RowsRead` 统计实际返回给适配器的记录 / 状态行，不能单独代表 SQLite 内部扫描次数；需要结合查询计划和实测耗时判断。

### JSONL 与 Memory

JSONL 使用 Session ID 的 SHA-256 文件名、版本头和带 SHA-256 校验和的批次。提交同步文件和目录后确认。首次访问日志回放并建立内存索引；以后只检查新增尾部，追加不重新校验已加载历史。

未以换行结束的最后一行视为未提交尾部，读取忽略它，下一次 Commit 仅截去这段。完整行损坏、校验和失败或非法状态转换返回 ErrCorrupt，不自动修复已提交历史。确认恢复在采用新读到的完整提交前执行同步。

同进程打开同一规范化目录的多个句柄共享缓存和写锁；跨进程必须由宿主独占目录。Close 由宿主调用。Memory 和 JSONL 将历史索引保存在内存中，不提供 SQLite 的长历史内存保证。

## 精确状态、Context 与预算

RuntimeState 包含版本、Sequence、创建信息、Head、有效配置 / 环境 / 摘要引用、当前批次执行事实、当前审批和待办数量。队列正文留在 Store 中，按索引分页读取；旧批次不保存在运行状态中。

每个历史节点保存 Projection，SelectHead 通过索引取得所选节点的分支状态，不继承兄弟分支的运行事实。Session 保留有效 Context 与必要引用，没有完整历史树缓存。任意旧节点若没有摘要且有效路径超限，选择或打开会明确报预算错误。

`MaxContextBytes` 默认 32 MiB，最小 4096，约束准备发送的 Request。Context 记录缓存还容纳信封、配置和当前批次，使用约两倍预算的保护上限。Token 估算与字节限制独立；模型上限仍由 EstimateTokens、输出预留和安全余量共同检查。当前工作状态、调用者持有的副本和 Backend 内存不包含在 Request 字节限制中。

模型摘要是 `CompactionRecord`；精确恢复点是 RuntimeState，二者不能互相替代。压缩选择完整消息 / 工具批次，先尝试不裁剪的摘要输入；仅在摘要模型预算不足时缩减输入副本中的工具文本。原始历史、调用标识和结果状态保留。巨大用户输入无法容纳时返回 ErrCompactionInputTooLarge。

摘要成功后，把摘要、读取边界和必要的完整环境重申原子提交，再释放旧 Context。每次请求准备最多自动压缩一次，避免无进展循环。失败摘要不会提交有效摘要；Attempt 诊断只通过可选实时事件提供，不强制写日志。成功回复及摘要保留 Usage。

## Model 与协议

Model 只包含 Spec / Stream；TokenEstimator 是 Session 的独立依赖。Spec 的 ID 必须稳定，窗口和输出 Token 限额为正。ResolveModel 返回的 ID 必须与请求一致。

适配器必须：

1. 保留有序 Content、Tool Call ID、参数、Reasoning、多模态部分及消息 / 内容 / 调用的 opaque Metadata。不支持时明确报错。
2. 保留 RoleContext 的位置和顺序，将其映射为模型可见上下文，不能把文件正文提升为 System 指令。
3. Stream 只返回一次完整 Reply，准确标记 FinishReason。length / error / cancelled 不作为成功回复提交。
4. 返回前停止调用 emit。Delta 是展示数据，最终 Reply 才可提交。
5. 通过 ModelError 的 Transient、RetryAfter、Code、Cause 分类错误。
6. 为 Session 提供目标模型实际输入的 Token 估算，包括工具 Schema、消息包装、图片等协议开销。

每次 Retry 产生新 Attempt ID，同一 Turn ID 保持不变。临时错误退避重试；请求超时可重试，父 Context 取消则停止。Fallback 由宿主选择目标模型以及是否持久改变首选模型，重新准备请求与预算。SelectModel 不截断已开始的 Stream；若发生在自动摘要期间，正式生成前重新检查新模型的预算。

SDK 保存协议数据，不包含真实 Chat Completions / Responses 转换器或 Provider 兼容性认证。

## 工具阶段、结果和恢复

工具声明在构造时冻结并验证 JSON Schema。默认 Draft 2020-12，允许声明内引用，拒绝外部 URL / 文件引用。恢复后实际使用的工具声明及 Version 必须与宿主实现一致。

```text
参数校验 → 目标绑定 → prepared → before hook → authorized
→ started 提交确认 → Execute → completed 提交确认
→ after hook → 交付预算 → result + 环境变更原子提交
```

prepared 固定 Invocation 与目标；authorized 固定 Hook 决定；started 在派发前确认。completed 保存 Outcome、Artifact 和上下文变更等执行事实，不存储未处理的大输出。无效参数、Skip、Block 可直接产生 not_executed 结果。

Core Invocation 只包含执行标识、参数及不可变 opaque Binding。TargetedTool 可在审批前固定目标。可选 environment 包负责解释 Binding，Core 不依赖该类型。工具产生的 Context 更新是不可被结果 Hook 改写的执行事实；需要声明 Sequential。

并行批次受 MaxParallelTools 限制。任一工具声明 Sequential，则整批按调用顺序执行。结果可乱序完成，但下一次 Request 按原 ToolCalls 顺序形成完整批次。工具与 Hook 必须支持并发，返回后不得继续调用 emit。

Outcome 表达执行效果；IsError / Code 也可表达交付失败。普通错误默认 failed，不保证没有部分效果。已知未派发用 OutcomeNotExecuted；确认丢失、取消或未知部分效果用 OutcomeUnknown。Core 不重试工具。超大输出变为 result_too_large，保留真实 Outcome 与 Artifact，清除超大的 Details；原始大输出由宿主管理。

| 最近可靠事实 | Continue 行为 |
| --- | --- |
| prepared / authorized，无 started | 复用目标与已存决定，继续执行 |
| started，无可靠终态 | 追加 outcome_unknown，返回 ErrOutcomeUnknown，不再派发 |
| completed，无最终结果 | 追加 result_unavailable，保留效果，不重跑 Tool 或 after hook |
| 已有最终结果 | 直接复用 |

宿主通过 UnresolvedTools 查询真实外部状态，再用 ResolveTool 追加确认结果；原记录保留，结果仍受交付预算约束。宿主也可显式接受仍未知的效果。ExecutionID 可作为业务幂等键，但这不是外部事务的 exactly-once 承诺。

## Hook、审批、输入和事件

Session 的 BeforeToolCall 接收 ToolContext：

```go
BeforeToolCall: func(ctx context.Context, inv session.ToolContext) (astral.ToolDecision, error) {
    decision, err := inv.AwaitApproval(ctx, session.ApprovalRequest{Prompt: "允许执行吗？"})
    return astral.ToolDecision{Block: !decision.Approved, Reason: decision.Reason}, err
},
```

每个调用支持一个稳定审批门，请求和决定都持久化。authorized 提交前 Hook 可能重入，审批门外的逻辑必须可重复，不应执行工具副作用。重复提交同一审批决定幂等，冲突决定返回 ErrAlreadyResolved。Standalone Core 的 Hook 接收普通 Invocation，审批策略由宿主自行提供。

AfterToolCall 返回 ToolResultPatch；nil 保留原字段，布尔指针允许设置 false。可改交付内容、Details、IsError、Code、Artifact、Terminate，不能改实际 Outcome 和 Context 效果。已 completed 的恢复不会重跑 after hook。

Steer 在完整回复或整批工具之后优先消费；FollowUp 在自然完成时消费。消费与用户消息同批提交。只有整批结果都要求 Terminate 才停止自动续问，排队输入仍会处理。收尾与队列检查共享锁，收尾之后入队的输入留给下一次 Continue，不丢失。

Subscribe 为尽力投递，缓冲溢出关闭订阅并返回 ErrSlowSubscriber。先订阅，再分页读取历史，按 Entry ID 去重。Event.Sequence 只在当前句柄内递增，不是持久游标；失败 Attempt 的 Delta 不能从历史恢复。InspectContext 不生成回复、压缩或发现环境。

## 环境、历史位置与 Fork

Environment 可选。environment.Snapshot 描述宿主确认的 Runtime / Computer / Workspace、OS、Shell、cwd、能力、资源和 opaque Metadata。环境条目按发生位置进入 Context，不改写 System Prompt。

标准工具通过 environment.Decode 解释 opaque Binding。已有进程保存原始 Binding；切换当前 Runtime 或 Workspace 不会改变进程目标。审批看到的目标与执行时验证的目标必须一致。

重新打开、Reload、SelectHead 和未提供新 Environment 的 Fork 都把已有环境视为历史信息。产生新副作用前必须通过宿主 Provider 或 RefreshEnvironment 重新确认。没有环境的纯聊天 / 业务 API Session 不要求环境发现。

SelectHead 只在空闲时追加选择记录，不删除兄弟分支。原 Session 的输入队列继续保留。ForkAt 复制指定历史节点及祖先；ForkBefore 仅接受用户消息，复制至它的父节点。ID 在 Session 内唯一，复制保留历史 ID / ParentID / Sequence 及协议引用，复制条目的 Version 归为 Child 的初始版本；Child 有新创建记录和独立后续提交。

运行中 Fork 只接受已提交、协议完整的节点。短暂固定 SourceVersion 和 Head 后在 Parent 锁外复制；不改变 Parent 的 Head、队列、环境、取消状态或执行路径。空闲时可以复制部分批次供检查，但任何不完整历史批次都不能自动进入执行。

Fork 不复制 Run、审批或输入队列，不共享父历史存储。复制时间与磁盘成本为 O(K)。SQLite 每页短事务写 ready=0 的隐藏目标，完成后原子发布；JSONL 写临时文件、同步并原子发布。失败、取消、进程崩溃不能暴露半成品。

SQLite 崩溃可能留下隐藏目标，宿主可在确认没有同名活动 Fork 后调用 DiscardUnpublished；该方法拒绝已发布 Session。JSONL 的崩溃临时文件也不会作为会话打开，由宿主按生命周期清理。没有后台 GC 或共享前缀回收机制。
