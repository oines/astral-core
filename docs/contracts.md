# 宿主与适配器契约

## Session 与所有权

根包 `astral` 提供 `NewSession(ctx, Config, SessionOptions)` 和 `OpenSession(ctx, Config, id)`。Model、Store、EstimateTokens 必填；Memory 与 SQLite 共用同一执行循环。Host 负责同一 ID 的唯一活动句柄与跨进程所有权。Store 乐观 CAS 拒绝冲突写入，但不能撤销已经执行的外部动作。

一个 Session 同时拥有一个 Run 或维护操作。冲突返回 ErrBusy。Fork 在固定、已提交的完整历史边界工作，不改变 Parent 的执行状态。OpenSession 只恢复已提交事实，不扫描环境、重放流、重启模型或恢复退避计时。

Run 接受成功后使用 Session 自己的生命周期。提交请求的取消不取消 Run；Run.Wait(ctx) 只取消等待，Session.Cancel / Run.Cancel 才取消执行。Compact 是同步维护操作，其调用 Context 可取消；HTTP Host 应使用服务生命周期执行它。Session.Close 取消并等待 Run、维护和 Fork，释放上下文缓存，最后关闭订阅。超时后可以再次 Close。Host 在 Session 结束后关闭 Store、Model、Backend 和 ToolSet。

回调必须响应 Context；Go 无法强制终止不配合取消的回调。

## 输入接收与执行

- Prompt 要求空闲且协议完整，接收与 Run 开始记录一起提交。Busy 不接收新 ID。
- Steer 和 FollowUp 只入队。活跃 Run 自然读取队列；空闲时需要显式 Continue。
- Continue 不制造消息；可处理首条排队输入、继续未完成模型工作或恢复工具批次，无工作返回 ErrNotContinuable。
- InjectContext 接收被动 Context；空闲维护及 Run 结束时分批应用积压，并可触发现有自动压缩，不启动主模型循环。预算、取消或摘要失败时保留尚未提交的 pending 输入，重试复用同一输入 ID。
- Cancel 和失败保留未消费队列，不自行继续。一次 EndRun 控制完成后，Core 为已排队的独立输入开始新 Run。

Input 要求调用方提供稳定 ID。未提供 Digest 时 Core 对消息与附带 Context 的规范化 JSON 计算摘要；提供 Digest 时它代表冻结前的原始业务提交。Host 必须诚实计算摘要，并在重读附件、生成时间戳之前调用 LookupInput。相同 ID、Kind、Digest 返回既有回执，重复 Prompt 的 Run 为 nil。不同内容或 Kind 返回 ErrConflict，即使会话正在运行也先进行此查询。

InputReceipt 保存第一次的 InputRecord、AcceptedAt、pending/applied/cleared 状态和驱动消息 EntryID。载荷不会因后续重试改变。索引与接收、消费、清除记录在同一个 Store 事务中更新。Host 不保存输入状态镜像，也不通过扫描历史补回执。业务事件路由和出站发送回执仍由 Host 管理。

Input.Contexts 与 Message 整体接收、整体应用。Steer 优先，默认每个边界选择一组，MaxSteeringPerTurn 允许 1–16；FollowUp 在自然结束时选一组。被动 Context 每个边界最多选择 64 组。选择范围有限且固定，期间新到输入留给下一边界。清除只影响驱动输入，不删除独立被动 Context。选中的完整候选放不下时整体失败，不先应用其中一部分。

## 统一准备与压缩

每个普通轮次使用 prepareStep：

1. 在短锁内复制分支与配置、读取选中输入，不消费。
2. 在锁外解析模型并调用 CollectContext，把最新环境、旧上下文和选中输入组成完整候选，再估算输入和判断压缩。
3. 必要时只对已提交旧历史生成摘要草稿，每次准备最多一次。待投递输入不进入该摘要。自动压缩根据剩余空间收缩近期保留目标。
4. 摘要完成后再次调用只读 CollectContext，取得压缩后的完整最新环境。被动维护可以跳过普通扫描，但发生压缩后必须重新采集。
5. 按“摘要与保留历史 → 被动 Context → 最新环境 → 驱动组”形成候选，检查完整协议、Token 和字节预算。
6. 重新加锁验证分支 Head 和输入仍处于 pending，原子提交摘要、Context、输入与消费事实，随后调用模型。

