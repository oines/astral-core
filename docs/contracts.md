# 宿主和适配器契约

## Core 与持久 Session

`astral.New(Config)` 创建一个独立执行器，Model 和可选 Tools 足够启动。默认 Driver 保存进程内消息、工具阶段和输入队列；它没有重启恢复、持久审批或自动压缩能力。

`session.New(Config)` 创建 Manager。每个 Session 用 `sessionDriver` 组合同一个 Core，通过 `Snapshot/Prepare/Commit/Boundary/Finish/AcceptResult` 接入状态、上下文策略和提交确认；没有第二套 Agent Loop。普通生成与摘要使用同一个内部 Retry / Backoff 实现。

Core 的 `Driver.Commit` 必须在事实被采用、依赖动作开始之前确认成功。`Boundary` 负责在同一串行边界内消费输入或完成 Run；自定义 Driver 不得把队列为空检查与完成动作拆开。事件只用于观察，不能代替提交确认。

## 所有权与生命周期

一个 Manager 内，同一 ID 的 OpenSession 返回同一已打开对象。一个 Session 同时只能有一个 Run 或独占维护操作，冲突返回 `astral.ErrBusy`。Compact、恢复预检、环境重新发现和效果确认属于维护操作。Fork 不占用 Parent 的维护状态，也不改变 Parent 的状态。

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

格式版本为 4。V4 将 Assistant 的工具调用放入唯一有序 Content 数组。V3 及更早格式返回 `ErrUnsupportedFormat`；检测在写入或调整日志模式前完成，不改写旧数据，不提供静默迁移或旧消息字段兼容层。

`Get` / `Scan` 的第一个字符串参数是 **RolloutID**。`LoadState` / `Pending` / `Commit` / `LookupCommit` 的第一个字符串参数是 **SessionID**；SessionID 在 Store 内唯一。

Commit 包含稳定 ID、ExpectedVersion、已有 EntryID / 写入 SessionID / 本地 Version 的 Entry；请求中的 Sequence 必须为 0。RuntimeState 的 Version 只用于该 Session 的 CAS，没有运行状态 Sequence。Entry ID 在 Rollout 内唯一，提交后不可变。Entry.Version 保留写入 Session 的提交版本；Entry.Sequence 表示 Rollout 内存储顺序。

普通提交顺序固定为：

1. Session 形成请求，不分配 Sequence，不提前 Reduce 或推进缓存。
2. Store 在原子写入范围内验证 Session CAS 和 Rollout 可写状态，分配连续 Sequence 区间。
3. 对已分配顺序的 Entry 执行 Reduce；原子保存 Entry、Projection、RuntimeState、索引和 Receipt。
4. Receipt 返回实际的 FirstSequence / LastSequence；Session 校验回执后，用 `Commit.Confirmed` 恢复同一批 Entry，再 Reduce 更新缓存。

Commit 摘要排除 Store 分配的 Sequence，其余请求内容绑定幂等身份。不同 Session 可交错提交而互不推进对方的 Version；Rollout Sequence 在交错批次间统一递增。Detached 导入保留祖先原始 Sequence，允许空隙，新提交从导入路径的最大 Sequence 后开始。

完整回放和 Checkpoint 使用持久化的已确认 Entry，不能从当前计数重建序号。Fork 的创建回执使用固定 `ForkCommitID`，摘要绑定 ForkRequest；它不属于普通 Commit 重试入口。

所有读取返回隔离副本。不存在返回 ErrNotFound，版本不符返回 ErrConflict。同一 Commit ID、相同内容的重试返回原始回执；同一 ID 改变内容是冲突。State 更新和 JSONL 回放使用同一个 Reduce；未确认的提交不能推进 Session 内存状态。

提交返回错误时，Session 使用独立、最多 5 秒的确认 Context 调用 LookupCommit，核对 Commit ID、摘要、提交版本以及当前版本。只有完全匹配才继续。即使目标提交存在，当前版本已被其他所有者推进也会停止。核对不读取完整历史。宿主重新建立独占所有权后，才可调用空闲 Session 的 Reload。

