# 代码导航与验证

## 结构

```text
astral-core/
├── agent.go, run.go                # 独立 Agent 与唯一执行循环
├── generation.go, tool_execution.go # 模型与工具调度
├── driver.go, memory_driver.go     # 必要策略接点和默认进程内状态
├── model.go, message.go, tool.go   # Provider 中立协议
├── events.go, config.go           # 非阻塞事件与执行配置
├── internal/retry/                # 生成与摘要共用 Retry / Backoff
├── internal/streamcheck/          # 每次 Attempt 的轻量内容索引校验
├── environment/                   # 宿主 Snapshot：模型 Context 与 opaque Binding
├── session/
│   ├── manager.go, session.go     # 持久会话所有权与生命周期
│   ├── driver.go, run.go          # Core 适配与恢复预检，无第二套循环
│   ├── state.go, entry.go         # 精确状态、纯 Reduce 与历史记录
│   ├── store.go, session_store.go # 新 Store 契约、确认核对、窗口缓存
│   ├── session_history.go        # Rollout HistoryReader、Shared / Detached Fork
│   ├── session_input.go          # 持久队列
│   ├── session_approval.go       # 审批门
│   ├── session_recovery.go       # 中断恢复和宿主效果确认
│   ├── context.go, budget.go     # 有效 Context 与目标模型预算
│   └── compaction.go, summary.go # 摘要与读取边界
├── store/
│   ├── memory/, jsonl/           # 轻量后端
│   ├── internal/ledger/          # Memory / JSONL 的内存索引
│   └── sqlite/                   # 索引查询、原子事务和分页 Fork
├── standardtools/                # 文件、命令和进程会话
│   ├── local/                    # 本地文件、管道和 PTY
│   └── testdata/                 # Pi 参考行为样例
├── tests/integration/            # 从宿主公开 API 验证语义
├── examples/                     # 五个可运行的离线示例
└── docs/
```

接入从 [独立 Core 示例](../examples/basic/main.go) 和 [持久审批示例](../examples/approval/main.go) 开始。执行路径按 `run.go → generation.go / tool_execution.go → Driver` 阅读；持久路径按 `session/driver.go → session_store.go → Store.Commit（分配 Sequence、Reduce、原子保存）→ Receipt → Session Reduce` 阅读。

Core 的直接第三方依赖只有 JSON Schema 编译器；只复用 environment.Snapshot 数据类型，不包含环境发现，也不导入 Session、存储、Provider SDK 或标准工具。Session 通过组合 Core 加入策略，Store 实现反向依赖 session.Store 契约。

