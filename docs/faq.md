# 常见问题

先看一眼日志基本能定位一半问题：

```bash
docker compose logs --tail 100        # 容器
tail -50 data/tgshelf.log                # 本机运行
docker compose exec tgshelf /tgshelf -health  # 健康检查（未登录时也会返回 0）
```

---

## 1. `FLOOD_WAIT` 怎么办

**现象**：聊天里出现「被限流，需等待 N 秒后重试」/「非会员账号被限流…」；日志里
`FLOOD_WAIT_42` 之类的错误码。

Telegram 对单位时间内的 API 调用有配额，超了就返回 `FLOOD_WAIT_x`（x 秒内不要再发）。

本项目的处理：

- **普通下载/解析**：中间件会按服务端给的秒数 `sleep(x+1)` 后**自动重试同一次调用**，
  等待可被取消（`/cancel`、进程退出会立刻中断）。所以你在聊天里只会看到进度停一会儿，
  不会看到报错。
- **登录流程**：不静默等待，直接把秒数回给你（「被限流：请等待 N 秒后重试」），
  因为发码的等待可能长达几十分钟甚至更久，静默卡住只会让人以为 bot 死了。

你能做的：

1. **等**。`FLOOD_WAIT` 是账号/会话级别的，换 IP、换代理都没用；反复重试只会把等待时间越拉越长。
2. **降并发**。它是"请求太密"的直接结果：
   ```toml
   [download]
   threads = 2            # 4 → 2
   file_concurrency = 1   # 2 → 1
   [bot]
   max_concurrency = 1    # 3 → 1
   ```
   改完重启。
3. **别同时跑两个实例**。两个进程用同一个 token/账号会互相抢配额，还会触发
   「已有另一个实例在使用该 session」或 `getUpdates` 冲突。
4. 登录时发码被限流，只能等——同一个手机号短时间内反复 `/login` 会越来越久。

---

## 2. 私有频道打不开（`t.me/c/...`）

**现象**：

- 「该频道/群组是私有的：请先加入该频道，然后发送 /sync」
- 「无法识别该对话/频道：请先发送 /sync 同步会话后再试」
- 「无法获取频道 123456 的 access_hash: …」

**原因**：`t.me/c/<channel>/<msg>` 里只有频道 ID，没有 `access_hash`；而 MTProto 取消息
必须同时有这两个值。`access_hash` 只能从"你已加入的会话列表"里拿到。

**处理**：

1. 用**同一个账号**（就是 bot 登录的那个）在 Telegram 里加入该频道/群组；
2. 回到 bot 聊天发 `/sync`（仅管理员），等它回「同步完成，共 N 个会话」；
3. 重新发那条 `t.me/c/...` 链接。

补充：

- `/sync` 会翻页拉取会话列表（每页 100，最多 50 页 ≈ 5000 个会话），账号会话特别多时
  要等十几秒。
- 缓存有 TTL（`download.cache_dialogs_hours`，默认 6 小时）。过期后会重新解析，
  频道还在的话通常无感；不在（被踢/退群）就会报上面第一条。
- **评论链接**（`?comment=<id>`）要求频道开了讨论组（`linked_chat_id` 非 0），
  否则报「该频道没有关联的讨论组，无法解析评论链接」。
- 公开频道（`t.me/<username>/<id>`）不需要 `/sync`，直接 `contacts.resolveUsername`。

---

## 3. 下载慢（`threads` 怎么调）

先分清瓶颈在哪：

| 现象 | 说明 |
|---|---|
| 进度条几乎不动，日志里 `FLOOD_WAIT` 很多 | 请求太密，**降**并发（见第 1 节） |
| 单文件慢、多文件也不快 | 多半是账号级限速或代理带宽，加线程几乎没用 |
| 单个大文件慢、小文件正常 | 可以试着加 `threads`（大文件才会用满） |
| 只有某个频道慢 | 文件在别的 DC，连接池会直连，但仍受该 DC 链路影响 |

调参顺序（一次只改一项，改完重启）：

```toml
[download]
threads = 8           # 默认 4；1–32
file_concurrency = 2  # 默认 2；相册/多链接任务才用得上
[bot]
max_concurrency = 3   # 默认 3；同时几个任务
```

几个实现细节，能解释"为什么加了线程没变快"：

- 每次 `getFile` 最多 1 MiB，且不能跨 1 MiB 边界——这是 Telegram 的硬约束，
  `threads` 只是并发请求数，不是"单请求更大"。
- `threads` 是**上限**：`<1 MiB → 1`、`<5 MiB → 2`、`<20 MiB → 4`、`<50 MiB → 8`、
  `≥50 MiB → threads`。小文件加线程没有意义。
- 连接池大小 = `threads × file_concurrency`，夹在 8–32 之间；开太多只会更早触发限流。
- 分片大小默认 1 MiB。实测 1 MiB 分片比 512 KiB 快约 1.7 倍（请求数减半）；
  想改可以用 `-chunk-bytes`（必须整除 1 MiB），改了会让已有 `.parts` 因 chunk 校验不符而重下。

