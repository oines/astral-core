# 可选标准工具

`standardtools` 实现五个工具的行为；`standardtools/local` 提供直接可用的本地 IO。Core 不依赖这两个包。宿主可以只使用自己的 `tools/` 包，也可以从 `Set.Tools()` 中选择工具并与业务工具组合，名称不可重复。

```go
backend, err := local.New(local.Options{
    CWD: preparedWorkspace,
    Shell: local.DefaultShell(),
})
if err != nil { return err }
set, err := standardtools.New(backend)
if err != nil { return err }
defer func() {
    cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = set.Close(cleanup)
}()
config.Tools = set.Tools()
```

完整的可运行代码见 [tools 示例](../examples/tools/main.go)。模型是离线确定性适配器，文件和命令执行是真实的。

## 与参考实现的对应

本轮以本地源码为准：Pi `a4d040d30f2cb30fffbd841b3f0a7052158e2e21`，Codex `af5d95f255`。参考来源及许可证在 [第三方声明](../THIRD_PARTY_NOTICES.md)。

| 工具 | 对齐行为 |
| --- | --- |
| `read` | Pi 的 1 起始行号、offset/limit、2000 行 / 50 KiB 的完整行截断、继续读取提示、图片附件 |
| `write` | Pi 的创建父目录、完整覆盖、按文件串行变更 |
| `edit` | Pi 当前 `edits` 数组、对同一原文匹配、唯一性与重叠检查、Unicode 模糊匹配、保留未修改行、BOM / 换行符、diff / patch |
| `exec_command` | Codex 的 `cmd/workdir/shell/login/tty/yield_time_ms/max_output_tokens`、默认值和等待范围、退出码或数字会话 ID |
| `write_stdin` | Codex 的 `session_id/chars`、空输入轮询、分段增量输出、按进程串行交互、非 PTY 输入限制及 Ctrl-C |

编辑和文本截断的预期结果由上述 Pi 源码生成，保存在 [testdata](../standardtools/testdata/README.md)，Go 测试不需要 Pi 或 Node。

这是工具语义与模型接口的对齐。保留以下明确边界：

- 不内置 Codex 的沙箱、授权策略、网络审批、设备协议或应用 UI。宿主通过 Backend、Hook 和环境提供这些能力。
- 不提供 Pi 的 `bash`；该位置由命令会话工具替代。没有在模型 schema 中加入 Codex 特定的 `sandbox_permissions` 等宿主策略字段。
- `edit` 发布当前数组 schema，不接受 Pi 为旧客户端提供的单组 `oldText/newText` 兼容输入。工具定义版本为 `2`；持久 Session 使用的声明必须与宿主实现匹配。当前存储格式不支持旧日志自动迁移。
- 匹配与修改行为有参考样例验证；Go diff 库可能为重复行选择不同但等价的 diff。Go 图片编码器的字节和压缩质量也不会与 Pi 完全相同。
- 本地 Shell / PTY 后端支持 Unix 和带 ConPTY 的 Windows。Windows 默认使用已安装的 `$SHELL`，否则优先 PowerShell 7，再使用系统 Windows PowerShell；可通过 `local.DefaultShell()` 取得实际选择的可执行文件。其他平台仍需提供原生或远程 Backend。标准工具不读取环境中的平台字段，平台要求由宿主 Backend 处理。

### 输入声明与普通调用结果

下表的 `?` 表示可省略，数字参数均使用整数。默认值由执行器填入，不是 JSON Schema 的 `default` 字段。

| 工具 | 必填参数 | 可选参数与默认值 |
| --- | --- | --- |
| `exec_command` | `cmd: string`，非空 | `workdir?: string`、`shell?: string`；`login?: boolean = true`；`tty?: boolean = false`；`yield_time_ms?: integer = 10000`；`max_output_tokens?: integer = 10000` |
| `write_stdin` | `session_id: integer >= 1` | `chars?: string = ""`；`yield_time_ms?: integer = 250`，空输入时最低等待 5000ms；`max_output_tokens?: integer = 10000` |
| `read` | `path: string`，非空 | `offset?: integer >= 1`，默认 1；`limit?: integer >= 1`，未提供时不施加调用级行数限制 |
| `write` | `path: string`，非空；`content: string`，允许空字符串 | 无 |
| `edit` | `path: string`，非空；非空 `edits` 数组，每项包含非空 `oldText: string` 和允许为空的 `newText: string` | 无 |

