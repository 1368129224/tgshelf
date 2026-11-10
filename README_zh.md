<h1 align="center">tgshelf</h1>

<p align="center">
一个自托管的 Telegram 媒体归档机器人：发 <code>t.me</code> 链接、选标签，媒体就落到对应子目录。<br>
单个静态 Go 二进制，自带 MTProto 客户端——不用再装别的东西。
</p>

<p align="center">
<a href="README.md">English</a> | 简体中文
</p>

---

## 特性

- **快**：1 MiB 分片，每文件并发 `threads` 片，跨多 DC 连接池取数据。
- **可续传**：分片位图存在 `downloads/<tag>/.parts/`，崩溃、重启、`/cancel` 之后只补缺失分片。
- **相册 / 私有频道 / 讨论组**：媒体组自动展开，`t.me/c/...` 与 `?comment=<id>` 链接透明解析。
- **报错可读**：`FLOOD_WAIT`、`FILE_REFERENCE_EXPIRED`、`ENOSPC` 等一律翻译成中文原因，
  而不是甩一串错误码。
- **实时进度**：每个任务一条持续编辑的消息（进度条 + 百分比 + 速度 + 剩余时间），最多每 3 秒编辑一次。
- **不用盯着**：任务可跨重启恢复（`/status`、`/cancel`、失败重试）、单实例锁、文件名防路径穿越、
  日志自动脱敏。

## 快速开始（Docker）

