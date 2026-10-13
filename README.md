# astral-core

可嵌入 Go 项目的 Agent Harness SDK。把模型和业务工具接进来，即可获得执行循环、持久会话、上下文预算、审批、历史分支和环境切换。

**Session 保存工作历史，Runtime 执行动作，Workspace 描述项目上下文。三者不绑定生命周期。** 纯聊天、业务 API Agent、连接多个执行环境的 Coding Agent 使用同一条执行循环。

## 包与边界

| 包 | 提供的能力 |
| --- | --- |
| `github.com/oines/astral-core`，包名 `astral` | Agent / Session / Run / Rollout / Context，Model、Tool、Store 接口及运行语义 |
| `github.com/oines/astral-core/store/memory` | 进程内隔离、并发安全的内存存储，适合测试和临时会话 |
| `github.com/oines/astral-core/store/jsonl` | 原子批次、版本检查、校验和、同步落盘及未提交尾部恢复 |
| `github.com/oines/astral-core/standardtools` | 可选的 `read`、`write`、`edit`、`exec_command`、`write_stdin`；另提供可选环境切换工具 |
| `github.com/oines/astral-core/standardtools/local` | 可直接使用的本地文件系统与 Unix Shell / PTY 后端 |

宿主负责模型 Provider、Runtime 连接、Workspace 创建与同步、权限判断、审批 UI、凭据、设备发现和存储设施。本地 IO 可直接使用可选后端；远程 IO 实现低层 `Backend` 接口即可复用标准工具行为。Core 不包含应用服务器、终端 UI、SSH/容器管理器、ACP/MCP 服务或多 Agent 编排器。

## 先运行示例

模块声明使用 Go 1.23；开发验证环境为 Go 1.26.3。

```sh
go test ./...
go run ./examples/basic
go run ./examples/approval
go run ./examples/environments
go run ./examples/tools
```

四个示例均使用明确标注的确定性模型适配器，无需 API Key，不访问真实模型服务。后两个使用真实的本地 Shell / PTY，需要 Unix 环境。

| 示例 | 演示 |
| --- | --- |
| [basic](examples/basic/main.go) | 业务工具 → 模型循环 → 查询最终 Context，无需执行环境 |
| [approval](examples/approval/main.go) | Hook 等待审批、宿主提交决定、JSONL 持久化、关闭后重新打开 |
| [environments](examples/environments/main.go) | 同一个 Workspace 切换 Runtime；已有进程仍在原 Runtime 上被轮询 |
| [tools](examples/tools/main.go) | 用内置工具真实创建、编辑并通过 Shell 读取文件 |

第三个示例的输出：

```text
current runtime: runtime-b
process runtime: runtime-a
workspace: project-one
```

## 接到自己的项目

在你的 Go 项目中安装：

```sh
go get github.com/oines/astral-core@latest
```

导入方式为 `import astral "github.com/oines/astral-core"`。本地联合开发时，可以用 `go mod edit -replace=github.com/oines/astral-core=/path/to/astral-core` 指向本地 checkout。

宿主先实现 `Model`，然后创建 Agent。下面的 `hostModel` 是该适配器：

```go
agent, err := astral.New(astral.Config{
    Model:        hostModel,
    Store:        memory.New(),
    SystemPrompt: "Use the available tools to complete the user's request.",
    Tools:        []astral.Tool{lookupOrder}, // 也可以没有工具
})
if err != nil {
    return err
}
defer agent.Close(context.Background())

session, err := agent.NewSession(ctx, astral.SessionOptions{})
if err != nil {
    return err
}
run, err := session.Start(ctx, astral.User("查询订单 order-42"))
if err != nil {
    return err
}
result, err := run.Wait(ctx)
```

`ModelFunc` 和 `ToolFunc` 可将现有函数接到接口上；完整的可编译用法见 [basic](examples/basic/main.go)。持久会话只需换成 `jsonl.Open(directory)` 或自己的 `Store`，记录 `session.ID()`，下次通过 `agent.OpenSession(ctx, id)` 打开。

内置工具不会自动注册。项目可以在自己的 `tools/` 包实现 `astral.Tool`，再传入 `Config.Tools`；只注册业务工具或不注册任何工具都有效。需要文件和命令工具时：

```go
backend, err := local.New(local.Options{CWD: preparedWorkspace})
if err != nil { return err }
toolset, err := standardtools.New(backend)
if err != nil { return err }
defer func() {
    cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = toolset.Close(cleanup)
}()
// 传入全部五个工具，也可以选择其中几个，或合并自己的工具。
config.Tools = append(toolset.Tools(), lookupOrder)
```

`read/write/edit` 的核心行为参考 Pi；`exec_command/write_stdin` 的模型参数、等待规则和命令会话行为参考 Codex。工具集负责算法和进程会话，后端只负责低层 IO。具体参数、生命周期和兼容边界见 [标准工具](docs/standardtools.md)。

## 常用操作

