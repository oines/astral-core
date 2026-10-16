# Rollout / Session Fork：版本 3 验收与性能

本文保留 2026-10-14 的 V3 历史测量，数值与原始输出未改写。当前消息协议及持久化格式已升级为 V5；以下结果不代表 V5 的重新测量。

日期：2026-10-14。契约见 [contracts.md](contracts.md)，离线用法见 [Fork 示例](../examples/fork/main.go)。本轮变更限于 Session、Store、测试、示例和文档；Core 源码没有修改。

## 实施与验收

- Rollout 拥有历史；Session 拥有独立执行状态。默认 Shared 只新增 Session 和创建记录，Detached 复制所选路径。稳定目标 ID 提供创建幂等。
- Store 在原子提交范围内分配 Rollout Sequence，再 Reduce 和保存 Receipt；Session 根据已确认区间 Reduce。确认丢失、兄弟提交交错、旧回执遇到新所有者的行为均有回归测试。
- Child 从 Projection 初始化，本地创建版本为 1。Detached 祖先保留原身份、写入 Session、版本、顺序及协议数据，通过导入路径保存，不参与 Child 本地提交回放。
- 完整工具批次可 Continue，历史 Tool / Hook 不重跑，Terminate 保留。不完整批次和未解决效果在创建前拒绝。配置、压缩、环境重新确认、Context 预算、队列与生命周期隔离均有覆盖。选择环境切换工具的结果节点时，同样保留快照效果并要求重新确认。
- SQLite / JSONL 各覆盖分页复制中、发布前、发布后的真实 SIGKILL，以及同身份清理重试、参数冲突和已发布目标保护。原工具副作用恢复的 14 个 SIGKILL 场景继续通过。
- 跨句柄并发创建、运行中 Parent 与 Child 同时执行、关闭 Parent 后 Child 继续、冷回放与 Checkpoint、一百万历史的有界读取、Detached 脱离源 Rollout、复杂协议数据冷重开保真均已验证。
- 归档中单条记录超过默认 4 MiB 复制页时，单独读取并完整复制；三后端测试确认它不会被裁剪，也不会重新进入已压缩的有效 Context。

最终检查均通过：

```sh
go test -race ./...
go vet ./...
GOTOOLCHAIN=go1.23.12 CGO_ENABLED=0 go test ./...
```

五个离线示例 `basic`、`approval`、`environments`、`tools`、`fork` 均实际运行成功。审批示例副作用计数为 1；环境示例仍保持原进程绑定；Fork 示例验证 Shared / Detached 的 Rollout 关系及独立生命周期。[验收输出](benchmarks/2026-10-14-rollout-validation.txt)。

## 测量方法

Apple M2，8 个逻辑 CPU，24 GiB RAM，macOS 15.7.5（24G624），darwin/arm64；Go 1.26.3、modernc.org/sqlite v1.38.2。SQLite 使用 WAL、synchronous=FULL、5 秒忙等待和有限连接页缓存。

夹具通过公开 Store.Commit，每批 256 条写入 10,000 / 100,000 / 1,000,000 条固定长度历史消息。摘要覆盖旧消息，固定保留 16 条近期消息；Session 缓存 19 条记录，投影为 17 条模型消息。模型与估算器离线确定，不调用 Provider。

- 热操作各测 20 次，Detached 每个规模测 1 次；夹具构造不计入 ns/op。数值为本机测量，不是延迟保证或吞吐 SLA。
- Append 追加一条运行记录。MultiSessionCommit 使用 8 个独立 Session 并发提交，报告总耗时除以总提交数；包含协调开销，不是单次请求的尾延迟。
- 热 OpenSession 在已打开的 Store / Manager 上加载并关闭 Session。ColdOpenSession 每次新建 SQLite Store 句柄和 Manager，随后加载并关闭；它测量新连接与上下文加载，**没有清空 OS 文件缓存，也不是全新进程或物理冷盘测量**。
- Shared / Detached 均包括边界校验、有效 Context 预算检查和 Child 加载。Shared 不复制祖先；Detached 导入全部祖先历史。
- Go 堆峰值每 1 ms 采样，包含采样开销，可能遗漏更短峰值。RSS 是操作批次前后的 `ps` 样本，不是每操作峰值；整进程最大 RSS 另由 `/usr/bin/time -l` 报告，包含夹具构造。
- `db-bytes/op` 是 SQLite 页数增量乘页大小，包含持久索引；`file-growth-B/op` 是数据库、WAL、SHM 的实际文件长度净增量。WAL 复用时文件增长可以为零，仍然发生持久写入。
- `RowsRead` 为返回给适配器的历史 / 状态行，包含分页预读；不代表 SQLite 内部扫描次数，也不计标量元数据探测。查询计划另行验证。

原始数据：[热操作](benchmarks/2026-10-14-rollout-hot.txt)、[Detached](benchmarks/2026-10-14-rollout-detached.txt)、[查询计划](benchmarks/2026-10-14-rollout-query-plan.txt)。