1. **创建 bot。** 找 [@BotFather](https://t.me/BotFather) 发 `/newbot`，留住 token；再从
   [@userinfobot](https://t.me/userinfobot) 拿你的**数字** Telegram ID 填进 `admin_user_ids`
   （`/login` 只允许管理员执行）。

2. **写配置。**

   ```bash
   cp tgshelf_config.toml.example tgshelf_config.toml
   ```

   ```toml
   bot_token = "123456789:AA...你的token..."   # @BotFather 给的
   admin_user_ids = [123456789]                # 你的数字 Telegram ID
   allowed_user_ids = [123456789]              # 可选白名单；留空 = 所有人可用
   ```

   `identity = "tdesktop"`（默认）使用 Telegram Desktop 的内置应用凭据，所以**不需要
   `api_id`/`api_hash`**——想用自己的凭据见 [docs/login.md](docs/login.md)。

3. **启动。** 容器以 uid/gid `65532`（`nonroot`）运行，宿主机目录必须让它可写：

   ```bash
   cp docker-compose.yml.example docker-compose.yml
   mkdir -p data downloads
   sudo chown -R 65532:65532 data downloads
   chmod 644 tgshelf_config.toml     # 容器内的 nonroot 用户要能读到配置文件
   docker compose up -d
   docker compose ps                 # 等到 "Up (healthy)"
   ```

   首次启动本来就没有 session（`not logged in: session file ... not found, run tgshelf -login first`），这是预期行为；健康检查把
   「还没登录」算作健康，正是"聊天里 /login"能成立的前提。

4. **登录。** 给 bot 发 `/login`，依次给国际格式手机号、登录验证码、以及（若开了两步验证的）
   2FA 密码；这三条消息会被自动删除。命令行 `./tgshelf -login` 同样可用，细节见
   [docs/login.md](docs/login.md)。

5. **下载。** 发任意 `t.me` 链接（例如 `https://t.me/telegram/193`），在按钮上选标签，文件落到
   `./downloads/<标签>/`。

```bash
docker compose down          # 停止
docker compose up -d --build # 升级（拉取新代码后重建）
```

## 命令

| 命令 | 谁可用 | 作用 |
|---|---|---|
| `/start`、`/help` | 白名单用户 | 用法说明 |
| `/status` | 白名单用户 | 排队中与正在下载的任务（管理员能看到所有任务） |
| `/cancel <ID>` / `/cancel all` | 白名单用户 | 取消任务；登录向导进行中时用来退出向导 |
| `/show_config`（别名 `/config`） | 白名单用户 | 查看当前配置（敏感项已脱敏） |
| `/sync` | **管理员** | 刷新会话 / `access_hash` 缓存（私有频道必须） |
| `/login` | **管理员** | 手机号 → 验证码 → 2FA 登录向导 |
| `/logout` | **管理员** | 删除 session 文件（需重启才真正未登录；设备还要在 Telegram 里手动终止） |

管理员就是 `[bot] admin_user_ids` 里的人。

## 不用 Docker 运行

要求：Go 1.26+（`Makefile` 里设了 `GOTOOLCHAIN=auto`，旧工具链会自动下载 1.26）。

```bash
make build                                         # 生成 ./tgshelf
cp tgshelf_config.toml.example tgshelf_config.toml # 然后改 bot_token / admin_user_ids
./tgshelf -config tgshelf_config.toml              # 启动 bot
```

命令行子功能（都读同一个配置文件）：

```bash
./tgshelf -config tgshelf_config.toml -login                       # 终端里交互式登录
./tgshelf -config tgshelf_config.toml -resolve https://t.me/telegram/193
./tgshelf -config tgshelf_config.toml -download https://t.me/telegram/193 -tag video -threads 4
./tgshelf -config tgshelf_config.toml -sessions                    # 列出账号的活跃会话
./tgshelf -config tgshelf_config.toml -health                      # 健康检查（Docker 也用它）
```

完整参数见 `./tgshelf -h`（`-to`、`-tag`、`-threads`、`-file-concurrency`、`-keep-parts`、
`-chunk-bytes`、`-keep-session`）。

## 配置

配置文件是 `tgshelf_config.toml`，支持环境变量覆盖：

| 环境变量 | 覆盖 |
|---|---|
| `TGSHELF_CONFIG` | 配置文件路径（默认 `tgshelf_config.toml`） |
| `TGSHELF_BOT_TOKEN` | `bot_token` |
| `TG_API_ID` / `TG_API_HASH` | `[telegram] api_id` / `api_hash`（必须成对；仅在 `identity = "custom"` 时生效） |

每个配置项的含义、默认值与约束：[docs/configuration.md](docs/configuration.md)。

## 目录与文件

```
tgshelf_config.toml                  配置文件
data/session.json                    MTProto 会话（权限 0600——等同账号密码）
data/session.json.lock               单个 session 的进程锁
data/session.lock                    单实例锁
data/state.json                      任务状态 + 会话实体缓存
data/tgshelf.log                     滚动日志（1 MB × 3）
downloads/<标签>/<文件>              下载完成的文件
downloads/<标签>/.parts/<文件>.part   稀疏分片文件（可续传）
downloads/<标签>/.parts/<文件>.state  分片位图与元数据
```

下载没完成时不要删 `.parts/`——那正是续传状态。`keep_parts = false`（默认）表示成功后删
`.state`，失败时一定保留。

## 文档

| 文档 | 内容 |
|---|---|
| [docs/configuration.md](docs/configuration.md) | 全部配置项、默认值与约束 |
| [docs/login.md](docs/login.md) | `api_id`/`api_hash` 申请、`/login` 流程、2FA、session 处理 |
| [docs/faq.md](docs/faq.md) | `FLOOD_WAIT`、私有频道、下载慢、磁盘满 |
| [docs/architecture.md](docs/architecture.md) | 分层、数据流、并发与恢复机制 |
| [docs/performance.md](docs/performance.md) | 实测吞吐与调优建议 |
| [CHANGELOG.md](CHANGELOG.md) | 版本变更记录 |

## 许可证

AGPL-3.0，见 [LICENSE](LICENSE)。

协议层是 [gotd/td](https://github.com/gotd/td)（MIT），Bot API 客户端是
[go-telegram/bot](https://github.com/go-telegram/bot)（MIT）。多 DC 连接池与重试 / 恢复中间件
复用了 [iyear/tdl](https://github.com/iyear/tdl) `core`（AGPL-3.0）中的对应实现。