| 需求 | API |
| --- | --- |
| 发送一条新的用户输入 | `session.Start(ctx, astral.User(text))` |
| 恢复未完成工作 / 消费待处理输入 | `session.Continue(ctx)` |
| 下一安全检查点插入要求 | `session.Steer(ctx, message)` |
| 当前工作完成后再处理 | `session.FollowUp(ctx, message)` |
| 停止执行 | `run.Cancel()` 或 `session.Cancel()` |
| 观察流式输出和提交 | `session.Subscribe(buffer)` |
| 重建已提交历史 | `session.Rollout()` / `session.Entries()` |
| 查看模型实际有效输入 | `session.InspectContext(ctx)` |
| 手动摘要旧上下文 | `session.Compact(ctx, instructions)` |
| 切换后续请求使用的模型 | `session.SelectModel(ctx, modelID)` |
| 浏览历史树 | `Tree` / `Entry` / `Children` / `BranchPath` / `Head` |
| 在同一会话中选择历史位置 | `session.SelectHead(ctx, entryID)`，`Branch` 是同义入口 |
| 复制某条历史路径成新会话 | `session.Fork(ctx, entryID, astral.ForkAt/ForkBefore, options)` |
| 提交审批结果 | `session.DecideApproval(ctx, approvalID, decision)` |
| 查看并处理不确定的工具效果 | `UnresolvedTools()` / `ResolveTool(ctx, executionID, result)` |

`Wait` 的 Context 仅限制等待时间；`Start` / `Continue` 的 Context 控制 Run 生命周期。需要脱离某次 HTTP 请求继续运行时，宿主应传入服务拥有的 Context。

## 运行保证

- 每次模型请求都重建、校验并计算最终 Request 的预算；Fallback 也使用目标模型的预算。
- 仅完整成功的模型回复写入历史。失败尝试的 Delta 保持为实时事件；长度截断的工具调用不会执行。
- 工具先做 JSON Schema 校验，再进入审批 Hook。允许并行的工具受并发上限约束；任一工具要求顺序执行时，整批按调用顺序执行。
- 工具执行意图、执行完成事实和最终交付结果分别提交。重启后不会自动重跑已经开始的工具；未知效果显式交给宿主处理。
- 一个工具输出自身无法放入模型窗口时，提交交付错误并保留真实执行状态。普通结果累积过大时走上下文压缩或明确预算错误。
- 历史只追加；配置、完整环境快照、摘要及分支选择均可追溯。压缩不改写旧消息；摘要覆盖当前环境时，Checkpoint 与完整环境重申原子提交。
- Steer / Follow-up 在完整回复或整批工具之后处理。只有整批最终结果都要求 `terminate` 才跳过自动续问；排队输入仍会处理。
- 慢订阅者不会阻塞执行；溢出后订阅关闭，宿主从 Rollout 恢复已提交内容。

这些保证依赖 [宿主和适配器契约](docs/contracts.md)。尤其是 **Store 的版本检查不能代替执行所有权**：同一个 Session 在任何时刻都必须由一个 Agent 实例拥有；JSONL 目录必须由一个进程独占。跨进程调度属于宿主。

## 默认策略

| 配置 | 零值行为 |
| --- | --- |
| `Retry.MaxAttempts` | 每个模型最多 3 次尝试 |
| `Retry.InitialBackoff` / `MaxBackoff` | 250ms / 10s；适配器给出的 `RetryAfter` 可以更长 |
| `Retry.RequestTimeout` | 不另加单次 Stream 超时，服从传入 Context |
| `Retry.MaxFallbacks` | 最多 1 次 Fallback；没有宿主回调时不切换 |
| `Budget.OutputTokens` | `min(16384, model.MaxOutputTokens)` |
| `Budget.SafetyMargin` | 0；宿主应按 Token 估算精度配置 |
| `Budget.AutoCompactAt` | 只在预算溢出时尝试自动压缩；正值表示额外提前预留量 |
| `Compaction.KeepRecentTokens` | 20000，按完整工具批次边界保留 |
| `Compaction.OutputTokens` | `min(4096, summaryModel.MaxOutputTokens)` |
| `Compaction.ModelID` | 当前有效模型 |
| `MaxParallelTools` | 8 |
| `MaxTurns` | 不限制 Run 产生的新 Turn 数量 |

`Compaction.Disabled` 关闭自动压缩；手动 `Compact` 仍可使用。构造参数中的切片、Schema 和快照会被复制；改变已有会话的配置要调用 `Configure` 或 `SelectModel`。

## 进一步阅读

- [宿主和适配器契约](docs/contracts.md)：持久化、恢复、审批、环境、事件和模型协议。
- [代码导航与验证](docs/development.md)：按职责组织的目录、推荐阅读顺序、测试和开发命令。
- [标准工具](docs/standardtools.md)：Pi / Codex 对齐范围、可选本地后端、远程后端契约与真实执行示例。

当前提供 Core、存储实现、标准工具实现、可选本地 IO 后端和离线模型示例。真实 Provider 的协议转换与 Token 估算、远程 Runtime 的 IO 和权限，需要在对应宿主适配器中接入并验证。
