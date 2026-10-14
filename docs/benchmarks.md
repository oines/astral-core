# 分层与长会话重构：验证与 Benchmark

日期：2026-10-14。代码直接替换旧 API 和格式，入口与边界见 [README](../README.md)，持久语义见 [契约](contracts.md)。

## 实施结果

| 阶段 | 交付与验证 |
| --- | --- |
| 1：唯一执行器 | 独立 astral.Agent 与 session.Session 共用 Loop、工具调度、Retry、取消、输入边界及事件；公共请求、结果顺序、并发上限和事件测试通过 |
| 2：包与 API | 持久策略迁入 session，可选环境迁入 environment，TokenEstimator 与 Model 分离；标准工具及四个示例均已适配并实际运行 |
| 3：增量状态与 Store | RuntimeState / Reduce、分页历史、稳定提交回执、Memory / JSONL / SQLite、原子发布的流式 Fork；三后端契约与回放 / Checkpoint 一致性测试通过 |
| 4：验证与收尾 | 真实崩溃、并发 Fork、协议保真、百万历史性能、Go 1.23 无 CGO、race、vet 和文档完成 |

当前关键入口：`astral.New(Config)`、`session.New(Config)`、`Manager.NewSession/OpenSession`、`Session.History()`、`Head/SelectHead/Fork`。Core 不导入 Session、Store、Environment 或 TokenEstimator。没有第二套 Agent Loop；摘要与普通生成共用内部 Retry 实现。

## 已运行验证

- `go test -race ./...`：通过。
- `go vet ./...`：通过。
- `GOTOOLCHAIN=go1.23.12 CGO_ENABLED=0 go test ./...`：通过，包含 SQLite 和真实本地工具测试。
- 四个示例：通过。basic 仅 Model / Tool；approval 持久审批成功且副作用计数为 1；environments 保留原进程目标；tools 真实写入、编辑并读取文件。
- SQLite / JSONL 各 7 个 SIGKILL 场景：启动提交前、启动提交后、确认丢失、执行完成但未记录、completed 提交后、结果 Hook 后、最终结果提交后。通过同步副作用计数验证无重复派发 / Hook 重跑。
- SQLite 复制中 SIGKILL：冷重开时 Child 不可见；显式清理拒绝已发布会话。
- 三后端：Commit ID 幂等、摘要不匹配、当前版本冲突、分页 / 字节限制、取消、隔离、非法信封、回放与 Checkpoint、旧格式逐字节不变。
- 多模态、有序 Content、Tool Call ID、Reasoning、opaque Metadata（包括大整数）冷重开与 Fork 保真。
- 暂停 Fork 时 Parent 仍能生成、排队和提交；Child 不复制操作状态，Parent / Child 后续独立。
- SQLite 实际 Run 的请求准备不读旧历史：仅三个新增提交各读一行当前状态。

## 环境与方法

Apple M2，8 个逻辑 CPU，24 GiB RAM，macOS 15.7.5（24G624），darwin/arm64。Benchmark 使用 Go 1.26.3、modernc.org/sqlite v1.38.2，SQLite WAL + synchronous=FULL，本机临时文件。Go 1.23.12 另行执行无 CGO 全仓测试。

夹具通过公开 Store.Commit，每批 256 条写入 100,000 / 1,000,000 条历史消息。末尾摘要覆盖旧消息，保留 16 条近期消息；Session 常驻 19 条记录（近期消息、边界、摘要、配置），投影为 17 条模型消息。Model 和 TokenEstimator 均为确定性测试实现，没有真实网络调用。

- Append：新增一条运行记录的原子提交，历史窗口不增长。
- OpenSession：同一已打开 SQLite Store 上创建新 Session 句柄并 Close；不是清空 OS 缓存后的磁盘冷启动。
- PrepareContext：InspectContext 的真实投影和估算；不包含网络、自动摘要或额外预算检查的耗时。实际执行路径的零历史读取另有测试。
- Fork：完整复制所选历史路径，创建独立 Child，并加载其有效 Context。
- 热路径各 20 次操作；Fork 每个规模 1 次。数值是本机测量，不是 SLA。
- Go 堆峰值每 1 ms 采样，包含运行时与采样开销，可能漏掉更短的峰值。
- 读取行数是返回给适配器的行数。路径查询另有 EXPLAIN QUERY PLAN 回归测试，确保使用主键定位，不扫描全会话。

