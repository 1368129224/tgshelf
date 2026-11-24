# 配置参考

配置文件默认是 `tgshelf_config.toml`，TOML 格式。首次运行若文件不存在，程序会按内置模板
生成一份带注释的默认配置并退出（`data/` 目录与 session 都在首次启动时创建）。

- 模板文件：[`tgshelf_config.toml.example`](../tgshelf_config.toml.example)
- 加载优先级（低 → 高）：**内置默认值 → TOML 文件 → 环境变量**
- 配置文件路径的确定顺序：命令行 `-config` → 环境变量 `TGSHELF_CONFIG` → `./tgshelf_config.toml`
- 路径解析：`telegram.session` 的**相对路径相对配置文件所在目录**展开；`download_path`
  的相对路径相对**进程工作目录**展开（容器内工作目录是 `/`，因此 `./downloads` 就是 `/downloads`）
- 用 Docker 时，容器以 uid `65532` 运行：配置文件必须对它可读（`chmod 644 tgshelf_config.toml`，
  或 `sudo chown 65532:65532 tgshelf_config.toml` 并保持 600）。

> 兼容性提醒：本加载器**不拒绝未知键**。配置里多余的节（例如 `[upload]`）会被静默忽略，
> 不会报错——请按本文逐项核对，不要以为"没报错就是生效了"。

## 顶层

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `debug` | bool | `false` | 调试模式：输出更详细的日志。日志固定写入配置文件同级的 `data/tgshelf.log`（1 MB × 3 份轮转），并自动脱敏 `bot_token`、`api_hash`、`proxy_url` |
| `bot_token` | string | `""` | **必填**。@BotFather 给的 token。也可用环境变量 `TGSHELF_BOT_TOKEN` |
| `download_path` | string | `"./downloads"` | **必填**。下载根目录，不能为空。标签子目录与 `.parts/` 都在它下面 |
| `tags` | string 数组 | `["video", "music", "doc"]` | 标签 = 下载子目录。不能为空、不能重复、每项不得含 `/`、`\` 或 `..`（路径穿越防护） |
| `proxy_url` | string | `""` | HTTP/SOCKS 代理，例如 `http://127.0.0.1:7890`、`socks5://user:pass@host:1080`。**同时作用于** Bot API 的 HTTP 客户端与 MTProto 连接；留空表示直连 |

## `[telegram]` —— 账号与接入身份

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `identity` | string | `"tdesktop"` | 接入身份：`tdesktop` 或 `custom`。留空等价于 `tdesktop`；其它取值在启动校验时报错（不静默兜底） |
| `api_id` | int | `0` | 仅在 `identity = "custom"` 时使用，必须与 `api_hash` 成对设置。也可用环境变量 `TG_API_ID` |
| `api_hash` | string | `""` | 同上，环境变量 `TG_API_HASH` |
| `session` | string | `"data/session.json"` | MTProto 会话文件。相对路径相对配置文件目录展开；文件权限固定 `0600`；旁边有 `<session>.lock` 做进程互斥 |
| `device_model` | string | `""` | 设备指纹。**留空 = 按身份取默认**：`tdesktop` → `Desktop`（配合 `Windows 10` / `4.2.4 x64` / `tdesktop` 语言包，对齐桌面端会话特征）；`custom` → `tgshelf` |
| `system_version` | string | `""` | 同上，`custom` 身份留空时用 `<GOOS>/<GOARCH>`（如 `linux/amd64`） |
| `app_version` | string | `""` | 同上，留空时用 `1.0` |
| `lang_pack` | string | `""` | 语言包名，通常留空。`tdesktop` 身份默认 `tdesktop` |

**两种身份怎么选**

- `tdesktop`（默认）：使用 Telegram Desktop 的官方应用凭据（`api_id = 2040`）接入，
  **不需要自己申请 api_id/api_hash**，账号在服务端的会话列表里会归到 Desktop 应用族。
  代价是使用了官方客户端的凭据，存在 ToS 风险。