五个输入 schema 均设置 `additionalProperties: false`。相较 Codex 的 `number` 声明，SDK 使用更准确的 `integer` 和非负范围；Codex 的命令执行器本身也按整数解析这些字段。相较 Pi，SDK 明确限制正行号、非空路径和非空编辑数组；不自动转换字符串数字、可选 null 或异常的编辑输入。Pi 额外接受旧的顶层 `oldText/newText`，并会修正字符串形式或单对象形式的 `edits`。

参考实现为参数逐字段提供 description；当前 SDK 将说明放在工具整体 description 中。完整工具声明保存在 Session 配置中，Core 按完整 JSON 核对，因此补充参数 description 也需要考虑既有会话的声明更新，不能仅将它视为不影响兼容性的文字修改。

普通调用时，模型接收 `ToolResult.Content`；`Details` 用于宿主展示和持久化，不自动进入模型上下文。SDK 没有为这五个工具声明独立的输出 schema；Codex 和 Pi 为程序化调用提供的输出 schema 不改变此处普通调用的文本与图片约定。

| 场景 | 模型收到的 Content | Details |
| --- | --- | --- |
| 命令完成 | 状态头、`Process exited with code N`、输出；非零退出码也属于正常工具返回 | `chunk_id / wall_time_seconds / exit_code / original_token_count / output` |
| 命令仍在运行 | 状态头、`Process running with session ID N`、本次新增输出 | `session_id` 代替 `exit_code`，其他字段同上 |
| 命令输出被截断 | 状态头、原始 Token 数、截断警告和头尾正文；发生缓冲省略时始终保留字节数 | `output` 与模型收到的正文一致 |
| `read` 普通文本 | 文本；调用级 limit 提前结束时附继续读取的 offset | 无 |
| `read` 达到行数或字节上限 | 完整行、截断提示和继续读取的 offset；首行过长则返回 shell 检查提示 | `truncation`，包含总行数/字节数、保留量、限制与截断原因 |
| `read` 图片 | 图片说明及 image 内容块；处理失败时返回省略原因 | 无；图片尺寸信息位于 image 内容块的 Metadata |
| `write` 成功 | `Successfully wrote to PATH`，与 Pi 相同 | 无 |
| `edit` 成功 | `Successfully replaced N block(s) in PATH.`，与 Pi 相同 | `diff / patch / firstChangedLine`，与 Pi 相同的字段；重复行的等价 diff 分组可能不同 |

缺失文件、越界 offset、编辑不匹配/不唯一/重叠等错误，由 Core 转成带 `IsError` 的文本结果。SDK 还记录 `Code` 和执行事实 `Outcome`（`succeeded / failed / not_executed / unknown`）；Pi 的普通错误主要是错误文本和 `isError`。这些宿主字段不应与进程退出码混用。图片无法解码或超过解码上限时 SDK 设置 `IsError`，Pi 的图片处理失败分支返回省略文字，未显式标记工具错误。

## 文件工具

```json
{"path":"src/main.go","offset":1,"limit":100}
{"path":"notes.txt","content":"new contents\n"}
{"path":"src/main.go","edits":[{"oldText":"old block","newText":"new block"}]}
```

`read` 默认从第 1 行开始，未指定 `limit` 时读取至结尾，再应用 2000 行 / 50 KiB 上限。它不把超长第一行切成不完整代码，而是说明该行超过限制。带分页或截断的结果包含后续 offset；超出文件末尾的 offset 报错。

图片按文件内容识别 PNG、JPEG、GIF、WebP、BMP。默认最大边长 2048×2048，并受最多 2500 个 32×32 patch 约束，base64 内容最多 4.5 MiB，解码上限 4000 万像素；宿主可用 `Options.Images` 调整。Pi 的无模型配置默认边长为 2000×2000，也可从当前模型读取缩放限制；SDK 使用宿主传入的固定选项。BMP 转为 PNG；需要压缩时可能转为 JPEG，动画图片缩放时只保留解码帧。JPEG 缩放前处理 EXIF 朝向。模型 Content 包含文本提示和 `image` 内容，`Data` 是 base64 字符串的 JSON 值；Model 适配器负责映射至 Provider 协议。`DisableResize` 保留图片原尺寸和编码大小，但仍检查解码像素上限。

