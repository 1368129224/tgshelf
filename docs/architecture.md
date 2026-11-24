# 架构

本文描述代码的实际结构：现在长什么样、为什么这样切。

一句话：**Bot 交互层不认识 MTProto，下载编排层不认识 Bot API，装配都在 `cmd/tgshelf`。**

## 1. 分层

```
┌────────────────────── cmd/tgshelf（进程入口 + 装配）──────────────────────┐
│ main.go         flag 解析、配置加载/校验、日志、单实例锁、state、重连循环   │
│ bot_download.go 把 Pipeline + TaskRunner 缝成 bot.DownloadFunc           │
│ login_cmd.go    -login  （手机号→验证码→2FA，终端交互）                    │
│ resolve_cmd.go  -resolve（打印媒体报告）                                 │
│ download_cmd.go -download（无 bot 的直连下载，排障/压测用）                │
│ sessions_cmd.go -sessions（account.getAuthorizations，排查会话）          │
└───────────────────────────────┬────────────────────────────────────────┘
                                │ 注入接口，不注入 gotd 类型
┌───────────────────────────────▼──── internal/bot（Telegram Bot API 层）──┐
│ bot.go        轮询接线、handler 注册、启动通知                            │
│ handlers.go   文本/链接处理、标签键盘、任务启动、终态消息                   │
│ admin.go      /status /cancel /show_config /sync /login /logout          │
│ wizard.go     /login 状态机（TTL 10 分钟，纯 Go 基础类型）                 │
│ auth.go       白名单 / 管理员判定                                        │
│ keyboards.go  inline keyboard + callback_data 编解码（<64 字节）           │
│ shortid.go    长链接 → 短 ID 的 LRU 注册表（按钮里只放短 ID）              │
│ manager.go    任务队列 + max_concurrency 信号量 + 取消                     │
│ progress/     进度渲染（15 格条 + 速度 + ETA）与 3 秒节流编辑              │
└───────────────────────────────┬────────────────────────────────────────┘
                                │ 只认这几个接口：DownloadFunc / LoginAPI /
                                │ SyncFunc / AccountFunc / LogoutFunc
┌───────────────────────────────▼──── internal/tg（MTProto 用户端）────────┐
│ client.go     gotd 客户端构造、中间件链、多 DC 连接池、重连参数            │
│ identity.go   接入身份（tdesktop/custom）→ api 凭据与设备指纹              │
│ session.go    gotd SessionStorage 的单文件 JSON 实现（0600 + flock）      │
│ login.go      sendCode / signIn / checkPassword（DC 迁移、FLOOD_WAIT）    │
│ resolve.go    t.me 链接纯函数解析（公开/私有/评论/话题）                   │
│ peer.go       peer 解析 + dialogs 同步 + access_hash 缓存                 │
│ pipeline.go   link → peer → message → MediaItem 列表（含相册展开）        │
│ media.go      gotd 消息 → MediaItem；相册（grouped_id）展开               │
│ download.go   位图驱动分片内核（自写，见第 5 节）                          │
│ task.go       任务编排：预检、文件并发、失败隔离、汇总                    │
│ refresh.go    file_reference 过期刷新（singleflight）                     │
│ errors.go     RPC/FS/网络错误 → 中文可读原因                              │
│ middleware.go floodWait 中间件（可中断的 sleep）                          │
│ proxy.go      proxy_url → dialer / HTTP 客户端                            │
└───────────────────────────────┬────────────────────────────────────────┘
                                │
        ┌───────────────────────┴────────────────────────┐
        ▼ 复用的上游实现                          ▼ 本仓库自写
  iyear/tdl/core/dcpool        多 DC 连接池       internal/state   任务 + 续传位图 + 实体缓存
  iyear/tdl/core/middlewares   重试 / 恢复        internal/naming  模板渲染 + 清洗 + 安全拼接
  gotd/td                      MTProto 协议本体   internal/fs      原子写、flock、磁盘空间
  go-telegram/bot              Bot API 客户端     internal/config  TOML + env + 校验
                                                 internal/logging zap + 轮转 + 脱敏
                                                 internal/tg 的下载内核与媒体归一化
                                                 （download.go / media.go）
```

依赖方向铁律（review 时按这一条卡）：

- `internal/bot` **不得** import `github.com/gotd/td` 或 `github.com/iyear/tdl/core`。
  它只用 `internal/tg` 里的纯数据类型与工具（`TaskProgress`、`Translate`、`NormalizePhone`），
  以及注入进来的函数。这样 bot 的全部交互逻辑可以对着 fake 做零网络单测。