想量化对比，用 CLI 绕过 bot 直接下（会打印每个文件的 `getFile` 次数、耗时、平均速度）：

```bash
./tgshelf -config tgshelf_config.toml -download https://t.me/telegram/193 \
  -to /tmp/dltest -tag video -threads 8
```

如果 bot 里慢、CLI 里快，问题在 bot 侧并发配置（`max_concurrency` / 多条任务互相挤）；
两边都慢就是链路/账号限速。

---

## 4. 磁盘满

**现象**：

- 任务直接失败：「磁盘空间不足：请清理下载目录所在磁盘后重试」
- 日志 `ENOSPC`；容器重启循环里出现写日志失败

**机制**：每个任务在**写任何文件之前**会检查下载目录所在文件系统的可用空间
（要求 ≥ 任务总字节 × 1.2），不够就直接失败，不会留下半截文件；运行中遇到 `ENOSPC`
也会翻译成上面那句中文。

**处理**：

1. 看是谁占的：
   ```bash
   df -h .                       # 宿主机
   du -sh downloads/* | sort -h  # 各标签目录
   docker system df              # 镜像/容器占用的空间
   ```
2. 清掉不再需要的成品文件；**未完成任务的 `.parts/` 别删**（删了等于放弃续传）。
   确认放弃某个任务后再删对应 `.part` / `.state`。
3. `.part` 是**稀疏文件**：`ls -l` 显示的是完整大小，`du` 看实际占用才是真实磁盘消耗。
4. 长期方案：把 `download_path`（容器里就是 `./downloads` 挂载）换到大盘，
   或定期归档。
5. 注意 `data/` 也别撑爆：日志是 1 MB × 3 轮转，`state.json` 只存任务与实体缓存，正常很小。

---

## 5. 启动相关

### 容器起来就退出

按日志对号入座：

| 日志 | 原因 / 处理 |
|---|---|
| `config validation failed: … bot_token: must not be empty` | 配置里没填 `bot_token` |
| `generated default config … fill in bot_token and try again` | 首次运行生成了模板，编辑后重启 |
| `build Telegram bot client: … unauthorized` | token 写错/已失效，去 @BotFather 重新拿 |
| `create data directory: fs.EnsureDir: chmod "data": operation not permitted` | 宿主机 `data/`、`downloads/` 不是 uid 65532 的：`sudo chown -R 65532:65532 data downloads` |
| `load config: reading config file "tgshelf_config.toml": permission denied` | 配置文件对容器用户（65532）不可读：`chmod 644 tgshelf_config.toml`；想保持 600 就改成 `sudo chown 65532:65532 tgshelf_config.toml`（之后编辑要 sudo） |
| `another instance is already using this session … stop it first` | 同一 `data/` 被两个进程用；`docker ps -a` 清掉重复容器 |
| `not logged in: session file … not found, run tgshelf -login first` | CLI 子命令（`-download` / `-resolve` / `-sessions`）要求已登录的 session：先跑一次 `tgshelf -login`。bot 模式不受影响，可以未登录启动，在 Telegram 里发 `/login` 即可 |

### 状态是 `unhealthy`

健康检查做三件事：读配置、看磁盘（下载目录存在时要求可用空间 ≥ 10 MB，实际按 1.2 倍判定）、
看 session 文件（不存在算健康，因为"还没登录"是合法状态）。所以 `unhealthy` 通常意味着：

```bash
docker compose exec tgshelf /tgshelf -health   # 直接看它报什么
```

- `health check failed: cannot load config …` → 挂载路径/权限不对；
- `health check failed: not enough disk space …` → 见第 4 节；
- `health check failed: session file unreadable …` → `data/` 权限不对。

### bot 不回消息

1. token 对不对（`curl https://api.telegram.org/bot<token>/getMe`）；
2. 是不是有另一个实例在轮询同一个 token（Telegram 只允许一个 `getUpdates`，
   另一个会 409）；本项目加了 `flock`，所以更常见的是"旧容器还在跑"；
3. 你的 user id 是否在 `allowed_user_ids`（留空 = 所有人可用）；
4. 需要代理才能访问 Telegram 时，`proxy_url` 是否配置正确——它同时作用于 Bot API 与 MTProto，
   **容器里不能用 `127.0.0.1`**，要用 `host.docker.internal`（compose 示例已加 `extra_hosts`）。

---

## 6. 登录与权限

