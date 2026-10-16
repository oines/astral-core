# astral-core

**An embeddable Go SDK for agents that outlive a single request.**

把 Agent Loop、工具执行和长期会话直接嵌进你的 Go 应用。你决定使用哪个模型、连接什么工具、在哪里执行、如何保存数据；`astral-core` 负责让 Agent **可靠地运行、继续运行，并记住发生过什么**。

它是一个 SDK，**不是**需要单独部署的 Agent 服务，也不替你决定 UI、工作区或产品形态。

## 为什么用它？

一次模型调用并不难。难的是把它变成一个可以长期运行的 Agent：用户中途追加要求、工具执行到一半断线、服务重启后继续、从过去某一步分出新的尝试，而且不能因为恢复会话就重复执行有副作用的命令。

`astral-core` 把这些执行语义集中在一个地方：

- **完整的 Agent Loop**：流式生成、工具调用、并行执行、模型重试与 Fallback。
- **随时介入**：Steer、Follow-up、Cancel、实时事件与工具审批。
- **持久 Session**：保存对话和执行轨迹；重启后从已确认的状态继续。
- **长上下文管理**：Token 预算、自动/手动 Compaction，不删除原始历史。
- **历史分支**：从任意合法历史节点 Fork，尝试另一条路径，不影响原会话。
- **可插拔执行环境**：工具可以在本机、远程机器或自定义 Runtime 运行，环境发现完全由宿主负责。

不必采用某种 Agent Framework 的全部约定。可以只用 Core，也可以按需加入持久化、标准工具和上下文注入。

## 五分钟试跑

需要 **Go 1.23+**。仓库提供无需 API Key 的离线示例，使用确定性测试模型演示真实的 Agent 调用流程。

```bash
git clone https://github.com/oines/astral-core.git
cd astral-core

go run ./examples/basic        # 一次模型 → 工具 → 模型的完整循环
go run ./examples/approval     # 持久审批与会话恢复
go run ./examples/fork         # 从历史节点创建新 Session
```

还可以运行两个使用真实文件和 Shell 的示例（需要 Unix）：

```bash
go run ./examples/tools
go run ./examples/environments
```

在自己的 Go 项目中安装：

```bash
go get github.com/oines/astral-core
```

## 使用方式

### 1. 只需要一个 Agent Loop

```go
import astral "github.com/oines/astral-core"

agent, err := astral.New(astral.Config{
    Model: model,
    Tools: []astral.Tool{myTool},
})
if err != nil { return err }
defer agent.Close(context.Background())

run, err := agent.Start(ctx, astral.User("帮我完成这项任务"))
if err != nil { return err }
result, err := run.Wait(ctx)
```

`model` 是你接入的模型适配器（实现 `Spec` / `Stream`），`myTool` 是可选的自定义工具。完整的可运行实现见 [`examples/basic`](examples/basic/main.go)。

这个模式不需要数据库，适合短任务、测试或自己管理会话的应用。

### 2. 需要跨重启的长期会话？

加入 `session` 和 Store 即可，不需要换一套 Agent Loop。

```go
import (
    astral "github.com/oines/astral-core"
    "github.com/oines/astral-core/session"
    "github.com/oines/astral-core/store/sqlite"
)

db, err := sqlite.Open("astral.db")
if err != nil { return err }
defer db.Close()

manager, err := session.New(session.Config{
    Model:          model,
    Store:          db,
    EstimateTokens: estimateTokens,
    Tools:          tools,
})
if err != nil { return err }
defer manager.Close(context.Background())

chat, err := manager.NewSession(ctx, session.SessionOptions{ID: "my-agent"})
if err != nil { return err }

run, err := chat.Start(ctx, astral.User("检查项目并运行测试"))
if err != nil { return err }
_, err = run.Wait(ctx)

// 以后可以使用 manager.OpenSession(ctx, "my-agent") 重新打开。
```

以上是接入示意：`model`、`estimateTokens`、`tools` 由你的应用提供。Token 估算器负责计算目标模型的输入预算。