队列或尝试审计引起的版本增加可以使用最新版本提交；分支或配置变化返回 ErrConflict，被清除的输入不能应用。Model / Computer I/O 不持有 Session 锁或数据库事务。上下文采集、最终预算或提交失败不会改变旧有效上下文；未提交摘要在重启后可以重算。提交成功而回执丢失，通过稳定 Commit ID 确认。

CollectContext 接收 SessionID、Reason、CompactionID 和只读 Context，返回独立消息。Host 负责发现、权限、指纹及相对当前分支的去重。新 CompactionID 要重新收集完整快照；Core 不把它当作持久待完成任务。动态环境应使用 RoleUser 普通 Context，SystemPrompt 仅放稳定规则。回调不能修改 Core 或执行业务副作用。

手动 Compact 复用准备流程，可应用选中的被动 Context，但不消费驱动输入、不生成普通回答。摘要请求独立校验预算，按完整消息/工具批次选择旧历史，沿用普通请求的对话顺序，保留原生消息、多模态内容和工具调用关系；同批工具结果按调用声明顺序紧随 assistant 调用，不额外附加内部历史 ID 或执行记录清单。仅摘要请求超预算时可缩短工具文本的副本；原始历史不变。只有有效非空文本摘要可提交。

默认摘要使用 Goal、Constraints & Preferences、Progress（Done / In Progress / Blocked）、Key Decisions、Next Steps、Critical Context 结构。已有摘要作为首条源消息参与下一次压缩；更新指令要求保留仍有效的目标与约束，合并新进展和决策，更新完成状态、阻塞与下一步，并保留准确路径、标识符和错误信息。模型只生成交接摘要，不继续执行源对话中的任务，也不能把未经确认的操作写成已完成。自定义 Compaction.SystemPrompt 可覆盖默认格式。

MaxContextBytes 默认 32 MiB、最小 4096，约束实际 Request；历史工作窗口有约两倍字节保护上限。输入优先采用有效普通生成实报 InputTokens + OutputTokens，并增量估算其后新增内容；InputTokens 已包含缓存，缓存分项不另加。有效性由成功 Attempt、Configuration 和冻结请求加回复的上下文指纹确认；压缩、模型/配置或覆盖前缀改变时回退为完整内容估算。缺失、全零和失败用量不充当基线。估算器必须非负，并支持完整请求、空请求与不带 System/Tools/Options 的部分消息尾段。

默认压缩条件为 Input > ContextWindow - 16384，近期保留目标 20000，摘要输出 min(floor(0.8 × ReserveTokens), 摘要模型最大输出)。按 ModelBudgets 可覆盖普通模型的 ReserveTokens / KeepRecentTokens。普通输出 min(期望输出或模型最大输出, ContextWindow - Input - 4096)，可用输出不足 1 时拒绝发送。SafetyMargin 只预留输出空间，不叠加到压缩阈值或独立的 MaxInputTokens 上；摘要仍独立检查完整请求与输出预算。单个工具结果不可交付时保存有界的交付失败结果与真实 Outcome；聚合压力在准备阶段处理，不改写成功工具的执行事实。

## 模型协议、重试与审计

Model 提供 Spec / Stream；ResolveModel 必须返回匹配 ID。RequestPreparer 在预算检查前完成无副作用的请求适配，可能被多次调用；不得在 Stream 内追加实时环境或重新读取可变附件。冻结图片与稳定引用由 Host 负责。RequestValidator 单独校验完整请求，在动态输出长度确定后、候选提交前运行；部分尾段的估算不要求完整工具协议。

普通生成与摘要调用同一个模型入口，默认参数如下：

| 参数 | 默认 |
| --- | --- |
| MaxAttempts | 3，总数包含首次与协议修复 |
| InitialBackoff | 1 秒 |
| MaxBackoff | 5 秒；合法的较长 Retry-After 优先 |
| RequestTimeout | 每次 4 分钟 |

只有 ModelError.Transient 或请求超时触发网络重试。用户取消与明确永久错误停止；网络等待可取消。每次新 Attempt 使用独立 ID，同一逻辑请求保持 RunID / TurnID / Purpose。网络重试复用已经冻结的模型、配置、消息，不再次准备、摘要、收集环境、消费输入或运行工具。