## 热路径结果

| 历史消息 | Append | OpenSession | PrepareContext | Open 读取行数 | Prepare 读取行数 | 常驻 Context 记录 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 100,000 | 0.145 ms | 0.271 ms | 0.144 ms | 20 | 0 | 19 |
| 1,000,000 | 0.152 ms | 0.273 ms | 0.141 ms | 20 | 0 | 19 |

| 历史消息 | 操作 | 累计分配 B/op | allocs/op | 采样 Go 堆峰值 MiB |
| ---: | --- | ---: | ---: | ---: |
| 100,000 | Append | 10,000 | 182 | 2.711 |
| 1,000,000 | Append | 10,162 | 183 | 2.760 |
| 100,000 | OpenSession | 95,091 | 889 | 4.290 |
| 1,000,000 | OpenSession | 95,125 | 889 | 4.265 |
| 100,000 | PrepareContext | 75,115 | 646 | 3.692 |
| 1,000,000 | PrepareContext | 76,003 | 647 | 3.777 |

Append 各读取一行当前状态。历史量增加十倍时，热路径的读取量、保留记录数和分配保持相同量级。[原始热路径输出](benchmarks/2026-10-14-hotpaths.txt)。

验证期间曾发现 SQLite 在只返回 20 行时仍扫描全部历史，OpenSession 从十万条约 46 ms 增至百万条约 452 ms。修复为有界递归链外层驱动的主键查找，并加入查询计划断言后，得到上表结果。

## Fork 结果

| 历史消息 | 时间 | 累计分配 B/op | allocs/op | 采样 Go 堆峰值 MiB | 读取行数 |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 100,000 | 2.972 s | 433,380,520 | 5,827,769 | 5.919 | 100,413 |
| 1,000,000 | 30.625 s | 4,332,381,912 | 58,271,775 | 6.042 | 1,003,929 |

这里的累计分配会被 GC 回收，**不是同时保留的内存**。百万条复制累计分配约 4.33 GB，采样 Go 堆峰值约 6.04 MiB。

Fork 使用预先编译的独立测试进程运行；`/usr/bin/time -l` 报告整次进程（包括两个规模的夹具构造）最大 RSS 为 35,045,376 字节，约 **33.42 MiB**。它不是某一次 Fork 独占的内存，也不是热路径的每操作内存。热路径日志中的 time 统计包含 go test 构建过程，不用于这一 RSS 比较。[原始 Fork 输出](benchmarks/2026-10-14-fork.txt)。

复制时间、磁盘占用和累计分配按历史量增长；Parent 的写锁只在短事务期间持有，临时复制页和 Child 的有效 Context 有界。

## 复现

```sh
go test ./tests/integration -run '^$' -bench 'BenchmarkSQLiteLongHistory/.*/(Append|OpenSession|PrepareContext)$' -benchtime=20x -benchmem -count=1 -timeout=30m
go test -c -o /tmp/astral-bench.test ./tests/integration
/usr/bin/time -l /tmp/astral-bench.test -test.run='^$' -test.bench='BenchmarkSQLiteLongHistory/.*/Fork$' -test.benchtime=1x -test.benchmem -test.timeout=30m
```

## 已知边界与未运行项目

SQLite 承担长历史保证；Memory / JSONL 的历史索引在内存中，JSONL 首次访问允许全量回放。Fork 是独立复制，仍有 O(K) 时间、磁盘和累计分配成本。崩溃残留的隐藏 SQLite 目标 / JSONL 临时文件需要宿主按生命周期清理，没有后台 GC。

没有真实 Chat Completions / Responses 适配器、旧格式迁移、共享历史前缀或分布式执行租约。宿主仍必须保证同一 Session 单执行所有者。

未运行真实 Provider、断电硬件测试、Linux / Windows 全仓测试或 OS 缓存清空后的存储 Benchmark；SIGKILL 测试不等同于物理断电认证。性能夹具的有效窗口固定且内容较小，不能用于预测大图片、巨大 Schema 或大型并行工具批次的绝对内存与延迟。
