# Core 离线增量迁移

Core 的 SQLite 数据格式版本独立于 Host 的应用版本。正常 `sqlite.Open` 与 Session 读取只接受 `core.FormatVersion`，不会在运行期间转换旧数据。Host 更新器使用以下独立入口：

```go
status, err := sqlite.InspectMigration(ctx, path)
// status.Current / Target 为 Core 数据格式版本；Missing 表示尚无数据库。
// NeedsMigration 也包括给合法格式 10 登记首次迁移账本。
err = sqlite.Migrate(ctx, path)
```

调用者先停止所有数据库使用者，并持有整个数据目录的进程所有权锁。检查、备份、迁移、恢复和启动新版本期间都不能释放这个所有权。Core 不代替 Host 管理进程，也不协调 Host 数据库或文件。

`InspectMigration` 不创建目标文件、父目录、迁移账本或原数据库的 SQLite 辅助文件。已完成 checkpoint 的库直接只读检查；存在非空 WAL 时，临时复制数据库和 WAL 后检查，防止忽略已提交的 WAL 或为检查修改原目录。临时副本在返回前删除，因此崩溃残留 WAL 的检查需要额外临时磁盘空间。此入口要求数据库内容在检查期间保持不变。

当前格式 10 是首个受支持的基线。合法的现有格式 10 只登记 Goose 账本，不改变 Session、历史、投影或 Attempt。早于 10 的未公开格式、未来格式、`format` 表不是恰好一行、`PRAGMA user_version` 不一致、损坏或非顺序账本都拒绝迁移。缺失数据库保持缺失，新数据库直接原子创建最新 schema 和完整账本。

最新版二进制内嵌 `store/sqlite/migrations/*.sql`，并显式注册 Go 迁移，不使用 Goose 全局注册。Goose 依次执行所有缺失步骤，不下载中间应用版本、不允许乱序或降级。每个迁移的数据变更、两个格式标记和账本记录处于同一个事务；失败步骤全部回滚，后续步骤不运行。之前已完成步骤保持提交，Host 可以恢复整套升级前备份，也可以在排除失败原因后重试。

## 新增格式的要求

1. 每次 Core 持久格式变化递增 `core.FormatVersion`，新增下一个连续编号的迁移，并同步修改新数据库的 schema。没有数据变化的应用发布不添加空格式版本。
2. 迁移一旦发布不得修改、删除或重新编号。修正迁移内容另增下一步。SQL 必须使用事务，不允许 `NO TRANSACTION` 或环境变量替换；Go 使用 `goose.NewGoMigration` 与 `RunTx`，通过 `migrationPlan.goMigrations` 显式注册。
3. 历史 Go 迁移使用对应版本的冻结结构和 SQL。不要用不断变化的最新 `core.Entry`、`RuntimeState` 或业务处理函数解码历史数据。涉及 Entry、Projection、State、Creation、引用摘要及校验信息时，必须维护它们之间的完整性与可重建性，不能只修改数据库表头的格式版本。
4. 两个数据库格式标记由迁移执行器在同一事务内推进，迁移本身不手写标记更新。只有真实数据转换成功后，才记录该步完成。
5. 使用旧格式夹具验证顺序补齐、冷重开、历史与 Fork、冻结请求重建以及失败恢复；仅验证 SQL 执行成功不等于数据语义正确。当前测试使用额外的临时 SQL/Go 步骤验证执行器，不宣称已支持不存在的旧格式转换。

Goose 只提供单个数据库的迁移执行。Host 必须在写入前备份数据库、WAL、密钥及必要配置；迁移后进行整体启动验证。多库升级失败时恢复一致的整套备份，不依赖 `Down` 撤销可能不可逆的数据转换。