- `internal/tg` **不得** import `internal/bot`。它不知道"聊天"的存在。
- 只有 `cmd/tgshelf` 同时认识两边，所以"接线"只有一处。

## 2. 目录结构（实际）

```
cmd/tgshelf/                    入口与 CLI 子命令
internal/config/             TOML 加载、默认值、env 覆盖、校验、默认配置模板
internal/bot/                Bot API 层
internal/bot/progress/       进度渲染与节流
internal/tg/                 MTProto 层
internal/state/              任务状态机、续传会话（.part/.state）、实体缓存
internal/naming/             文件名模板、清洗、SafeJoin
internal/fs/                 原子写、flock、磁盘空间检查
internal/logging/            zap + 1MB×3 轮转 + 敏感词脱敏
deploy/                      Dockerfile（多阶段、distroless）+ 部署说明
docs/                        使用文档
```

## 3. 数据流

### 3.1 一条消息 → 一次下载 → 一个文件

```
用户消息 "https://t.me/xxx/123 https://t.me/c/456/7"
  │
  ├─ bot/handlers.go  extractLinks()            （中文标点已处理）
  │    每条链接 → shortid.Put() → 发一条「请选择下载标签」+ 分页键盘
  ▼ 用户点标签（callback_data: t:<shortID>:<page>:<tagIdx>）
  │
  ├─ bot/handlers.go  startDownload()
  │    taskID = chatID + 纳秒时间戳
  │    发占位消息「⏳ 正在准备下载…」（带「取消」按钮）
  │    state.Task{PENDING} 落库 → Manager.Submit()
  ▼ Manager 拿到 max_concurrency 槽位
  │
  ├─ cmd/tgshelf/bot_download.go  注入给 bot 的 DownloadFunc
  │    pipeline.ResolveLink(link)
  │      ├─ resolve.ParseLink               纯函数；公开/私有/评论/话题
  │      ├─ peer.Resolve                    username 解析或 access_hash 缓存
  │      ├─ messages.getHistory(offset=id+1) 反查单条，校验 msg.ID
  │      ├─ media.MediaFromMessage          → MediaItem
  │      └─ ExpandAlbum                     grouped_id ≠ 0 时展开整组
  │    naming.Render(template) + SafeJoin    每个 item → 安全相对路径（<tag>/<name>）
  │    fs.EnsureDir(root) + fs.CheckSpace    预检：≥ 总字节 × 1.2，任何文件产生之前
  │
  ▼
  ├─ tg.TaskRunner.Run(ctx, taskID, items, progress)
  │    文件级并发（file_concurrency 信号量）→ 每个文件 runItem()
  │      ├─ skip_same：同名同大小 → 直接 DONE
  │      ├─ state.Open(root, relPath, meta)  → .parts/<name>.part + .state
  │      │    三校验（size/chunk/doc_id）不一致 → 丢弃旧状态重下
  │      ├─ sess.Flush()                     预落盘，保证失败也有 .state
  │      ├─ ChunkPlan{Size, 1 MiB, bitmap}.Missing()
  │      ├─ download.go  N 个 worker 取缺失分片
  │      │    每片：getFile(offset, ≤1MiB) → WriterAt → 位图置位 → OnChunk
  │      │    5 秒 / 32 片触发一次原子落盘
  │      │    FILE_REFERENCE_* → refresh.go 重拉消息 → 只重试当前片
  │      └─ sess.Commit()                    fsync → rename(.part → final) → chtimes
  ▼
  ├─ state.Task{DONE|FAILED|CANCELLED} 落库
  └─ bot 终态消息（编辑占位消息）：成功耗时 / n/N 成功 / 中文失败原因 + 重试按钮
```

### 3.2 进度

`TaskRunner` 把每个分片完成的字节数累加进 `atomic.Int64`，并通过容量 128 的
`chan taskEvent` 汇总到**单个 goroutine** 串行回调（回调不被假定线程安全）。
bot 侧 `progress.Editor` 负责：

- 采样算 EWMA 速度，`Render()` 生成"文件名 + [n/N] + 15 格进度条 + 百分比"，
  再追加速度与剩余时间；
- 编辑节流：距上次编辑 < 3 秒直接丢弃；**文件完成数变化或终态时跳过节流**，
  否则小相册的 `[n/N]` 递进会被吞掉；