Store 有三种选择：

| Store | 适合场景 |
| --- | --- |
| [`memory`](store/memory) | 测试、临时会话 |
| [`jsonl`](store/jsonl) | 简单的文件持久化，日志可检查 |
| [`sqlite`](store/sqlite) | 长期运行的应用，支持索引和分页历史；纯 Go、无需 CGO |

## 让 Agent 真正可控

```text
用户输入 → 模型生成 → Tool Call → 执行工具 → 模型继续 → 最终回复
                         ↑
                 审批 / 取消 / 状态记录
```

使用持久 Session 时，每次工具调用都会记录执行阶段。**如果服务在工具启动后崩溃，SDK 不会猜测命令是否成功，更不会自动盲目重跑。** 它会保留未知结果，交给宿主确认。

| 想做什么 | API |
| --- | --- |
| 追加新任务 | `Start` |
| 恢复未完成执行或消费保留队列 | `Continue` |
| 被动更新模型上下文 | `InjectContext` |
| 查看或清理待处理输入 | `PeekQueuedInputs` / `ClearQueuedInputs` |
| 工作中修改方向 | `Steer` |
| 当前任务结束后再处理 | `FollowUp` |
| 停止工作 | `Cancel` |
| 订阅实时输出 | `Subscribe` |
| 查看模型实际上下文 | `InspectContext` |
| 压缩长对话 | `Compact` |
| 查看历史 | `History().Scan` / `Path` / `Children` |
| 从历史重新尝试 | `Fork` |
| 人工审批 | `PendingApprovals` / `DecideApproval` |

Core 和持久 Session 都默认每个安全边界消费一条 Steer。宿主可在各自 Config 中设置 `MaxSteeringPerTurn: 2` 到 `16`，将已排队的多条 Steer 按 FIFO 放进下一次模型请求；每条仍是独立 User Message。0 等同默认值 1，超出范围报错。FollowUp 始终逐条处理，Steer 优先；字节预算不足时剩余输入继续留队。

### Fork 的是历史，不是文件系统

每个 Session 有自己的执行状态和当前 Head；它们可以共享同一份 Rollout 历史，也可以将选中的历史路径复制成独立 Rollout（Detached Fork）。

Fork 不会回滚已经改过的文件、撤销 API 调用或复制正在运行的进程。那些外部副作用始终由宿主管理。

## 工具由你决定

任何实现 `astral.Tool` 的 Go 类型都能成为工具。你可以自己定义工具名称、参数 Schema 和执行逻辑，也可以直接使用可选的 [`standardtools`](docs/standardtools.md)：

`read` · `write` · `edit` · `exec_command` · `write_stdin`

它们通过 `Backend` 访问文件系统和进程。SDK 提供本地 Unix Shell / PTY 实现；远程 Computer、容器、SSH 或其他执行方式都可以由应用接入自己的 Backend。

**工具 Schema 是固定注册的；当前机器上有哪些 Skills、项目说明或可用资源，是环境上下文，不是新的工具定义。**

## 一次回复，保留全部内容顺序

Assistant 的 `Message.Content` 同时保存 Reasoning、Text 和 Tool Call，例如：

```json
{"role":"assistant","content":[
  {"type":"reasoning","text":"先检查第一项"},
  {"type":"text","text":"正在查询"},
  {"type":"tool_call","tool_call":{"id":"a","name":"lookup","arguments":{"key":"a"}}},
  {"type":"reasoning","text":"再检查第二项"},
  {"type":"text","text":"继续查询"},
  {"type":"tool_call","tool_call":{"id":"b","name":"lookup","arguments":{"key":"b"}}}
]}
```

没有独立的 `Message.ToolCalls` 字段；`message.ToolCalls()` 按块顺序提取调用。并行工具可以乱序完成，下一轮模型上下文仍按调用块顺序排列结果。Reasoning 签名、各层 Metadata、多模态和 Provider 扩展数据随原始消息保存，重新打开或 Fork 后保留相同顺序。