## 耗时

除 Detached 使用秒，其余均为毫秒 / 操作。

| 历史消息 | Append | 热 Open | 新句柄 Open | PrepareContext | Shared | 8 Session Commit | Detached（秒） |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 10,000 | 0.189 | 0.330 | 0.741 | 0.163 | 0.817 | 0.238 | 0.410 |
| 100,000 | 0.355 | 0.323 | 0.769 | 0.151 | 0.835 | 0.271 | 4.558 |
| 1,000,000 | 0.397 | 0.301 | 0.847 | 0.143 | 0.861 | 0.310 | 49.205 |

Shared 三个规模均读取 **41 行**，累计分配约 **222 KB / 次**，SQLite 持久页增量约 **2.2–2.7 KB / 次**；没有复制完整前缀。热 / 新句柄 Open 均读取 20 行；PrepareContext 不读取存储；Append 与多 Session Commit 各读 1 行当前状态。常驻 Context 一直为 19 条记录。

这些结果支持 Shared 的成本取决于有效 Context、创建批次与必要索引访问。耗时仍受 fsync、检查点和索引页访问影响，不能从一次测量推导固定毫秒上限。

## 分配、内存与存储

B/op 是单次操作的累计分配，**不是同时保留的内存**。RSS 列为批次开始 → 结束；所有内存单位 MiB 使用 2²⁰ 字节。

### 10,000 条历史消息

| 操作 | 分配 B/op | allocs/op | Go 堆峰值 MiB | RSS MiB（前 → 后） | DB 增量 B/op | 文件净增量 B/op | 读取行数/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Append | 14,592 | 263 | 2.785 | 30.02 → 30.03 | 204.8 | 0.0 | 1 |
| OpenSession（热） | 107,403 | 912 | 4.505 | 30.23 → 30.25 | 0.0 | 0.0 | 20 |
| PrepareContext | 77,881 | 667 | 4.037 | 30.33 → 30.36 | 0.0 | 0.0 | 0 |
| Shared | 221,575 | 2,101 | 4.755 | 30.36 → 30.52 | 2,458.0 | 0.0 | 41 |
| 8 Session Commit | 17,658 | 292 | 2.859 | 30.53 → 30.53 | 819.2 | 5,120.0 | 1 |
| OpenSession（新句柄） | 131,081 | 1,210 | 4.578 | 30.53 → 30.56 | 0.0 | 0.0 | 20 |
| Detached | 45,498,312 | 605,973 | 5.577 | 29.64 → 30.28 | 18,677,760.0 | 18,610,648.0 | 10,082 |

### 100,000 条历史消息

| 操作 | 分配 B/op | allocs/op | Go 堆峰值 MiB | RSS MiB（前 → 后） | DB 增量 B/op | 文件净增量 B/op | 读取行数/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Append | 14,772 | 270 | 2.820 | 31.50 → 31.50 | 409.6 | 100,966.0 | 1 |
| OpenSession（热） | 107,350 | 912 | 4.559 | 31.52 → 31.52 | 0.0 | 0.0 | 20 |
| PrepareContext | 76,987 | 666 | 3.697 | 31.52 → 31.52 | 0.0 | 0.0 | 0 |
| Shared | 221,844 | 2,102 | 4.841 | 31.52 → 31.52 | 2,253.0 | 0.0 | 41 |
| 8 Session Commit | 17,806 | 294 | 2.937 | 31.53 → 31.53 | 1,024.0 | 5,325.0 | 1 |
| OpenSession（新句柄） | 130,953 | 1,209 | 4.741 | 31.53 → 31.53 | 0.0 | 0.0 | 20 |
| Detached | 453,152,952 | 6,110,224 | 5.870 | 32.34 → 32.50 | 188,825,600.0 | 190,985,032.0 | 100,433 |

### 1,000,000 条历史消息

| 操作 | 分配 B/op | allocs/op | Go 堆峰值 MiB | RSS MiB（前 → 后） | DB 增量 B/op | 文件净增量 B/op | 读取行数/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Append | 14,786 | 270 | 2.923 | 33.47 → 33.47 | 204.8 | 96,666.0 | 1 |
| OpenSession（热） | 107,124 | 911 | 4.325 | 33.48 → 33.48 | 0.0 | 0.0 | 20 |
| PrepareContext | 76,219 | 666 | 3.783 | 33.48 → 33.48 | 0.0 | 0.0 | 0 |
| Shared | 221,260 | 2,101 | 4.716 | 33.50 → 33.50 | 2,662.0 | 0.0 | 41 |
| 8 Session Commit | 17,779 | 295 | 3.020 | 33.52 → 33.52 | 1,024.0 | 5,530.0 | 1 |
| OpenSession（新句柄） | 130,918 | 1,208 | 4.710 | 33.52 → 33.52 | 0.0 | 0.0 | 20 |
| Detached | 4,537,826,720 | 62,252,959 | 6.246 | 32.92 → 30.88 | 1,888,940,032.0 | 1,888,362,688.0 | 1,003,949 |