| 现象 | 处理 |
|---|---|
| `/login` 回 `Not allowed: /login is admin only.` | 把你的 user id 加进 `[bot] admin_user_ids`，重启 |
| `/login` 后发手机号没反应 | 向导 10 分钟没推进会过期；重新 `/login`。发手机号时不要带 `+` 以外的字符以外的奇怪符号（允许空格/`-`/`(`/`)`/`.`） |
| 2FA 密码忘了 | 只能走 Telegram 官方找回；项目无法绕过 |
| 登录成功但下载报 `login session is no longer valid: log in again (CLI: tgshelf -login)` | session 被服务端作废（例如在手机上「终止所有会话」），重新 `/login` |
| `/logout` 后还是能用 | `/logout` 只删本地文件，进程仍持连接；`docker compose restart` 后才真正未登录 |
| 想换一个账号 | 停 bot → 删 `data/session.json`（或 `-login` 时默认会备份旧文件）→ 重新登录 |

---

## 7. 下载/任务相关

| 现象 | 说明与处理 |
|---|---|
| `invalid message ID: check that the message the link points to exists, then send /sync again` | 链接指向的消息没了；检查链接，或频道已被删 |
| `this link has no downloadable media (probably a text or service message)` | 纯文本/服务消息/投票等；不是所有消息都有文件 |
| 相册只下了一部分，提示 `✅ Download complete, n/N succeeded, took …` | 单条失败不影响其它条目；失败的条目在终态消息里能看到原因，点 `Retry` 会整组重来（已完成的会被 `skip_same` 跳过） |
| 重试后从头开始下 | `.parts/` 被删了，或分片大小变了（`-chunk-bytes` 改了），或文件 size/doc_id 变了（消息被重新上传）；`.state` 校验不通过会主动丢弃并重下 |
| 成功后 `.parts/` 还留着 | 目录会保留，成功时删掉 `.state`；`keep_parts = true` 时会连 `.state` 一起留 |
| 同一链接点两次标签 | 会被去重挡住（同 chat + 同链接 + 同标签的非终态任务不会重复创建） |
| 重启后 `/status` 里有奇怪的"正在下载" | 启动时会把遗留任务收尾（RUNNING → 失败、PENDING → 取消），一般不会残留；若持续出现，抓 `data/tgshelf.log` |
| 取消后文件还在 `.parts` | `/cancel` 保留续传数据；确定不要了就手动删对应 `.part`/`.state` |

---

## 8. 其它

- **时区**：容器默认 `TZ=Asia/Shanghai`（compose 里可改）。文件 mtime 用消息时间，
  不信时区；日志时间戳受 `TZ` 影响。
- **arm64**：`deploy/Dockerfile` 支持 `linux/amd64` 与 `linux/arm64` 交叉编译，
  CI 也是双架构。
- **升级**：`docker compose up -d --build`。`data/` 与 `downloads/` 是挂载卷，不会被重建掉。
- **备份**：只需要 `tgshelf_config.toml` + `data/session.json`（外加你想留的下载文件）。
  `data/` 是 `0700`、归 65532，备份要 `sudo`。
- **多实例**：不支持。同一个 `data/` 会被第二把锁挡住；同一账号的多个实例还会互相抢
  `getUpdates`。要跑多个 bot 就复制一份目录、换 `bot_token`、换 `data/` 与 `downloads/`。

---

## 9. 凭据泄露了怎么办

**先吊销，再清理。顺序不能反。**

2026-11-04 本项目自己踩过一次：`internal/logging/logging_test.go` 的脱敏单测
直接把真实 `bot_token` 当输入值提交了，仓库是 public，GitHub secret scanning
立刻报警。事后复盘的处理顺序如下，可直接照做：

1. **立刻吊销**（这是唯一真正止损的一步）
   Telegram 找 `@BotFather` → `/revoke` → 选 bot → 拿新 token。
   代码清理再快也追不上爬虫——凭据一旦公开就要假定已被利用。
   吊销后可用 `curl https://api.telegram.org/bot<旧token>/getMe` 验证：
   返回 `401 Unauthorized` 才算真的失效。

2. **更新本地配置**（`tgshelf_config.toml` 被 .gitignore 保护，永远不会入库；
   建议 `chmod 600`），重启 bot。

3. **清洗历史**（旧提交仍可被按 SHA 访问，不能只删文件了事）
   ```bash
   # 备份（含全部 ref，可回滚）
   git bundle create backup/pre-rewrite.bundle --all
   # 只重写受影响的分支区间，避免牵连干净的分支
   FILTER_BRANCH_SQUELCH_WARNING=1 git filter-branch --force \
     --tree-filter 'sed -i "s/<泄露串>/<占位符>/g" <文件路径>' \
     -- origin/main..rework
   git push --force-with-lease=rework:<旧远端SHA> origin rework
   ```
   验证要用**全新克隆**跑 `git rev-list --all` 逐提交 `git grep`，本地缓存会骗你。

4. **关闭告警**：GitHub → Settings → Security → Secret scanning，
   把对应告警以 `resolution: revoked` 标记 resolved。

写测试时的铁律：**脱敏/加密类单测只准用合成占位值**，且占位值要满足目标正则的形状
（如 Bot Token 需 `\d{8,11}:[A-Za-z0-9_-]{35}`）才能覆盖到分支，但绝不能是真凭据。