## 验证命令

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go test ./store/sqlite ./tests/integration
GOTOOLCHAIN=go1.23.12 CGO_ENABLED=0 go test ./...
go run ./examples/basic
go run ./examples/approval
go run ./examples/environments
go run ./examples/tools
go run ./examples/fork
```

集成测试不需要模型账号或网络。模型为受控实现，文件、SQLite、JSONL 和 Shell / PTY 测试使用真实资源。Go 1.23 工具链及模块首次安装需要网络；下载后测试可离线运行。Unix 外会跳过或明确拒绝本地 Shell / PTY 功能。

## 关键验收

| 测试 | 验证 |
| --- | --- |
| 根目录 `message_test.go` | 唯一有序 Content、调用提取、统一校验、扩展类型、签名与大整数、旧字段明确拒绝 |
| `memory_driver_test.go`（根目录）/ `core_environment_test.go`（integration） | Standalone Snapshot 分离、非法更新拒绝、连续调用绑定、完整 Request 不泄露私有值、字节前缀 |
| `steering_test.go` | 默认逐条 / 显式批量、配置边界、Core / Session 一致性、FIFO、Metadata、预算、三后端原子提交 / 确认丢失 / 冷重开 / Cancel |
| `ordered_content_test.go` | 交错流式索引、提交前不派发、乱序结果排序、Retry / Fallback 隔离、三后端冷重开 / 压缩 / Fork / 恢复 |
| `core_test.go` | 仅 Model / Tools 启动；Core 与 Session 的请求、事件、Retry、输入、顺序、并行上限一致；取消不重跑 |
| `crash_test.go` | SQLite / JSONL 在七个确定屏障强杀子进程；持久效果与 Hook 计数证明无重复副作用 |
| `recovery_test.go` | 审批恢复、固定授权、未知效果、完成但缺输出、丢失确认 |
| `state_store_test.go` | 三后端回执、分页、取消、非法批次；增量 / 全回放 / Checkpoint 一致；旧格式逐字节不变；多模态 / Reasoning / Metadata 冷重开与 Fork |
| `rollout_test.go` / `rollout_durable_test.go` | 共享并发 Sequence、按 Session 回放、边界矩阵、工具结果、创建幂等、独立生命周期、Detached 无源恢复 |
| `store/*/rollout_crash_test.go` | SQLite / JSONL 分页中、发布前后 SIGKILL 与清理重试 |
| `fork_concurrent_test.go` | Parent 运行与排队期间 Fork；固定读取边界、Child 隔离、不复制操作状态；错误所有权回执不授权执行 |
| `store/sqlite/fork_test.go` | 分页中间暂停时 Parent 仍能写；取消清理；复制中真实进程崩溃；路径查询计划无全历史扫描 |
| `scale_test.go` / `cold_start_test.go` | 一万 / 十万 / 百万历史 Benchmark；固定有效窗口；新进程冷打开、Open 读行数、Context 保留记录数和零历史读取准备 |
| `lifecycle_test.go` | Close / Cancel、创建中的环境发现、卸载与重开、并发上限、Hook 效果不可改写 |
| `budget_compaction_test.go` | 工具输出与累计预算、完整批次摘要、裁剪副本、环境原子重申 |
| `history_environment_test.go` | 分支配置隔离、包含式 Fork 边界、不完整批次、环境重新发现 |
| `environment_contract_test.go` | 任意 JSON Binding、模型 / 摘要不泄露、隔离复制、失败切换、Compaction、Fork 与冷重开 |
| `standardtools/local/backend_test.go` | 宿主绑定解析、目标拒绝、完整执行设置和回调隔离 |
| `store_test.go` | 三后端隔离 / CAS / 原子性，JSONL 多句柄、未提交尾部和已提交损坏 |
| 标准工具包测试 | Pi 样例、真实文件与图片、命令等待、PTY、原目标绑定、进程清理 |

崩溃测试不依赖睡眠猜测中断点：子进程在指定事实之后发出屏障，父进程强制终止，再以新 Store 句柄读取。效果计数同步写入独立文件。普通恢复测试还使用故障注入检查提交前失败、确认丢失和版本竞争。

## Benchmark

```sh
go test ./tests/integration -run '^$' -bench 'BenchmarkSQLiteLongHistory/.*/(Append|OpenSession|PrepareContext|Shared|MultiSessionCommit|ColdOpenSession)$' -benchtime=20x -benchmem -count=1 -timeout=30m
go test ./tests/integration -run '^$' -bench 'BenchmarkSQLiteLongHistory/.*/Detached$' -benchtime=1x -benchmem -count=1 -timeout=30m
```

夹具通过公开 Commit 以 256 条批次写入，摘要后固定保留 16 条消息。1 万 / 10 万 / 100 万历史分别测量 Shared、Detached、多 Session Commit、Append、热 / 冷 OpenSession 和 PrepareContext；报告延迟、分配、Go 堆、RSS 样本、磁盘增量、读取行数。Detached 单独测量，以免重复复制大量数据影响热路径。环境、原始输出及限制见 [Benchmark 报告](benchmarks.md)。

## 范围

存储格式为 3，没有旧 API 兼容层、旧格式迁移、真实 Provider 转换器、分布式执行租约或后台 GC。Memory / JSONL 冷打开与历史内存用量允许随历史增长；长会话保证针对 SQLite。

模块地址为 `github.com/oines/astral-core`，项目使用 [Apache-2.0 许可证](../LICENSE)。标准工具参考来源及许可证见 [第三方声明](../THIRD_PARTY_NOTICES.md)。本地验证环境为 macOS / arm64，不能把离线协议测试视为真实 Provider 或所有平台的兼容性认证。