持久 Store 成功确认后必须能在重新打开时读到数据。Backend 的 WAL / fsync 保证仍依赖操作系统和文件系统。Memory 提供相同的原子性与隔离，但没有进程退出后的持久性。

### SQLite

`store/sqlite` 使用 Go 1.23 兼容的纯 Go modernc 驱动，不需要 CGO。连接使用 WAL、synchronous=FULL、busy_timeout=5000，并设置有限页缓存。写事务使用 BEGIN IMMEDIATE，顺序分配、CAS、记录、状态、索引、回执在同一事务里提交。数据库忙等待保留为数据库错误，不伪装成版本冲突。

rollouts 保存状态与顺序计数；entries 按 Rollout / ID、Rollout / Sequence、写入 Session、父节点、类型 / 引用建立索引。sessions、receipts、pending 和未发布副本均有明确 Rollout 归属；pending 按 Session 隔离。当前状态单独存储。普通 Commit 处理新增批次和当前工作状态，不回放或复制全部历史。历史路径通过受限的递归查询、固定连接顺序和主键查找读取；查询计划测试防止退化为全 Rollout 扫描。

`Stats().RowsRead` 统计实际返回给适配器的记录 / 状态行，不能单独代表 SQLite 内部扫描次数；需要结合查询计划和实测耗时判断。

### JSONL 与 Memory

JSONL 目录包含版本 4 的 format.json；每个 Rollout 使用其 ID 的 SHA-256 文件名、版本 4 的日志头和带 SHA-256 校验和的批次。SQLite 的 format 表与 user_version 同为 4，RuntimeState 与创建记录也使用 4。多个 Session 的提交、创建与回执进入同一 Rollout 日志。提交同步文件和目录后确认。首次访问日志回放并建立内存索引；以后只检查新增尾部，追加不重新校验已加载历史。

未以换行结束的最后一行视为未提交尾部，读取忽略它，下一次 Commit 仅截去这段。完整行损坏、校验和失败或非法状态转换返回 ErrCorrupt，不自动修复已提交历史。确认恢复在采用新读到的完整提交前执行同步。

同进程打开同一规范化目录的多个句柄共享缓存和写锁；跨进程必须由宿主独占目录。Close 由宿主调用。Memory 和 JSONL 将历史索引保存在内存中，不提供 SQLite 的长历史内存保证。

## 精确状态、Context 与预算

RuntimeState 包含 RolloutID、SessionID、本地版本、创建信息、Head、有效配置 / 环境 / 摘要引用、当前批次执行事实、当前审批和待办数量。队列正文留在 Store 中，按索引分页读取；旧批次不保存在运行状态中。

每个历史节点保存 Projection，Fork 通过索引取得所选节点状态；Projection 保留末尾工具批次的结果引用，Child 可向 Core 提供已完成的 CallState。授权、执行中状态、审批和队列不继承。Session 保留有效 Context 与必要引用，没有完整历史树缓存。任意旧节点若没有摘要且有效路径超限，Fork 或打开会明确报预算错误。

`MaxContextBytes` 默认 32 MiB，最小 4096，约束准备发送的 Request。Context 记录缓存还容纳信封、配置和当前批次，使用约两倍预算的保护上限。Token 估算与字节限制独立；模型上限仍由 EstimateTokens、输出预留和安全余量共同检查。当前工作状态、调用者持有的副本和 Backend 内存不包含在 Request 字节限制中。

模型摘要是 `CompactionRecord`；精确恢复点是 RuntimeState，二者不能互相替代。压缩选择完整消息 / 工具批次，先尝试不裁剪的摘要输入；仅在摘要模型预算不足时缩减输入副本中的工具文本。原始历史、调用标识和结果状态保留。巨大用户输入无法容纳时返回 ErrCompactionInputTooLarge。