- 进度消息上始终带「取消」按钮（`EditMessageText` 不带 `reply_markup` 会掉按钮）。

### 3.3 相册、私有频道、讨论组

| 链接形态 | 解析路径 |
|---|---|
| `t.me/<username>/<id>` | `contacts.resolveUsername` → `InputPeerChannel/User/Chat` |
| `t.me/c/<channel>/<id>` | 用 `channelID` 查缓存（`-100…` 前缀在这里换算）；miss 时 `channels.getChannels(hash=0)`；都失败 → 提示「请先加入该频道，然后发 /sync」 |
| `t.me/<user>/<topic>/<id>` | topic id 丢弃，只取最后一段消息 ID |
| `…?comment=<id>` | 先解析主频道 → `channels.getFullChannel` 取 `linked_chat_id` → 在讨论组里取消息 |
| 相册（`grouped_id ≠ 0`） | `messages.getHistory(offset=msgID+11, limit=20)` 取同组消息，按 ID 升序展开 |

`/sync` 走 `messages.getDialogs` 翻页（每页 100，全量最多 50 页），把
`(peerID → accessHash, title, kind)` 写进 `data/state.json` 的实体缓存，
TTL 由 `cache_dialogs_hours` 控制（默认 6 小时）。

### 3.4 关键词搜索批量下载（/search）

`/search <channel> <keyword>` 是独立于链接流程的第二入口，两阶段确认：

1. **检索**：`internal/tg/search.go` 把频道参数（`@user` / `t.me/user` / `t.me/c/<id>` / 数字 ID）解析成
   `MessageRef` 后走共用 PeerResolver；然后 `messages.search` 携带
   `filter=InputMessagesFilterPhotoVideo` **服务端过滤**（只返回既匹配关键词又含图片/视频的消息，
   不需要全量拉历史），每页 100 条、`OffsetID=上一页最小 ID` 向前翻页，页间 sleep 500ms 防
   FLOOD_WAIT，默认 200 条封顶（超出标记 truncated）。命中的相册（`grouped_id ≠ 0`）用 3.3 的
   ExpandAlbum 补齐同组媒体。
2. **确认与下载**：`internal/bot/search.go` 异步执行检索（占位消息 → 统计面板：命中消息数、媒体数、
   总字节），结果存进程内 `SearchStore`（LRU 64 条），`search:<id>` 合成链接走**共用**标签键盘 →
   `state.Task{Link:"search:<id>"}` → `cmd/tgshelf/bot_download.go` 检测该前缀后跳过
   ResolveLink，直接从缓存取 `[]MediaItem` 进入既有下载内核（过滤、命名、分片续传全复用）。
   搜索结果只在内存中，重启后需重新搜索。

## 4. 存储

| 文件 | 内容 | 保护 |
|---|---|---|
| `data/session.json` | gotd `session.Data` 的 JSON（DC、AuthKey、Salt、Config） | `0600`；加载时校验 `sha1(AuthKey)[:8] == AuthKeyID`，不匹配报"已损坏，请重新登录" |
| `data/session.json.lock` | session 的进程级 flock | 防两个实例同时用同一 auth key（`AUTH_KEY_DUPLICATED`） |
| `data/session.lock` | 整进程单实例锁 | 防同一 `data/` 被两个进程使用 |
| `data/state.json` | 任务列表 + 实体缓存 | 原子写（temp + fsync + rename） |
| `data/tgshelf.log` | 运行日志 | 1 MB × 3 轮转；`bot_token`/`api_hash`/`proxy_url` 自动脱敏 |
| `downloads/<tag>/<file>` | 成品 | 先写 `.parts/<file>.part`，完成后 rename |
| `downloads/<tag>/.parts/<file>.state` | 位图 + `{v,size,chunk,done,doc_id,peer,msg_id}` | 成功后删除（`keep_parts` 可留）、失败保留 |

## 5. 自写下载内核与上游复用

业务代码真正 import 的上游包只有 `gotd/td`（MTProto 协议本体）、`go-telegram/bot`
（Bot API 客户端），以及 `github.com/iyear/tdl/core` 的 `dcpool`（多 DC 连接池）与
`middlewares/{retry,recovery}`（重试 / 恢复）。连接池的池化、回收、DC 迁移与退避逻辑
竞态多，这两块直接复用经过验证的实现。