ModelRequestRecoverer 仅处理 Provider 明确拒绝的回放数据。最多一次有效修复，占用剩余尝试预算，只改请求副本；模型身份、Options、SystemPrompt、Tools、输出预留不能改变，协议和 Token/字节预算重新校验。Models 自身只做单次请求与错误分类。Host 和 UI 不叠加模型重试。

提供商明确标记 ModelError.ContextOverflow 时，跳出原样网络重试，在完整边界最多压缩并重新生成一次。恢复期间不消费新队列输入，不重跑工具；保留完整工具批次，即使估算器低估也必须切出可压缩的旧组。再次溢出或压缩失败停止；Disabled 禁止自动溢出恢复。

ModelAttempt 独立于历史树，归属于 Session。RequestInput 固定 RolloutID、HeadID、ConfigurationID、ModelSpec、消息下标选择和 JSON 结构差异，并保存重建结果的语义摘要。它引用历史正文，不保存每轮完整 Request。ReadContext/RebuildRequest 只加载所选祖先链上的有效窗口，不执行准备回调、环境采集、估算或模型 I/O；重建必须验证配置引用和内容摘要。

首个普通 Attempt 与选中的输入、环境、摘要采用、消费队列记录原子提交。成功回复与 Attempt 完成原子提交：正文仅保存于 EntryMessage，Outcome.EntryID 指向该节点，Outcome.Reply 为空。失败或尚未采用的摘要输出保留在 Outcome.Reply；摘要采用与 Compaction 节点原子提交并清除临时正文。审计保存失败阻止依赖动作，不把 Store 错误当作模型暂时错误。仅完整、校验通过的 Stop / ToolCalls 回复进入对话；Generation 自带 AttemptID、ConfigurationID、冻结 ModelSpec 和覆盖上下文指纹，不依赖父 Session 的审计记录。指纹不含动态输出上限。失败、取消、Length 片段既不合并到成功回复，也不触发工具。

RuntimeState.CurrentAttemptID 固定尚未确认回复的普通生成。进程崩溃后显式 Continue 将无终态的旧尝试标为 interrupted，创建 RetryOf 关联的新 Attempt，并重建同一冻结输入；期间到达的输入仍留在队列。远端执行结果可能未知，不承诺外部请求恰好一次。明确取消或正常失败的 Run 会清除此指针，后续激活重新准备。已确认的助手/工具结果不重复执行。

Message.Content 是唯一有序内容来源，按原始顺序包含 Reasoning、Text、ToolCall、多模态及扩展块。message.ToolCalls() 仅派生调用列表。工具 ID 在同一消息内唯一，参数是完整 JSON。未知 Content 类型保留，由适配器明确支持或拒绝；签名、加密数据、Metadata 和大整数保持原样。

text_delta / reasoning_delta / tool_call_delta 的 content_index 必须引用最终 Content 数组中的对应类型，ToolCallID 也必须匹配。每次尝试独立校验，SDK 不从增量拼接完整回复。工具只在完整回复已确认后派发。

Request.Options 保留未指定与显式零值/false 的差别，Reasoning.Effort 使用 Provider 原生值。Core 校验结构，Models 验证协议能力。Compaction.Options 独立于普通生成。SelectModel 不截断已经冻结的请求；准备尚未提交时改变配置会导致旧准备返回冲突，下一次 Continue 使用新配置。

## 工具事实、审批与控制

普通工具持久顺序是 prepared → authorized → started → completed → result。Invocation 包含稳定 ExecutionID、AssistantID、Call 和 Host 私有 Binding。TargetedTool.Bind 的结果在 Prepared 时冻结，恢复不重新绑定。Binding 不进入模型或摘要。

非串行批次先按声明顺序完成全部预检，再按 MaxParallelTools 派发。Sequential / MayEndRun 会串行化批次；Exclusive 要求调用独占该回复。结果按调用声明顺序进入下一请求，执行可以乱序完成。

BeforeToolCall 可以通过 ToolContext.AwaitApproval 使用一个稳定审批门。请求和决定持久化；相同决定幂等，冲突决定返回 ErrAlreadyResolved。authorized 提交前 Hook 可能重入，审批门外不应产生外部副作用。AfterToolCall 返回结果补丁，可修改交付内容而不能改实际 Outcome；恢复已 completed 操作不重新运行该 Hook。

