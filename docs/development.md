# 代码导航与验证

## 结构

```text
astral-core/
├── session.go, session_config.go      # 直接创建/打开 Session 与生命周期
├── run.go, execution_state.go         # 唯一执行循环和当前工具批次
├── input.go                          # 稳定输入 ID、索引回执与持久队列
├── prepare.go                        # 预览、准备、预算与原子边界提交
├── compaction.go, summary.go          # 已提交历史摘要草稿
├── generation.go, model_recovery.go   # 生成/摘要共用尝试、超时、退避与修复
├── tool_execution.go, tool_hooks.go   # 工具调度、审批与结果处理
├── session_recovery.go               # 未知效果与中断恢复
├── state.go, entry.go, store.go       # 精确状态、纯 Reduce、存储契约
├── session_store.go, context.go       # 提交确认、有效窗口与投影
├── session_history.go                # 分页历史与 Shared/Detached Fork
├── events.go                         # 事件和原子运行快照
├── internal/streamcheck/              # 每次 Attempt 的内容索引校验
├── store/memory/, store/sqlite/       # 同一 Session 的两种后端
├── standardtools/                    # 文件、命令、进程与本地 Backend
├── tests/integration/                # 公开 API、真实存储与崩溃测试
└── examples/                         # 六个可运行离线示例
```

执行路径从 `run.go → prepare.go → generation.go / tool_execution.go` 阅读。提交路径为 `Session → Store.Commit → Reduce + 原子保存 → Receipt → Session 更新缓存`。Store 实现依赖根包契约，根包不依赖 Store 后端或 Provider SDK。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go test ./store/sqlite ./tests/integration
```

跨 Astral、agent-core、astral-models 调整 API 时，在包含三个 checkout 的 Go workspace 中验证全部模块，重新生成 Astral OpenAPI / TypeScript SDK，并运行前端 typecheck、test、lint 和全新数据目录验收。发布独立模块前再更新下游版本约束。

集成测试使用受控模型以及真实临时文件、SQLite、Shell / PTY，不需要模型账号。新格式拒绝旧数据，不进行迁移；测试不得复用用户的实际数据目录。

## 验收覆盖

| 测试 | 契约 |
| --- | --- |
| `session_contract_test.go` | Prompt Busy、稳定 ID、队列只入队、显式 Continue、请求 Context 独立 |
| `prepare_atomic_test.go` | 摘要排除新输入、环境顺序、完整原子提交、预算/回调/写入失败、丢失回执、并发接收/清除、冷重开 |
| `retry_contract_test.go` | 普通生成与摘要冻结请求、三次总预算、永久错误/超时、Retry-After 与取消、事件和模型隔离 |
| `context_injection_test.go` / `steering_test.go` | 多模态、FIFO、完整投递组、被动上下文、清除、预算及两后端一致性 |
| `model_recovery_test.go` / `model_snapshot_test.go` | 受限协议修复、实际请求审计、冻结附件、不重复工具、只支持新格式 |
| `execution_semantics_test.go` / `lifecycle_test.go` | Continue、串行预检、不可变目标、平衡终止事件、关闭与模型选择竞争 |
| `tool_control*_test.go` | EndRun 原子完成、已排队独立输入、恢复、Fork、投影与未知控制效果 |
| `crash_test.go` / `recovery_test.go` | 子进程确定屏障强杀，审批、Started/Completed/Result 和未知副作用恢复 |
| `state_store_test.go` / `store_test.go` | Memory/SQLite 回执、分页、CAS、非法批次、格式标记与 checkpoint 一致性 |
| `rollout*_test.go` / `fork_concurrent_test.go` | 共享顺序、包含式边界、Fork 幂等、独立生命周期、Detached 无源恢复 |
| `store/sqlite/*_test.go` | 分页复制竞争、真实进程崩溃、发布与清理、查询计划 |
| `budget_compaction_test.go` | 完整批次、多模态摘要、独立摘要预算、裁剪副本与有界自动压缩 |
| `scale_test.go` / `cold_start_test.go` | 固定有效窗口、百万级历史夹具、新进程打开、索引读取上限 |
| 标准工具包 | Pi 参考样例、真实文件/图片、命令、PTY、原目标绑定和清理 |

崩溃测试通过子进程屏障定位中断点，效果计数写入独立文件。故障注入验证提交前失败与提交后回执丢失，不能仅用成功路径或内存模拟代替。

历史性能测量见 [benchmarks.md](benchmarks.md)，尚未以格式 9 重跑全套百万记录 Benchmark。离线模型和协议夹具也不代表真实供应商账号或全部操作系统认证。

模块地址为 `github.com/oines/astral-core`，使用 [Apache-2.0](../LICENSE)。标准工具参考来源见 [第三方声明](../THIRD_PARTY_NOTICES.md)。
