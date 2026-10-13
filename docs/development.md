# 代码导航与验证

## 组织方式

Core 保持一个 `astral` Go 包：Agent、Session 和 Run 共同维护执行状态与权威历史。源文件按职责分组，公开类型与对应能力放在一起。存储实现和可选工具是独立包；端到端测试从宿主视角消费公开 API。

```text
astral-core/
├── agent.go、config.go            # Agent 创建、配置与依赖
├── session.go、session_*.go       # 会话身份、历史、输入、审批和恢复
├── run.go、generation.go          # 执行循环与模型请求调度
├── model.go、message.go           # 模型接口与消息协议
├── tool.go、tool_*.go             # 工具接口、执行、Hook 与结果
├── environment.go                # 环境快照、目标绑定与刷新
├── rollout*.go                   # 持久条目、Store 契约与校验
├── context.go、budget.go          # 上下文投影与预算
├── compaction.go、summary.go      # 历史选择与摘要请求
├── events.go、errors.go           # 事件与错误
├── store/
│   ├── memory/                   # 内存 Store
│   └── jsonl/                    # 文件 Store
├── standardtools/
│   ├── backend.go                # 低层 FileSystem / Process 契约
│   ├── files.go、edit.go、diff.go  # Pi 文件工具算法与变更队列
│   ├── image.go、truncate.go      # 图片处理和有界输出
│   ├── process.go、terminal.go   # Codex 命令协议与会话管理
│   ├── environment.go            # 可选环境切换工具
│   ├── tools.go                  # 可选工具注册与 Set 生命周期
│   ├── local/                    # 真实本地文件、管道和 PTY
│   └── testdata/                 # 从本地 Pi 源码生成的语义样例
├── tests/integration/            # 公开 API、故障恢复、存储和工具集成测试
├── examples/                     # 可直接运行的接入示例
└── docs/                         # 契约与开发说明
```

## 从哪里开始读

接入 SDK 时，从 [`config.go`](../config.go)、[`model.go`](../model.go)、[`tool.go`](../tool.go) 看宿主要提供什么，再看 [`examples/basic`](../examples/basic/main.go)。

理解执行路径时，按 **`run.go` → `generation.go` / `tool_execution.go` → `session_store.go`** 阅读。理解 Context 时，按 **`rollout.go` → `session_history.go` → `context.go` → `compaction.go`** 阅读。

| 文件 | 职责 |
| --- | --- |
| `agent.go` / `config.go` | Agent 生命周期、默认配置冻结、显式配置变更 |
| `session.go` | 会话身份、独占维护操作与取消 |
| `session_store.go` | 原子提交、丢失确认核对、Reload |
| `session_history.go` | Rollout 读取、树查询、Head、Branch / Fork |
| `session_input.go` | Steer / Follow-up 的持久队列 |
| `session_approval.go` | 审批请求、等待和决定 |
| `session_recovery.go` | 中断恢复、未知效果查询及宿主确认 |
| `run.go` | 唯一的 Start / Continue 执行驱动 |
| `model.go` / `message.go` | Model 接口、函数适配器、消息与内容类型 |
| `generation.go` | 请求准备、完整回复提交、Retry / Fallback |
| `tool.go` / `tool_execution.go` | 工具契约、批次调度和持久执行阶段 |
| `tool_hooks.go` / `tool_result.go` | Hook 覆盖语义、效果与交付预算 |
| `environment.go` | 完整环境快照、固定 Binding、环境刷新 |
| `rollout.go` / `rollout_validation.go` | Store 契约、持久条目信封及树完整性校验 |
| `context.go` / `budget.go` | 唯一的历史到 Request 投影和预算入口 |
| `compaction.go` / `summary.go` | 历史选择、摘要输入适配、摘要请求和原子 Checkpoint |
| `events.go` | 不阻塞执行的实时订阅 |
| `schema.go` / `json.go` | Schema 编译和内部 JSON 辅助函数 |

Core 只依赖标准库和 JSON Schema 编译器；没有对宿主业务、Provider SDK、Runtime 后端或 UI 的反向依赖。

## 验证命令

```sh
go test ./...
go test -race ./...
go vet ./...
go run ./examples/basic
go run ./examples/approval
go run ./examples/environments
go run ./examples/tools
```

Core 集成测试位于 `tests/integration`，使用内存依赖、真实临时文件、受控并发和可注入故障，不需要网络或模型账号。标准工具测试在对应包中，覆盖 Pi 参考样例、文件与图片处理、真实 Shell / PTY、并发读取和资源回收。Shell 测试由测试自身启动并回收进程，不需要预先运行后端服务；Unix 之外跳过本地命令测试。

## 主要验收场景

| 测试文件 | 验证内容 |
| --- | --- |
| `run_test.go` | 聊天持久化、Steer / Follow-up、安全检查点、慢订阅者与单活动写者 |
| `model_test.go` | 不完整响应、Retry / Fallback、元数据保留和模型切换 |
| `tool_test.go` | 并行结果顺序、参数拒绝、Terminate、外部 Schema 引用拒绝 |
| `recovery_test.go` | 审批恢复、已存授权复用、开始但结果未知的工具、执行完成但交付未提交、确认丢失、Hook 错误 |
| `history_environment_test.go` | 全树查询、Head 持久化、分支配置隔离、Fork 位置、部分批次拒绝、环境前缀稳定、顺序屏障、重新发现 |
| `budget_compaction_test.go` | 单条与累计输出预算、完整工具批次压缩、大窗口不裁剪、小窗口裁剪副本、巨大用户输入、摘要失败、原子环境重申 |
| `lifecycle_test.go` | Close / Cancel、单请求超时、并发上限、确认结果预算、Hook 覆盖语义、摘要期间模型选择 |
| `store_test.go` | 两种 Store 的隔离/原子性/CAS、多句柄写入、崩溃尾部、完整提交损坏、非法工具消息时序 |
| `standardtools_test.go` | Core 中真实命令的原始目标与审批绑定、切换屏障、后端不确定效果 |
| `standardtools/*_test.go` | Pi 语义样例、分页、编辑、图片、Codex 等待和输出、PTY、取消、并发续读、进程回收、异常后端 |
| `standardtools/local/*_test.go` | 路径展开、文件别名、符号链接与文件名兼容 |

恢复测试会保存精确中断点的 Rollout，并用它创建新的 Agent，验证决策和副作用次数。JSONL 测试真实截断最后一个提交，分别验证未提交尾部和已提交损坏的不同处理。

## 当前边界

这是 SDK 的 v1 实现，模块地址为 `github.com/oines/astral-core`，项目自身的开源许可证尚未指定。第三方适配代码遵循仓库中的对应许可证与声明。示例模型用于离线验证接线与语义，不代表真实 Provider 兼容性认证。

JSONL 是单进程所有权的本地参考实现；读取和校验会扫描会话历史。它没有跨进程锁、数据库索引、后台快照或压缩归档。需要更大规模时，可在保持 Store 契约的前提下接入数据库。

Provider Token 计数、跨 Provider Reasoning 转换和权限判断由适配器承担。标准工具负责有界输出、会话管理和清理调度；Backend 负责终止目标上的实际进程，宿主负责在正确生命周期调用 `Set.Close`。本地进程后端目前支持 Unix，macOS 上执行测试，其他平台可提供自己的 Backend。Core 对已知预算溢出、消息配对、已提交事实和不确定效果提供统一控制，不推断外部世界状态。