摘要成功后，把摘要、读取边界和必要的完整环境重申原子提交，再释放旧 Context。每次请求准备最多自动压缩一次，避免无进展循环。失败摘要不会提交有效摘要；Attempt 诊断只通过可选实时事件提供，不强制写日志。成功回复及摘要保留 Usage。

## Model 与协议

Model 只包含 Spec / Stream；TokenEstimator 是 Session 的独立依赖。Spec 的 ID 必须稳定，窗口和输出 Token 限额为正。ResolveModel 返回的 ID 必须与请求一致。

### 唯一有序消息内容

`Message.Content []Content` 是消息内容的唯一来源，按原始位置同时保存 Reasoning、Text、多模态、Provider 扩展块和工具调用。`Message.ToolCalls` 字段已删除；`message.ToolCalls()` 只按 Content 顺序派生调用批次，不缓存另一份列表。

```go
type Content struct {
    Type      string          `json:"type"`
    Text      string          `json:"text,omitempty"`
    MediaType string          `json:"media_type,omitempty"`
    URL       string          `json:"url,omitempty"`
    Data      json.RawMessage `json:"data,omitempty"`
    Metadata  json.RawMessage `json:"metadata,omitempty"`
    ToolCall  *ToolCall       `json:"tool_call,omitempty"`
}
```

只有 `Type="tool_call"` 的 Assistant 块可以携带 ToolCall，且必须提供该字段；其他类型携带 ToolCall 会被拒绝。同一消息内调用 ID 唯一，名称非空，Arguments 必须是单个完整有效 JSON 值，继续允许对象以外的 JSON 值。工具参数是否符合声明的 Schema 仍在派发前检查。

Core、Session 和 Reduce 使用同一个 `Message.Validate()` 校验角色、块结构、调用身份和 JSON。类型保持开放：未知类型及其 Data / Metadata 完整保留，不被 SDK 当作工具执行；适配器不支持时必须明确报错。消息、内容块及调用各层 Metadata、加密 Reasoning、签名和大整数不经有损解析。JSON 消息入口明确拒绝旧的顶层 `tool_calls` 字段，包括空数组和 null，避免静默丢弃操作。

投影保留 Assistant.Content 的内部顺序。所有调用对应的 RoleTool 消息在完整 Assistant 消息之后，按调用块出现顺序组成批次，使用 ToolCallID 配对。Compaction 接收相同的有序消息，只能按原有策略压缩输入副本；原始块及 opaque 数据始终留在历史中。Fork、Reload、冷重开和恢复使用这些持久化事实，不从流式片段重建消息。

### 流式观察

ModelEvent 支持 `ContentIndex *int`（JSON `content_index`）与 `Metadata json.RawMessage`。标准类型为 `text_delta`、`reasoning_delta`、`tool_call_delta`，Text 保存对应增量。工具参数的增量可以是尚未闭合的 JSON 字符串，不触发参数执行。

标准 Delta 必须携带非负索引，0 也必须序列化，指向本次 Attempt 最终 Reply.Message.Content 的位置；不是工具列表中的序号。没有 Delta 的块也占据数组位置。Provider 控制事件可以省略索引，Data 和 Metadata 原样隔离复制并转发。事件保持到达顺序，由 Event.Sequence、TurnID 和 AttemptID 标识。

SDK 只保存已观察索引、类型和可选 ToolCallID，验证它们与最终成功 Reply 一致；缺失或非法索引、冲突块类型、调用 ID 不一致和非法 JSON 元数据都会使 Attempt 失败。每次 Retry / Fallback 的校验独立；客户端应按 AttemptID 区分并丢弃失败尝试的展示内容。SDK 不累积 Delta、不维护第二份消息，也不合并流式签名到最终回复：适配器必须把需要重放的数据放入完整 Reply。

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

Core Invocation 只包含执行标识、参数及不可变 opaque Binding。TargetedTool 可在审批前固定目标。Binding 的结构、目标解析与授权属于宿主；Core 和 environment 包均不解释其内部结构。工具产生的 Context 更新是不可被结果 Hook 改写的执行事实；需要声明 Sequential。