- `custom`：走 [my.telegram.org](https://my.telegram.org) 自己申请 `api_id`/`api_hash`，
  设备字段按配置发送（留空则用中性默认值，不伪装任何官方客户端）。

> 三个设备字段只要显式填了**任意一个**，就整体按你的配置发送（缺的字段用中性默认值），
> 不再套用 TD 指纹。

## `[bot]` —— Bot 行为与权限

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `max_concurrency` | int | `3` | 同时运行的下载任务数上限，取值 `1`–`16`（超出直接校验失败） |
| `allowed_user_ids` | int64 数组 | `[]` | 允许使用 bot 的用户 ID。**留空 = 不限制**（启动日志会 WARN，不建议公开部署） |
| `admin_user_ids` | int64 数组 | `[]` | 管理员 ID：可执行 `/login`、`/logout`、`/sync`；管理员也能看到并取消所有人的任务。留空则没有人能登录账号 |

## `[download]` —— 下载参数

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `threads` | int | `4` | 单文件并发分片数**上限**，取值 `1`–`32`。实际值按文件大小自适应：`<1 MiB → 1`、`<5 MiB → 2`、`<20 MiB → 4`、`<50 MiB → 8`、`≥50 MiB → threads` |
| `file_concurrency` | int | `2` | 一个任务内同时下载的文件数，`≥1`。相册/多链接任务会用到 |
| `skip_same` | bool | `true` | 目标文件已存在且大小相同则跳过（不发任何 `getFile` 请求） |
| `takeout` | bool | `false` | 通过导出（takeout）会话下载。默认关闭；服务端要求导出时会报「该操作必须在导出会话中执行」 |
| `template` | string | `'{{ .DialogID }}_{{ .MessageID }}_{{ filenamify .FileName }}'` | 文件名模板（Go `text/template`）。渲染结果会再做安全清洗；标签子目录是自动加的，**模板里不要写目录** |
| `keep_parts` | bool | `false` | 成功后是否保留 `.state` 位图文件。默认成功后删除、**失败一定保留**（失败保留才能续传与排查） |
| `cache_dialogs_hours` | int | `6` | 会话（dialog）缓存有效期，单位小时。`0` 或负数 = 使用内置默认 6 小时。私有频道的 `access_hash` 就来自这份缓存 |
| `rewrite_ext` | bool | `false` | **预留字段，当前版本不生效**（按文件头纠正扩展名尚未实现） |
| `desc` | bool | `false` | **预留字段，当前版本不生效** |
| `include` | string 数组 | `[]` | **预留字段，当前版本不生效**（扩展名白名单） |
| `exclude` | string 数组 | `[]` | **预留字段，当前版本不生效**（扩展名黑名单） |

### 并发怎么叠加

```
任务并发      max_concurrency          同时进行的下载任务数（bot 层）
  └ 文件并发  file_concurrency         每个任务内并行的文件数
      └ 分片  threads（按大小自适应）  每个文件并行的 ≤1 MiB 分片请求数
```

带宽/限流压力 ≈ 三者相乘。多 DC 连接池的大小取 `threads × file_concurrency`，
并夹在 `8`–`32` 之间。遇到 `FLOOD_WAIT` 时优先降 `file_concurrency`，再降 `threads`
（见 [faq.md](faq.md)）。

### 文件名模板

可用字段：

| 字段 | 含义 |
|---|---|
| `{{ .DialogID }}` | 对话 ID（频道是 `-100…` 形式） |
| `{{ .MessageID }}` | 消息 ID |
| `{{ .MessageDate }}` | 消息发送时间（`time.Time`） |
| `{{ .FileName }}` | 原始文件名（**不可信输入**，务必用 `filenamify` 或模板外的自动清洗）：文档/视频/原图优先取上传时的 `DocumentAttributeFilename`；无文件名的音频优先回退为 `Performer - Title.<ext>`，再回退为 `<DocumentID>.<ext>`；Telegram 普通压缩图片因服务端不保存原名，固定为 `<PhotoID>.jpg` |
| `{{ .FileSize }}` | 文件字节数 |
| `{{ .DownloadDate }}` | 下载发起时间（`time.Time`） |
| `{{ .FileCaption }}` | 目前恒为空（未填充） |

可用函数：`lower`、`upper`、`replace s old new`、`filenamify`（清洗成安全文件名）、
`truncate s n`（按字符截断）、`date t layout`、`now`、`size`（字节数转人类可读）。

示例：

```toml
# 默认值（带对话 ID 与消息 ID 前缀）
template = '{{ .DialogID }}_{{ .MessageID }}_{{ filenamify .FileName }}'

# 直接使用原始文件名（无前缀）
template = '{{ filenamify .FileName }}'

# 按消息日期 + 消息 ID + 截断后的原文件名
template = '{{ .MessageDate | date "20060102" }}_{{ .MessageID }}_{{ truncate (filenamify .FileName) 80 }}'
```

**同名文件防覆盖说明**：当磁盘目标路径已存在同名文件时，若开启了 `skip_same = true` 且文件大小完全一致则直接跳过；若大小不一致（或 `skip_same = false`），系统会自动重命名为 `原名 (1).ext`、`原名 (2).ext`……绝不会覆盖已有文件，且中断后重试仍能精准续传对应的 `.parts/` 状态。

> 模板只能决定**文件名**：渲染结果统一过一遍 `Sanitize`，`/ \ : * ? " < > |` 以及控制字符
> 都会被剔除，所以模板里写不出子目录。要分目录就用标签（`tags`，下载时点选）。

未知字段/未知函数会在渲染时报错，任务以「渲染文件名模板: …」失败——不会写出奇怪的文件名。
渲染结果为空也会报错。最终路径还要通过 `SafeJoin` 校验，绝对路径、`..`、NUL 等一律拒绝。

## 环境变量

| 变量 | 作用 |
|---|---|
| `TGSHELF_CONFIG` | 配置文件路径（等价于 `-config`，命令行优先） |
| `TGSHELF_BOT_TOKEN` | 覆盖 `bot_token` |
| `TG_API_ID` / `TG_API_HASH` | 覆盖 `[telegram] api_id` / `api_hash`。**必须成对出现**，只给一个会直接报错 |

代理没有独立环境变量，统一用 `proxy_url`。

## 校验规则（启动时一次性汇总报错）

- `bot_token` 非空、`download_path` 非空
- `tags` 至少一个、无重复、不含 `/` `\` `..`
- `identity` 只能是 `tdesktop` / `custom`
- `api_id` 与 `api_hash` 必须成对（`identity = "tdesktop"` 时二者都留空即可）
- `bot.max_concurrency` ∈ [1, 16]
- `download.threads` ∈ [1, 32]
- `download.file_concurrency` ≥ 1
- `download.cache_dialogs_hours` ≥ 0

校验失败时程序以非 0 退出，并一次性列出所有问题，例如：

```
Error: load config: config validation failed (2 errors):
  - bot_token: must not be empty
  - download.threads: must be between 1 and 32, got 64
```

## 完整示例

```toml
debug = false
bot_token = "123456789:AAExampleTokenFromBotFather"
download_path = "./downloads"
tags = ["video", "music", "doc"]
proxy_url = ""

[telegram]
identity = "tdesktop"
api_id = 0
api_hash = ""
session = "data/session.json"
device_model = ""
system_version = ""
app_version = ""
lang_pack = ""

[bot]
max_concurrency = 3
allowed_user_ids = [123456789]
admin_user_ids = [123456789]

[download]
threads = 4
file_concurrency = 2
skip_same = true
takeout = false
template = '{{ .DialogID }}_{{ .MessageID }}_{{ filenamify .FileName }}'
keep_parts = false
cache_dialogs_hours = 6
```

改了配置要**重启进程**才生效（配置是只读输入，运行期不回写文件）。
