# 宿主和适配器契约

## 执行所有权与生命周期

一个 `Agent` 内，同一 ID 的 `OpenSession` 返回同一 Session 对象；一个 Session 同时只能有一个 Run 或独占维护操作。手动 Compact、恢复预检和工具效果确认属于维护操作。冲突返回 `ErrBusy`。

宿主必须保证跨 Agent 实例、进程和主机的独占执行所有权。即便多个实例共享具有 CAS 的数据库，也不能并行执行同一 Session：版本检查只能阻止冲突写入，无法撤销已经发出的外部操作。v1 不实现租约、fencing 或分布式调度。

`Agent.Close(ctx)` 阻止新工作、取消并等待已有 Run 和维护操作，关闭事件订阅，不关闭宿主依赖。模型、工具、环境发现和其他宿主回调必须响应 Context；Go 不能强制终止不配合取消的回调。`Run.Wait(ctx)` 超时只停止等待；`Run.Cancel()` 才取消执行。

## Store

`Append(ctx, sessionID, expectedVersion, entries)` 提交非空、完整、不可分割的批次。新 Session 的初始版本是 0，每个成功批次加 1，而非每个 Entry 加 1。

- Entry ID 和内容一经提交不可变，不得覆写或部分提交。
- `Read` 返回隔离副本；不存在时返回 `ErrNotFound`，版本不匹配时 `Append` 返回 `ErrConflict`。
- 持久 Store 成功确认的写入必须可以在重新打开后读取。
- 对于提交成功但确认丢失的错误，Core 会用独立、短暂的恢复 Context 读取并检查整个批次的 ID、内容和版本；只有完全匹配才继续依赖它的动作。
- 因此，供确认恢复使用的 `Read` 也必须返回可靠提交状态。不能把尚未满足自身持久性保证的缓存数据当作已提交批次。
- 冲突后不会自动刷新并继续副作用。宿主重新建立独占所有权后，才调用空闲 Session 的 `Reload`。

`ValidateRollout` 检查信封、版本和历史树引用。工具配对由统一的 Context 构建路径进一步检查；历史可以停在尚未完成的工具批次中。宿主正常使用 Session API，不通过直接写 Store 绕过执行阶段。

### 内存与 JSONL

`memory.Store` 并发安全，但不提供进程退出后的持久性。

`jsonl.Store` 使用 Session ID 的 SHA-256 作为文件名；一行是一个带校验和的完整提交，包含该批次的所有 Entry。它不使用 Scanner 的小行长限制。

- Append 同步文件和目录后才确认；Read 在返回可用于确认恢复的完整提交前也执行同步。
- 最后一行没有换行符时，它被视作未提交尾部。读取忽略它，下一次 Append 只截去这一段。
- 已完整结束的行一旦损坏、校验和失败或版本断裂，返回 `ErrCorrupt`，不会自动删去已提交历史。
- 同一进程通过多个路径别名打开一个目录时共享写锁；跨进程必须由宿主独占目录。
- 存储目录及其父目录应位于宿主管理的持久文件系统。持久性依赖文件系统正确实现文件和目录同步；不支持该操作时会返回错误。

## Model

`Model.Spec()` 的 ID 必须稳定，窗口和最大输出 Token 必须为正数。`ResolveModel` 返回的 ID 必须与请求 ID 一致。`EstimateTokens` 要计算**实际发送的完整输入**：System Prompt、全部有效消息、工具定义，以及 Provider 的消息包装、图片和其他协议开销。Core 单独加入输出预留和安全余量。

适配器需要做到：

1. 保留 Tool Call ID、参数、消息/内容/调用的 opaque Metadata、Reasoning 签名和多模态部分。不支持时明确报错，不能静默丢弃。
2. 将 `RoleContext` 转换为目标协议允许的模型可见上下文，保留其位置和顺序；不将索引中的文件正文提升为 System 指令。
3. `Stream` 只返回一次完整 `Reply`，准确标记 `FinishReason`。`length`、`error`、`cancelled` 都不能作为成功回复提交。
4. 在 `Stream` 返回前停止调用 `emit`。Delta 是展示信息，最终 Reply 才是可提交内容。
5. 通过 `ModelError{Transient, RetryAfter, Code, Cause}` 分类错误。认证、无效请求等应为非临时错误。

每次重试都产生新 Attempt ID，同一个 Turn ID 保持不变。临时错误按策略退避；单次 Stream 超时按临时错误重试，父 Context 取消则停止。失败 Delta 不会混入重试后的消息。只有宿主的 Fallback 回调决定替代模型，以及是否持久改变首选模型。

显式 `SelectModel` 不截断正在进行的 Stream。下一次请求重新解析模型；如果选择发生在较长的自动摘要期间，正式生成前会使用新模型重新检查预算。已完成的工具不会因 Retry 或 Fallback 再执行。

## 工具执行、结果和恢复