并行批次受 MaxParallelTools 限制。任一工具声明 Sequential，则整批按调用顺序执行。结果可乱序完成，但下一次 Request 按 Content 中 tool_call 块的先后顺序形成完整批次。只有完整 Reply 校验成功、Assistant 消息提交确认后才允许派发工具；参数流式生成不执行工具。工具与 Hook 必须支持并发，返回后不得继续调用 emit。

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

Environment 可选，快照只保存宿主提供的两个值：

```go
type Snapshot struct {
    Binding json.RawMessage `json:"binding,omitempty"`
    Context string          `json:"context,omitempty"`
}
```

Binding 可以是任意有效 JSON，包括对象、数组、字符串、数字、布尔和 null；nil 或零长度 Binding 表示未提供，与 JSON null 不同。它直接作为 Invocation.Binding 的隔离副本传递，不附加 Snapshot 或 SDK 目标字段。SDK 不解释其身份、路径或权限。快照、历史读取、InspectContext、Provider 的 Request.Previous 和工具回调的可变字节均与内部状态隔离。`InspectContext.Binding` 是 json.RawMessage，SnapshotID 取自保存有效快照的 Rollout Entry ID，宿主无需分配。

Context 是模型可见的宿主文本。环境条目按发生位置投影为 RoleContext / ContextKind=environment，正文精确使用 Snapshot.Context；普通生成和摘要都不自动序列化 Binding。System Prompt 不受环境影响。环境发现、设备列表、Shell、Workspace、Skills Index 以及 AGENTS.md / SOUL.md / SKILL.md 的读取和组织均属于上层产品；引用文件正文是来源材料，不提升为 System 指令。

NewSession 可以接收初始快照，或调用宿主 Provider，第一轮请求即可看到 Context。RefreshEnvironment 只允许空闲且未关闭的 Session，保留原有维护和预算限制。普通 Sequential Tool 可以通过 ToolResult.Context 提交 `json.Marshal(snapshot)`；SDK 将结果与环境节点原子持久化。失败的宿主解析不得返回环境更新；结果 Hook 无权改写已发生的环境效果。

环境切换工具的结果节点本身也保存快照效果：若 Fork 恰好选在结果节点、尚未包含同批后续环境节点，Projection 会引用结果内的快照并投影其 Context；包含后续环境节点时不重复投影。继续执行前仍需宿主重新确认。Compaction 原子重申最新完整快照，执行 Binding 和模型 Context 分开保留。

标准工具只检查 Binding 的 JSON 有效性，将目标解析与授权交给 Backend。已有进程保存原始 Binding；切换当前快照不会改变进程目标。write_stdin 在审批前恢复原始绑定，执行前再次验证所有权和绑定。审批看到的目标与执行时验证的目标必须一致。工具 Schema 固定注册，环境变化不改变工具声明。

重新打开、Reload 和未提供新 Environment 的 Fork 都把已有环境视为历史信息。产生新副作用前必须通过宿主 Provider 或 RefreshEnvironment 重新确认。Provider 只是宿主回调，SDK 不自行发现环境。没有环境的纯聊天 / 业务 API Session 不要求环境发现。环境快照继续只包含 Binding 和 Context，不提供旧环境类型或绑定信封的兼容层；有序消息使用前述 V4 存储格式。

```go
type ForkMode uint8
const (
    ForkShared ForkMode = iota
    ForkDetached
)
type ForkOptions struct {
    ID          string
    Mode        ForkMode
    Environment *environment.Snapshot
}
func (s *Session) RolloutID() string
func (s *Session) Fork(context.Context, string, ForkOptions) (*Session, error)
```

Rollout 拥有历史和存储命名空间；Session 拥有执行状态；Run 仍表示一次执行。NewSession 创建新 Rollout 及首个 Session；OpenSession 仍使用 Store 内唯一的 SessionID。Shared 是默认模式，同 Rollout 内共享历史；Detached 导入所选节点及祖先，建立独立 Rollout。