`edit` 先验证整组修改，再调用一次 `WriteFile`。所有 `oldText` 都针对原文件，不能依赖同批前一条修改的结果。缺失、歧义、重叠、空 oldText 或整体没有变化都会报错。先精确匹配，再按 Pi 规则处理 NFKC、行末空白、智能引号、横线和特殊空格。模糊匹配只改变涉及的行，其他行保留原字节；BOM 和原始 CRLF 格式恢复后写入。

`write/edit` 通过 `ResolvedPath.Key` 共享进程内变更队列，不强制所有文件工具串行。这个队列不锁住外部编辑器，也不是跨进程文件锁。宿主的环境切换工具应声明 Sequential，Core 会让其所在批次顺序执行。

本地路径支持相对 / 绝对路径、`~`、`@file`、`file://`、粘贴的特殊空格，以及截图文件名的 AM/PM、NFD 和弯引号变体。符号链接别名共享队列。写入遵循 Pi 的原地覆盖，跟随符号链接并保留已有权限；不是文件系统事务。所有匹配成功才开始写入，实际 IO 失败仍可能产生部分变更，此时返回未知效果。

Windows 同时支持盘符、UNC、`~\`、`file:///C:/...` 与 `file://server/share/...` 路径。路径由执行所在机器解析；远程 Backend 必须返回目标系统的 `Parent` 和 `Key`，宿主不能用自己的路径规则重算。Windows 环境变量覆盖忽略键名大小写，保留驱动器当前目录变量。

## 命令工具

```json
{"cmd":"go test ./...","workdir":"/project","yield_time_ms":1000,"max_output_tokens":2000}
{"cmd":"python3 -q","tty":true,"login":false,"yield_time_ms":250}
{"session_id":12345,"chars":"print(1 + 1)\n","yield_time_ms":250}
{"session_id":12345,"chars":"","yield_time_ms":5000}
{"session_id":12345,"chars":"\u0003"}
```

| 参数或限制 | 行为 |
| --- | --- |
| `shell` | 优先显式参数，否则使用宿主 Backend 为固定 Binding 解析的 Shell |
| `login` | 默认 true；Unix Shell 使用 `-lc`，false 使用 `-c` |
| `tty` | 默认 false，使用合并 stdout/stderr 的管道且关闭 stdin；true 分配 PTY |
| `workdir` | 默认 Backend 为固定 Binding 解析的 cwd；相对路径相对该 cwd 解析 |
| exec 等待 | 默认 10000ms，约束到 250–30000ms |
| 非空 stdin 等待 | 默认 250ms，约束到 250–30000ms |
| 空 stdin 轮询 | 最低 5000ms，默认最大 300000ms，可用 `MaxPollWait` 调整 |
| `max_output_tokens` | 默认 10000，并受 Set 上限约束；显式 0 不保留原始输出正文，但仍返回截断提示 |
| 活跃进程数 | 默认最多 64；满时回收最久未用且当前没有交互的会话，全部忙则拒绝新启动 |
| 输出缓冲 | 每个进程未读输出、每次调用收集输出各默认最多 1 MiB，保留头尾并标记省略 |

等待窗口结束会返回数字 `session_id`，不会终止命令。`write_stdin` 只交付上次取走之后的新输出；各进程可以并行，同一进程的读写串行。完成结果包含 `exit_code`，该 ID 随后不可再次使用。输出正文采用 Codex 的约 4 字节 / Token 估算，按 UTF-8 边界保留头尾，插入 `…N tokens truncated…`。普通返回按 Set 的 Token 上限增加 20% 序列化余量，并为状态头和截断提示预留空间；较小的调用级预算仍限制正文。缓冲区省略字节数独立保留，后续截断不会抹掉该提示。精确 Provider 预算由 Session 的独立 TokenEstimator 检查。

非 PTY 会话拒绝普通输入，但单独的 Ctrl-C 可发送中断。Unix 使用进程组信号；Windows 使用每个命令的私有 ConPTY 控制通道，不修改宿主进程的控制台。Windows 管道模式仍向子进程提供真实的 stdout/stderr 管道和 stdin EOF。PTY 可用于交互式程序；Windows 会将输入 LF/CRLF 转成控制台 Enter 的 CR。默认本地 PTY 为 80×24；没有终端 UI 或窗口尺寸同步服务。

模型 Content 使用 `Chunk ID`、`Wall time`、退出码或 `Process running with session ID`、原始 Token 数及 `Output` 文本。结构化副本保存在 `ToolResult.Details`：