工具声明在 `New` 时复制并编译 JSON Schema。默认使用 Draft 2020-12；允许声明内的引用，不加载外部 URL 或本地文件引用。恢复会话时，当前工具声明及 Version 必须与路径上的配置匹配；升级行为要显式重新配置。

Core 按以下顺序工作：

```text
参数校验 → 目标绑定 → prepared → before hook → authorized
          → started → Execute → completed → after hook
          → 结果预算检查 → result（以及可选的完整环境快照）
```

`prepared` 固定 Invocation 和执行目标；`authorized` 固定 Hook 决定；`started` 在实际派发前提交。`completed` 保存执行效果与可用 Artifact/环境事实，不存储未处理的大输出。最终 `result` 保存实际接纳的模型结果。Skip、无效参数和 Block 可以直接形成 `not_executed` 的终态结果。

允许并行的工具最多使用 `MaxParallelTools` 个工作者。任何声明了 `Sequential` 的调用都会使整批串行。所有结果落定后，Context 按原调用顺序拼成合法完整批次。工具和 Hook 在并行批次中可能并发调用，宿主实现应适当同步。`Execute` 返回后不得继续调用 `emit`。

### 效果与交付分别表达

`ToolResult.Outcome` 描述执行效果；`IsError` / `Code` 可以描述执行错误，也可以描述输出交付失败。正常返回且未设置 Outcome 时，按 `IsError` 推导为 `succeeded` 或 `failed`。普通工具错误产生配对结果，并让模型决定下一步；Core 不自动重试工具。

普通 Go 执行错误默认记录为 `failed`，但不保证没有部分效果。宿主已知未派发时可返回 `ToolError{Outcome: OutcomeNotExecuted}`；远程确认丢失等不确定情形应返回 `ToolError{Outcome: OutcomeUnknown}`，或在 ToolResult 中显式设置 Outcome。取消或 panic 导致的未知效果同样不会被自动重复。

单个结果的预算检查包含系统提示、工具定义、当前 Assistant 的全部调用、其余结果占位、输出预留和安全余量。超大输出替换为 `result_too_large`，保留真实 Outcome 和 Artifact 引用。普通结果累积超限则由下一请求的预算/压缩路径处理，不能误报某个工具执行失败。

Core 向模型投影结果的 Content、IsError、工具名和 Call ID；Details、Artifact、Code 和 Outcome 也保存在 Rollout 供宿主读取。需要模型直接使用的附加信息，应包含在 Content 中。大原始输出由宿主存入 Artifact；它不应塞入 Details 绕过模型结果预算。

### 重启

| 最近可靠事实 | Continue 的行为 |
| --- | --- |
| 参数准备好，但无 `started` | 可以继续审批与执行，复用固定 Invocation 和已存决定 |
| 有 `started`，没有可靠终态 | 追加 `outcome_unknown`，返回 `ErrOutcomeUnknown`，不再派发 |
| 有 `completed`，没有最终结果 | 追加 `result_unavailable`，保留已知效果；不重跑工具或 after hook |
| 已有最终结果 | 直接复用；不会重复执行 |

`UnresolvedTools()` 返回需要宿主处理的记录。宿主查询真实外部状态后，通过 `ResolveTool` 追加确认结果；原记录保留。新的结果仍经过输出预算检查。宿主也可明确接受仍然未知的 Outcome，再继续工作；确认本身不会重新执行任何工具。

这是一套避免盲目重放的持久语义，不是对外部世界的 exactly-once 承诺。工具可以把 `Invocation.ExecutionID` 用作业务侧幂等键，但相关业务存储与外部事务仍由宿主负责。

## Hook、审批和提前结束

`BeforeToolCall` 只接收已验证的参数和已固定的目标。默认放行；返回 Block 则形成 `not_executed` 结果，不运行 after hook。

```go
BeforeToolCall: func(ctx context.Context, inv astral.Invocation) (astral.ToolDecision, error) {
    decision, err := inv.AwaitApproval(ctx, astral.ApprovalRequest{
        Prompt: "允许执行这次操作吗？",
    })
    return astral.ToolDecision{
        Block: !decision.Approved,
        Reason: decision.Reason,
    }, err
},
```

每个 Invocation 支持一个稳定审批门。请求及决定都持久化；恢复时以同一审批请求重入会返回已记录的决定。**Hook 在 `authorized` 提交前可能重入**，因此审批门之外的 Hook 逻辑应可重复，不能在此偷跑工具副作用。宿主用 `PendingApprovals()` 重建待审批 UI，并以 `DecideApproval` 提交决定；同一决定重复提交是幂等的，冲突决定返回 `ErrAlreadyResolved`。

`AfterToolCall` 返回 `ToolResultPatch`：nil 字段维持原值，布尔指针可以明确设置 true 或 false。它能修改 Content、Details、IsError、Code、Artifact 和 Terminate；实际执行 Outcome 与环境效果不允许改写。Hook 失败时保留已完成事实并追加处理失败说明。所有最终内容仍须通过结果预算检查。