下载内核（`internal/tg/download.go`，约 440 行）是自写的：上游下载器按"多个文件"迭代，
没有分片位图、也无法跳过已完成分片，满足不了本项目的断点续传与"n/N 汇总"。自写版本
从位图取缺失分片、N 个 worker 并发、每片最多 3 次重试（200/400ms 退避）、失败即停派发
并等在途请求返回，`ctx` 取消可被 `errors.Is(err, context.Canceled)` 判定。

`core/downloader` 与 `core/tmedia` 仍留在 `go.mod`，并由 `internal/tg/deps_check.go`
用符号锚定，防止 `go mod tidy` 误删依赖、上游改 API 时也能第一时间编译报错；它们都不在
下载热路径上。

实现细节：

- 分片大小固定 1 MiB（服务端硬限制；实测 1 MiB 比 512 KiB 快约 1.7 倍，因为请求数减半）。
- 进度是"位图 + 原子字节计数"：`ChunkPlan.Missing()` 只产出缺失分片，
  `MarkDone`/`Flush` 由 `state.Session` 加锁串行化，5 秒或 32 片落一次盘。

## 6. 并发模型

```
进程
├─ bot 轮询 goroutine（go-telegram/bot 的长轮询）
├─ gotd 客户端 goroutine（连接、ping、更新分发）
├─ 重连循环（client.Run 返回 → 指数退避 1s→60s 封顶 → 重建客户端）
└─ 每个任务一个 worker（Manager 的 max_concurrency 信号量）
     └─ 每个文件一个 goroutine（file_concurrency 信号量）
          └─ threads 个分片 worker（按文件大小自适应，1/2/4/8/threads）
```

- 连接池大小 = clamp(`threads × file_concurrency`, 8, 32)。
- 并发压力 ≈ `max_concurrency × file_concurrency × threads`；`FLOOD_WAIT` 时先降
  `file_concurrency`（见 [faq.md](faq.md)）。
- 取消是 `context` 传播：`/cancel` → `Manager.Cancel` 置状态 → 取消 ctx → 下载内核
  停止派发、在途请求返回、`.part/.state` 保留。

## 7. 失效与恢复

| 场景 | 机制 |
|---|---|
| 网络抖动 / `pingLoop: disconnect` | `client.Run` 返回后外层指数退避重建客户端（**不退出进程**）；只有 SIGINT/SIGTERM 才结束 |
| `FLOOD_WAIT_x` | 中间件按服务端给的秒数 sleep（可被 ctx 中断）；登录流程则把秒数直接回给用户 |
| `FILE_REFERENCE_EXPIRED/INVALID` | `refresh.go` 用 singleflight 重拉消息换 Location，只重试当前分片，不清位图 |
| 进程被杀 | 已置位分片在 `.state` 里（5 秒 / 32 片落盘），重启后只补缺失分片；任务状态启动时把遗留 RUNNING 收尾成 FAILED |
| 同名文件冲突 | 目标已存在且大小不同 → 加 `(1)`、`(2)` 后缀占名 |
| 磁盘不足 | 下载前 `CheckSpace` 要求 ≥ 总量 × 1.2；运行期 `ENOSPC` 翻译成中文并失败 |

## 8. 安全

- **路径穿越**：模板渲染 → `Sanitize`（剔除 `/ \ : * ? " < > |`、控制字符、首尾点号）
  → `SafeJoin`（拒绝绝对路径、`..`、NUL、超长）→ 只有校验通过才创建目录/文件。
- **凭据**：session `0600`、`data/` `0700`；日志脱敏；`/show_config` 绝不回显
  `bot_token`/`api_hash`，代理只显示 `scheme://***`。
- **单实例**：`flock` 两个锁（进程 + session），重复启动直接报中文错误而不是互相踩。
- **容器**：distroless static、`USER nonroot`、无 shell/包管理器；只暴露一个二进制。
- **入站**：`allowed_user_ids` 白名单、`admin_user_ids` 管理命令隔离；未知命令不执行任何动作。

## 9. 构建与体积

- `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w'` → 约 45 MB 静态二进制
  （主要是 `gotd/td` 的生成代码）。
- 镜像基于 `gcr.io/distroless/static-debian12:nonroot`，约 49 MB，`linux/amd64` + `linux/arm64`。
- CI：`go vet` → `go test -race` → `golangci-lint` → 静态构建（超 10 MB 只告警）→ buildx 多架构。

## 10. 延伸阅读

- [configuration.md](configuration.md) — 配置项与默认值
- [login.md](login.md) — 登录、身份、session
- [faq.md](faq.md) — 常见故障
- [performance.md](performance.md) — 实测吞吐与调优