- 未 started 的准备工作可以恢复执行。
- 已 started 且没有完成事实的操作记为 OutcomeUnknown，并阻止继续，直到 Host ResolveTool。
- 已 completed 但缺结果的操作保存 result_unavailable，不为重取输出而重复执行。
- Outcome、IsError、进程退出码分别表示执行事实、交付错误与进程结果，不能互相替代。

MayEndRun 要求唯一 ToolCall，允许 Reasoning、不允许普通正文。返回 EndRun 时原子保存 ToolControl 和 Run(completed)，不生成普通 ToolResult、不调用结果处理。确认的控制不被并发 Cancel 改写。模型投影省略完整控制 Assistant，原始历史仍可查询；Fork 保留控制事实。控制工具在 Started 后中断时保存 ToolUnresolved，Host 可以明确确认普通结果或 EndRun，Core 不猜测。

## Store、历史与 Fork

根包 Store 提供 LoadState、Get、Scan、GetAttempt、ScanAttempts、Pending、Commit、LookupCommit、LookupInput 和 Fork。Get / Scan 按 RolloutID 读取；Attempt、状态、队列、提交与输入回执按 SessionID 读取。所有返回值是隔离副本。Attempt 分页游标按创建时的 Session Version 前进，不随结果完成改变；支持用途过滤和正反向扫描。

格式 **10** 仅支持自身；旧格式返回 ErrUnsupportedFormat，不在运行时升级、导入或改写。SQLite 在变更 WAL/Schema 前检查格式。Memory 具有相同的原子性，但不承诺进程退出后的持久性。

Commit 包含稳定 ID、ExpectedVersion、Entries 和 Attempts 变更。Store 在一次原子操作中检查 CAS、分配 Rollout Sequence、执行同一个 Reduce 与 ApplyAttemptMutation、保存记录、Projection、RuntimeState、Attempt、Pending、Input 索引及回执。只有 Attempt 变更的提交仍推进 Session Version，Receipt 的 FirstSequence/LastSequence 均为零，不占用 Rollout Sequence。Entry.Version 是写入 Session 的本地版本；Entry.Sequence 是 Rollout 顺序，两者独立。创建 Attempt 时必须验证固定 Head 的协议完整性、Configuration 引用与归属；成功结果只能关联同一提交中新写入的合法节点。

确认前 Session 不推进缓存。错误后最多用独立 5 秒 Context 查询 LookupCommit，验证 ID、Digest、Version 和 CurrentVersion；其他所有者已推进版本时仍停止。相同 Commit ID 相同内容幂等，不同内容冲突。Reload 只供 Host 重新建立独占所有权后的空闲句柄使用。

SQLite 使用纯 Go 驱动、WAL、synchronous=FULL、busy_timeout=5000 和 BEGIN IMMEDIATE。输入正文按索引读取，RuntimeState 只保留当前批次、审批、待办数量、分支引用和最近 Run。普通提交不扫描历史。Session 缓存有效窗口与必要引用，不持有完整历史树。Stats.RowsRead 是返回到适配器的行数，不等于数据库内部扫描量。

Fork 边界包含所选历史节点且必须协议完整。Shared 共享不可变祖先，Detached 分页复制所选路径并在完成后发布。稳定子 Session ID 与 ForkRequest 摘要用于重试确认；重试返回新句柄，Host 负责唯一活动实例。子会话继承历史配置与完整结果，不继承 Parent 的 Attempt、队列、运行中授权或审批。复制的 Generation 仅为来源与用量元数据，不要求父 Attempt 存在。关闭 Parent 不关闭 Child，Fork 不回滚文件或复制活动进程。清理规则见 Store 的引用与 Rollout 状态 API。

## 事件与断线恢复

Subscribe 非阻塞、尽力投递，溢出关闭并返回 ErrSlowSubscriber。先订阅，再获取 Snapshot 和分页历史；按 Entry ID 去重。Event.Sequence 与 Snapshot.EventSequence 只属于当前 Session 句柄，Host 用运行实例 Epoch 区分重启，不把事件序号当持久历史游标。

AttemptStatus 提供 Purpose、Run/Turn/Attempt ID、Attempt、MaxAttempts、State、NextRetryAt 和安全错误代码。Snapshot 原子读取当前 RuntimeState、运行/维护/停止状态与尝试信息。重连恢复当前状态并忽略快照序号之前的增量；失败 Attempt 的临时输出不能恢复为对话事实。浏览器重连不调用 Prompt 或 Continue，也不取消服务器工作。