只有一批全部最终结果的 Terminate 都为 true，才停止自动续问。Steer 和 Follow-up 仍在相同检查点处理，Cancel 则另行中止运行。

## 环境、Runtime 与 Workspace

Environment 不是 Session 的前提。没有快照、没有标准工具时，纯聊天和业务 API 工具仍正常运行。

宿主提供完整 `EnvironmentSnapshot`：Runtime / Computer / Workspace 标识、OS、Shell、cwd、Computer/Skills/Instructions 索引和自定义元数据。索引发现、文件读取、授权、连接建立和 Workspace 准备都由宿主完成。索引本身不会让外部文件内容取得 System 指令优先级。

初始、切换、刷新快照均追加为独立、不可变、完整的历史记录。后续普通请求保留原有前缀；System Prompt 不随环境改变。工具批次内部出现的环境记录在完整 Tool Call/Result 组后投影，彼此顺序不变；没有模型请求会观察半个批次。

执行时 `Invocation.Binding` 固定 Snapshot、Runtime、Computer、Workspace 标识，`Invocation.Environment` 携带准备时的完整快照。环境切换必须使用 Sequential 工具，其结果与新快照同批提交，之后的调用才能使用新目标。活跃 Run 期间直接调用 `RefreshEnvironment` 返回 `ErrBusy`。

`TargetedTool.Bind` 可在准备/审批前选择某个资源的原始目标。标准 `write_stdin` 用它查询工具集拥有的数字 `session_id`，先验证所属 Agent Session，再返回启动该进程时冻结的 Binding 和完整快照；当前环境的变化不会改变它。执行前再次检查句柄与绑定，防止审批或排队期间进程已被回收。Backend 仍须验证自己可以服务该目标。数字 ID 不是授权凭据，也不跨工具集重启恢复。

重新打开有环境的会话、选择历史 Head 或 Fork 后，旧 Snapshot 只说明历史。继续执行前需要宿主 `Environment` 回调重新发现环境，或在空闲时显式 `RefreshEnvironment`；缺少该能力会得到 `ErrEnvironmentRefresh`。Fork 可以通过 SessionOptions 提供重新授权后的快照。未知工具效果的恢复先于环境发现，避免把旧效果误当成新目标上的操作。

## 历史、分支与压缩

历史条目以 ParentID 组成树。配置、消息、环境、工具终态和摘要参与树；运行、队列、审批及执行阶段事实通过引用关联，不改变历史父链。

`Entries` 返回全部原始条目；`Tree` 返回全部历史节点；`BranchPath` 返回指定 Head 的祖先路径。`SelectHead` / `Branch` 持久化 Head 选择，下一次历史 Append 形成新子节点。所有兄弟分支继续保留。`ForkAt` 复制指定路径，`ForkBefore` 只接受 User Entry，复制到其父节点。

Fork 不复制 Run、排队输入、待审批状态或操作授权。历史里可能仍能看到旧进程句柄，但它属于源 Session；标准工具不允许新 Session 直接使用它。分支和 Fork 都不会回滚文件、进程或外部副作用。任意 Entry 都可以浏览，半个 Tool Batch 的历史位置不能直接执行。

Steer / Follow-up 是 Session 级持久队列，一次按 FIFO 消费一个；同 Session 改选 Head 后尚未消费的输入仍属于该 Session，Fork 不继承它们。队列消费与对应 User Message 原子提交。终止检查与队列观察共用锁，较晚到达的输入留给下一次 Continue。

Context 只采用活动祖先路径上的有效配置和 Compaction。压缩选择完整消息/工具批次，先尝试完全不裁剪的独立摘要请求；只有它超预算时，才逐步缩减摘要输入副本中的工具文本。调用标识、状态、裁剪说明和原始 Rollout 保留。巨大用户输入仍无法放入时返回 `ErrCompactionInputTooLarge`，不发出已知超预算请求。

成功的摘要 Checkpoint 与必要的完整环境重申原子提交。随后重新校验普通模型请求；每次请求准备至多一次自动压缩，不能以无进展摘要无限循环。失败摘要可留下尝试审计记录，但不会留下有效 Checkpoint。

## 事件与读取

事件包含 Session / Run / Turn / Attempt / Entry / Execution 等适用的关联 ID。Turn 是一条完整 Assistant 回复和其整批工具；重试属于同一 Turn，摘要不算普通 Turn。

`Subscribe` 提供尽力投递的实时事件。缓冲满时订阅关闭，`Err()` 为 `ErrSlowSubscriber`；模型和工具不会等待慢 UI。先订阅，再读取 Rollout，并按 Entry ID 去重，可衔接持久内容。Delta 无法从历史还原，失败 Attempt 的 Delta 应被 UI 丢弃。

事件 Sequence 只在当前 Session 句柄内递增，不是跨进程永久游标。订阅关闭或网络重连后，以 Rollout 重建已提交状态。`InspectContext` 返回有效请求、消息来源 ID 和估算，但不会触发模型请求、压缩或环境发现。