Fork 的 nodeID 是包含式边界。排除消息时传其 ParentID。SelectHead、ForkAt、ForkBefore 和选择记录已移除；History 只读浏览，OpenSession 恢复已有执行，Fork 另起执行。History.Scan 支持 SessionID、Kind、HistoryOnly；Children 分页读取直接子节点，Path 从节点向祖先分页读取。Shared 的 History 可见整个 Rollout，不能把历史可见性当作该 Session 的运行状态。

| 所选边界 | 创建与 Continue |
| --- | --- |
| User 消息 | 创建后 Continue 调用模型 |
| 完整工具批次 | 复用历史结果，不重跑 Tool / Hook，保留 Terminate 语义 |
| Assistant 最终回复 | Continue 保持完成；Start 可追加输入 |
| 配置、环境、压缩节点 | 由 Projection 和最后有效消息决定 |
| 不完整工具批次 | ErrIncompleteBatch，不创建 Child |
| 未解决外部效果 | ErrOutcomeUnknown，不创建 Child |

无论 Parent 是否运行，边界规则相同。检查 Rollout 归属、已提交节点和 Projection，不使用变化中的父 Version 判断历史可见性。Parent 可继续生成、审批、排队和提交；Fork 不改变其 Head、Version 或取消状态。

两种模式都从所选 Projection 初始化 Child，自己的创建批次使本地 Version 从 0 到 1。Detached 祖先通过历史导入路径写入，保留原始 EntryID、写入 SessionID、ParentID、Version、Sequence 和协议数据。祖先版本可以高于 Child；回放时不能把这些祖先作为 Child 的本地 Commit Reduce。Child 后续回放从创建记录保存的 Projection 开始，只应用自己的提交。Detached 的来源字段仅用于追溯，不依赖源 Rollout 存活。

目标 ID 必填，作为创建幂等身份。摘要绑定来源 Rollout、来源 Session、nodeID、模式、目标 ID 和显式 Environment，排除随机记录 ID、时间戳和父 Version。创建、Rollout 关联、来源记录和创建回执原子发布。相同请求返回同一 Child；参数变化返回 ErrConflict；已推进 Child 不重置、不自动启动。确认丢失可通过目标 Session 的 LookupCommit / OpenSession 恢复。

Shared 不复制历史前缀，只保存 Child 创建批次并读取有效 Context。Detached 的时间、磁盘和累计分配为 O(K)：SQLite 以短事务写入 building Rollout 和 ready=0 Session，JSONL 使用带稳定请求身份的 .pending 文件；完成后原子发布。复制页默认 4 MiB；超过页大小的不可分割历史记录单独读取并完整复制，峰值内存因此也取决于最大单条记录。有效 Context 预算与归档复制互相独立。分页和发布前后的真实 SIGKILL 测试覆盖两种持久后端。

未完成 Detached 在核对请求身份、取得同目标创建锁后安全清理并从头复制，不提供断点续传。`DiscardUnpublished` 同样持有目标锁并拒绝已发布目标；重试不能删除仍在复制的目标。跨进程由宿主保证目标创建和执行的独占所有权，JSONL 额外要求独占目录。所有持久记录和临时副本都可按 Rollout 归属定位。

## Rollout 生命周期边界

Session.Close 只释放执行资源，保留持久状态和全部历史；Parent 与 Child 生命周期独立。等待、取消联动、汇总、并发配额、工作目录和终端资源隔离由宿主实现。Shared 不意味着共享授权，Detached 也不自动复制外部资源。

本轮不提供 Rollout / Session 删除 API，不引入 TreeID、Branch 管理器、引用计数或 GC。Rollout 是未来整体删除单位：事务内先标记不可写，阻止 Commit / Fork；宿主停止并等待执行；整体移出可见集合并清理。崩溃后继续清理，不重新变为可执行 Rollout。当前读写路径已检查 Rollout 状态，分页导入和最终发布也验证目标仍处于 building 状态。