百万历史 Detached 用时 **49.205 秒**，累计分配 **4.538 GB**，采样 Go 堆峰值 **6.246 MiB**，数据库页增量约 **1.759 GiB**。复制成本随历史量增长；分页只限制同时处理的工作集，不会消除全部历史的累计复制量。

热操作整进程最大 RSS 为 **33.70 MiB**，Detached 整进程为 **33.19 MiB**。它们包括全部三个规模的夹具构造，不是某一个 Fork 独占的峰值内存。

## 独立进程冷启动

另用全新子进程分别测量三个规模，每个规模 3 次；构造夹具后先关闭父进程的 Store。每个子进程从空的 Store / Manager / Session 缓存开始。OS 文件缓存仍可能命中刚写好的夹具，这不是物理冷盘测量。

“首次打开”覆盖 SQLite.Open、Manager 创建、Session 加载与关闭；子进程总时间还包含 Go / testing 启动、GC、RSS 采样子命令及结果传输。分配是子进程内首次打开期间的增量；堆和 RSS 是子进程完成打开后的样本，不能与包含夹具构造的主 Benchmark 进程峰值混为一谈。

| 历史消息 | 首次打开 ms | 子进程总时间 ms | 分配 B/op | allocs/op | 子进程堆 MiB | 子进程 RSS MiB | 读取行数 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 10,000 | 1.926 | 14.720 | 266,003 | 2,894 | 2.537 | 17.49 | 20 |
| 100,000 | 1.958 | 14.884 | 266,565 | 2,897 | 2.542 | 17.70 | 20 |
| 1,000,000 | 2.121 | 14.856 | 266,864 | 2,897 | 2.543 | 17.56 | 20 |

新进程首次打开仍只读取 20 行；原始数据见 [冷启动输出](benchmarks/2026-10-14-rollout-cold.txt)。

## SQLite 查询计划

历史路径使用有界递归链，并由链中的 ID 对 `entries(rollout_id,id)` 做主键定位。实际计划包含三次 `SEARCH e USING INDEX sqlite_autoindex_entries_1 (rollout_id=? AND id=?)`；`SCAN c` 和排序仅作用于最多一页加一条预读的递归结果，未扫描整个 entries 表。

| 查询 | 有序索引 |
| --- | --- |
| 历史类型 | `entry_kind(rollout_id,kind,seq)` |
| 仅历史记录 | `entry_history(rollout_id,seq)`，仅索引有 Projection 的记录 |
| 直接子节点 | `entry_parent(rollout_id,parent_id,seq)`，仅索引有 Projection 的记录 |
| 写入 Session | `entry_writer(rollout_id,session_id,seq)` |

上述筛选的 EXPLAIN 回归断言禁止全 entries 扫描和无界临时排序。主键路径读取、筛选索引和实际 RowsRead 共同支撑百万历史的有界读取结论。

## 复现

```sh
go test -c -o /tmp/astral-rollout-bench.test ./tests/integration
/usr/bin/time -l /tmp/astral-rollout-bench.test -test.run='^$' -test.bench='BenchmarkSQLiteLongHistory/.*/(Append|OpenSession|PrepareContext|Shared|MultiSessionCommit|ColdOpenSession)$' -test.benchtime=20x -test.benchmem -test.timeout=30m
/usr/bin/time -l /tmp/astral-rollout-bench.test -test.run='^$' -test.bench='BenchmarkSQLiteLongHistory/.*/Detached$' -test.benchtime=1x -test.benchmem -test.timeout=30m
/usr/bin/time -l /tmp/astral-rollout-bench.test -test.run='^$' -test.bench='BenchmarkSQLiteColdProcess' -test.benchtime=3x -test.timeout=30m
go test ./store/sqlite -run 'TestHistoryPathPlan|TestRolloutHistoryFilters' -v
```

## 剩余限制

- 格式升级为 3，旧格式明确拒绝；没有迁移器、旧 API 兼容层、Rollout / Session 删除 API 或 GC。
- 宿主保证每个 Session 和同一 Fork 目标的跨进程执行 / 创建所有权；JSONL 要求单进程独占目录。父子等待、取消联动、汇总、配额和外部资源隔离由宿主管理。
- 百万历史性能数据针对 SQLite。Memory / JSONL 的历史索引与冷回放允许随历史增长；本轮没有它们的百万历史性能测量。
- Detached 的内存峰值还受最大单条记录影响；固定小记录的 Benchmark 不能预测大图片、巨大 Schema 或大型工具批次的绝对内存。
- 未执行物理断电、清空 OS 缓存后的冷盘测试、Linux / Windows 全仓验收或真实 Provider 集成。SIGKILL 验证不等于断电认证。

变更保留在本地工作区，没有 push 或发布。