流式 `text_delta`、`reasoning_delta` 和 `tool_call_delta` 使用 `content_index` 对应最终 Content 数组；事件按 AttemptID 区分重试。增量用于展示，完整回复提交成功后才派发工具。协议细节见 [宿主契约](docs/contracts.md)。

当前 Rollout 格式为 **5**。V4 及更早的持久数据会被明确拒绝并保持原样，本轮不提供自动迁移。历史 V3 性能测量仍见 [基准报告](docs/benchmarks.md)。

## 上下文与执行控制分开

`InjectContext` 追加模型可见信息，支持 `user` / `developer` 目标 Role 和有序多模态 Content，不修改 System Prompt，也不触发模型或自动摘要。

```go
observations := []astral.Message{{
    Role: astral.RoleUser,
    ContextKind: "observation",
    Content: []astral.Content{
        {Type: "text", Text: "用户分享了这张图片。"},
        {Type: "image", MediaType: "image/png", URL: "asset://stable-image-id"},
    },
}}
id, err := chat.InjectContext(ctx, observations)

// 必须随这条 Prompt 一起生效的 Context，与输入整体接收、整体提交。
run, err := chat.Start(ctx, astral.User("分析刚才的图片"),
    astral.InputOptions{Contexts: observations})
```

这两种调用方式可独立使用。返回 ID 表示接收成功；运行中先持久排队，在整批工具结果之后安全应用。被动 Context 不使 Run 继续，也不能单独使 Continue 合法。Context 正常参与 Compaction，原始多模态消息留在历史中。大型媒体由 Host 保存并提供可重放的稳定引用，Model Bridge 负责解析及能力适配。

`Continue()` 不生成“继续”提示词。空历史、仅有 Context，或已经完成且没有驱动队列的会话返回 `ErrNotContinuable`。Cancel 保留未消费队列，恢复需要 Host 显式调用；它不会自动重启。

环境发现、授权、连接、目标选择及其持久一致性由 Host 管理。Host 工具通过 `TargetedTool.Bind` 为每次调用提供不透明 `Invocation.Binding`，Core 在 Prepared 前保存它，恢复时复用原引用。环境说明可作为普通 Context 注入；SDK 没有专用 Snapshot 或 Session 级当前 Binding。

Assistant 的终止原因保存在 `Message.FinishReason`，错误说明保存在 `Message.ErrorMessage`。失败和取消保留合法部分快照，历史可见、模型投影跳过；length 回复保留，工具调用以 NotExecuted 结果交回模型。`FinishTurn` 可选择结束或继续一次，其确认后的决策可恢复。

## 边界清晰，才能真正嵌入

`astral-core` **负责** Agent Loop、工具调用语义、Session / Rollout、历史分支、压缩、审批与恢复。

它**不提供**模型 API 聚合、托管 Sandbox、设备连接协议、身份系统、UI、Subagent 编排或 Workflow Engine。你可以把它嵌入 CLI、Web 服务、IM Agent 或已有产品，而不必反过来适应 SDK 的应用架构。

目前也没有内置 OpenAI Responses / Chat Completions 适配器或精确 Tokenizer；通过 `Model` 和 `TokenEstimator` 接入你选择的实现。

## 更多资料

- [Basic：最小 Agent 示例](examples/basic/main.go)
- [Approval：审批与持久化](examples/approval/main.go)
- [Environments：跨执行目标与进程绑定](examples/environments/main.go)
- [Tools：文件与 Shell](examples/tools/main.go)
- [Fork：历史分支](examples/fork/main.go)
- [标准工具与自定义 Backend](docs/standardtools.md)
- [宿主与持久化契约](docs/contracts.md)
- [开发与验证](docs/development.md)
- [性能基准](docs/benchmarks.md)

```bash
go test ./...
```

**一句话：你负责 Agent 能做什么，`astral-core` 负责它如何可靠地做下去。**