```json
{"chunk_id":"abc123","wall_time_seconds":0.25,"session_id":12345,"original_token_count":120,"output":"new output"}
```

`session_id` 与 `exit_code` 互斥。与 Codex 的普通调用一致，非零退出码是正常的工具返回，不设置 `IsError`；命令是否成功由 `exit_code` 表达。已知执行完成的 `Outcome` 为 `succeeded`，表示命令已经执行，不应因退出或交付错误被自动重跑。执行或输出收集故障仍设置 `IsError`；无法确认 Wait 状态时记录 `OutcomeUnknown`，保留资源用于清理。无输出时结构化结果仍包含 `original_token_count: 0`。

## 生命周期与环境绑定

一个 `Set` 可以跨多个 Run 和 Session 复用，每个终端会话有自己的执行所有者。Invocation.Binding 是宿主定义的 opaque JSON；标准工具只检查 JSON 有效性，将解析和授权交给 Backend。`write_stdin` 在审批之前找到原始绑定，执行之前再次验证。切换当前环境不会把输入发到另一台机器；Fork 不获得源会话的终端使用权。

命令不会随着一次工具调用的 Context 结束而被自动杀死。取消等待会返回已经收集的输出和已知的活跃 ID；后续可继续读取。取消一个正在阻塞的 PTY 写入可能已提交部分输入，本地后端会终止该进程并报告未知效果，避免返回后继续暗中写入。

进程会话保存在 Set 内存中。它们不会通过持久历史在进程重启后自动重新建立，也不会重跑旧命令。重启后的旧 ID 返回不可用；持久 Session 按 started / completed 事实处理恢复，不自动重跑未知效果的工具。Standalone Core 只保留进程内状态。

宿主关闭使用该工具集的 Session 后，应在自己的清理 Context 中调用 `Set.Close`。它取消待处理调用、终止所有拥有的进程并等待 IO 工作者退出，可以再次调用。`Session.Close` 不替宿主关闭 Set 或 Backend。Unix 本地后端清理 Shell 的进程组；主动创建新会话逃离进程组的程序，需要宿主 Runtime 的容器 / 作业管理机制约束。Windows 在 Shell 恢复执行前将其放入 Job Object，终止时清理整个 Job；Shell 正常退出也清理后台子进程，宿主崩溃后由关闭 Job 句柄触发收尾。

[environments 示例](../examples/environments/main.go) 使用宿主自定义 Binding 和同机上的两个目录模拟两个目标，演示切换后新命令使用新目标，真实 PTY 仍归属原始目标。环境切换是示例中普通的宿主 Sequential Tool，选择 Schema、发现及授权均由示例宿主实现；SDK 不提供 SwitchEnvironment 或设备选择接口。

## 接入其他 Runtime

`Backend` 只需提供两种能力：

```go
FileSystem(ctx, invocation) (FileSystem, error)
StartProcess(ctx, invocation, request) (Process, error)
```

`FileSystem` 提供路径解析、原始字节读取、写入和创建目录。路径的 Parent 由目标后端计算，不假定宿主与目标使用相同路径规则。Key 必须在路径别名间稳定，并能区分不同文件系统。IO 方法返回前必须真正结束；取消时提前返回却继续写入，会破坏变更队列的互斥保证。

`Process` 提供合并输出流、一次 Wait、Write、Interrupt 和 Terminate。返回成功后的进程必须独立于启动 Context 生存。非零退出码不是 Go error；无法确认退出状态才是 error。Terminate 必须停止它拥有的进程树并使 Output / Wait 结束，且遵守传入 Context。Set 管理数字 ID、所有权、输出缓冲和轮询，不要求远程后端重新实现整套工具逻辑。

本地 Backend 在创建时固定默认 cwd、Shell 和环境变量。未配置解析回调时，仅空 Binding 可以使用这些默认值，任何非空 Binding（包括 JSON null）都会在 IO 前被拒绝。

宿主可以提供 `local.Options.ResolveBinding`，签名为 `func(context.Context, astral.Invocation) (cwd, shell string, err error)`。该回调接收隔离复制的 Binding，负责解析、授权，并返回完整的执行设置：cwd 必须为已存在的绝对目录，Shell 必须非空。返回错误、缺少字段或目录无效都会拒绝，绝不回退到默认目录和 Shell。显式命令参数的 workdir / shell 保留现有语义，权限及访问边界由宿主检查。Backend 不解释目标身份、发现设备或解析环境描述。
