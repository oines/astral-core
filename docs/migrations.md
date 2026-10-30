# Core 离线增量迁移

Core 逻辑格式、SQLite 物理 schema 和应用发布版本分别演进。当前逻辑格式为 **11**，SQLite schema 为 **12**。正常 `sqlite.Open` 校验格式、schema 与 Goose 账本，只接受当前布局，不在运行期间转换旧数据。

```go
status, err := sqlite.InspectMigration(ctx, path)
// Current / Target: Core 逻辑格式。
// SchemaCurrent / SchemaTarget: SQLite 物理版本。
// NeedsMigration: 物理布局或迁移账本尚未达到目标。
err = sqlite.Migrate(ctx, path)
```

调用者先停止所有数据库使用者，并持有整个数据目录的进程所有权锁。检查、备份、迁移、恢复和启动新版本期间都不能释放所有权。Core 不代替 Host 管理进程，也不协调 Host 数据库或文件。

`InspectMigration` 不创建目标文件、父目录、迁移账本或原数据库的 SQLite 辅助文件。已完成 checkpoint 的库直接只读检查；存在非空 WAL 时，临时复制数据库和 WAL 后检查。Host 必须在这一步之前检查临时卷空间，并保证源文件不再变化。缺失数据库保持缺失；新数据库直接原子创建最新 schema 和完整账本。

## 版本链

`format.version` 表示逻辑格式；`PRAGMA user_version` 表示物理 schema。Goose 账本是连续的物理迁移链，当前为 `0, 11, 12`。执行器逐步验证 schema 对应的逻辑格式：schema 11 和 12 均对应逻辑格式 11。当前程序拒绝格式 10 及更早版本、未来 schema、重复或缺失标记、损坏或不连续的账本。旧程序也会拒绝新的 schema 12，不能直接用旧程序打开升级后的数据库。

- 步骤 11：登记首次公开的 schema 11 基线，保持原数据。
- 步骤 12：按行转换为共享大字段的内部表示；`inputs` 和 `pending` 引用实际队列事件。每条记录先验证编码还原等价，输入与 pending 再验证实际引用。未知 envelope 字段保留，供应商不透明 JSON 不被递归拆解。历史身份、时间、顺序、正文、回执与冻结请求语义不变。

迁移内存随单条最大记录变化，不加载整个 rollout。整个步骤使用一个 Goose `RunTx`，逐行处理不等于逐行提交；日志、备份及临时空间仍可能较大。转换不重新下载媒体、压缩预览或重做 Compact。

最新版携带全部 SQL 与显式注册的 Go 迁移，不使用 Goose 全局注册，不下载中间应用版本。数据变更、逻辑/物理标记及账本在同一事务提交；失败步骤全部回滚，后续步骤不运行。之前已完成步骤保持提交，Host 可以恢复整套升级前备份或重试。

## 新增迁移

1. 物理布局变化递增 `sqlite.SchemaVersion`，增加连续步骤，并更新 fresh schema。只有公共逻辑数据契约变化才递增 `core.FormatVersion`；在 `migrationPlan.formats` 中明确 schema-only 步骤对应的逻辑格式。
2. 已发布步骤不可修改、删除或重新编号。SQL 使用事务且不允许环境变量替换；Go 使用 `goose.NewGoMigration` 的 `RunTx`。
3. `00012_migration.go`、`00012_codec.go`、`00012_payloads.go` 是步骤 12 的冻结转换与编码依赖，也受已发布迁移不可变检查保护。未来布局使用新的编号实现，不能修改这些文件来改变旧步骤的行为。正常运行复用当前版本编码，不包含旧布局读取分支。
4. 转换使用冻结的 JSON 字段语法、SQL 和 `RawMessage`，不调用最新 Session reducer 重放历史。数字、不透明字段和未知值不能被无意重编码或丢弃。
5. 格式标记由执行器与 Goose 账本一起推进；迁移不手写标记。用真实旧布局夹具验证冷重开、队列修订、请求恢复、Fork、取消、事务回滚和重复执行。

Goose 不提供跨数据库恢复。Host 必须备份数据库、WAL、密钥及必要配置，迁移后验证整体启动；多库升级失败时恢复整套备份，不依赖 `Down`。

## 空间维护

删除无引用载荷会产生 SQLite 空闲页，不承诺立即缩小文件。`sqlite.Vacuum(ctx, path)` 是独立离线入口：要求当前 schema、完整备份、所有权锁、关闭其他连接，并由调用者预检数据卷与 SQLite 临时卷空间。它在事务外执行 VACUUM，前后检查并完成 `wal_checkpoint(TRUNCATE)`，再做完整性检查。不自动删除备份、不转换旧库、不改变逻辑历史。
