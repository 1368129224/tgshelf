# 登录指引

本 bot 用一个**你自己的 Telegram 账号**（而不是 bot 账号）去读消息和下载文件，所以需要
先把这个账号的 MTProto 会话登录进来。登录只需要做一次，之后会话保存在
`data/session.json`，重启、升级镜像都不用再登。

两种登录入口，效果完全一样：

- **聊天内 `/login`**（推荐，Docker 部署唯一方便的方式）
- **命令行 `./tgshelf -login`**（本机运行、或容器内 `docker compose run --rm tgshelf -login`）

---

## 1. 先决定接入身份：`tdesktop` 还是 `custom`

`[telegram] identity` 决定用哪套 `api_id`/`api_hash` 和设备指纹接入 Telegram。

### `identity = "tdesktop"`（默认）

- 使用 Telegram Desktop 官方客户端的应用凭据（`api_id = 2040`）与 TD 设备指纹。
- **不需要申请 api_id/api_hash**，开箱即用。
- 账号在「设置 → 设备」里会显示成 Telegram Desktop 一类的桌面会话。
- 代价：官方客户端的凭据用于第三方程序属于灰色地带，存在 ToS 风险；介意就切 `custom`。

### `identity = "custom"`

用你自己在 [my.telegram.org](https://my.telegram.org) 申请到的 `api_id`/`api_hash`，
设备字段按配置发送（留空则用中性默认值，不伪装官方客户端）。

**申请步骤：**

1. 浏览器打开 <https://my.telegram.org>，用**要登录的那个手机号**登录。
2. Telegram 会往你的 App 里推一条登录确认（不是短信），在 App 里点确认。
3. 进入 **API development tools**。
4. 新建应用，随便填：
   - `App title`：例如 `tgshelf`
   - `Short name`：例如 `tgshelf`（5–32 个字母数字）
   - `URL`、`Platform` 可留空或填 `Other`
5. 提交后页面给出 **`App api_id`**（数字）和 **`App api_hash`**（32 位十六进制）。

填进配置：

```toml
[telegram]
identity = "custom"
api_id   = 12345678
api_hash = "0123456789abcdef0123456789abcdef"
```

也可以用环境变量（优先级高于文件，且必须成对）：

```bash
export TG_API_ID=12345678
export TG_API_HASH=0123456789abcdef0123456789abcdef
```

> `api_hash` 等同于密码：不要提交进 git、不要发到群里。泄露了就到 my.telegram.org
> 删除应用重新生成。**不要**使用网上流传的公开 api_id/api_hash——违反 ToS，而且可能
> 牵连你的账号。

---

## 2. 聊天内 `/login`（推荐）

前提：你的 Telegram 用户 ID 在 `[bot] admin_user_ids` 里。别人的 `/login` 会被拒绝：
`Not allowed: /login is admin only.`

流程：

```
你: /login
bot: Login wizard started. Login code and 2FA password messages are deleted automatically, so do not use this command in a public group.
     Send /cancel at any time to leave the wizard.
     Send your phone number in international format, e.g. +8613800138000:

你: +8613800138000
bot: Login code sent. Enter the code you received:

你: 12345
bot: Logged in.              ← 账号没开两步验证时到此结束
     或者
bot: This account has two-step verification enabled. Enter your 2FA password:
```

要点：

- **手机号必须是国际格式**：`+` 开头，允许空格、`-`、`(`、`)`、`.` 等分隔符会被去掉。
- **验证码与 2FA 密码消息在进入业务逻辑前就被删除**（先删再处理），失败也一样删。
  但 Telegram 的推送通知仍可能显示内容，所以别在公共群/公共设备上做这件事。
- 向导状态保存在内存里，**10 分钟**不推进就过期，需要重新 `/login`。
- 发送 `/cancel` 会退出向导（不会删除已有 session）。
- 验证码填错可以直接再输入一次（状态还停在「等待验证码」这一步）；验证码过期或
  想重新发码，重新发 `/login` 即可——注意发码本身受 `FLOOD_WAIT` 限制。
- 登录成功只回一句 `Logged in.`，账号信息可以用 `/show_config` 查看
  （`signed in (@username, user id …, DC …)`）。

常见报错（都是中文直说原因）：

| 提示 | 原因与处理 |
|---|---|
| `rate limited: wait N seconds and retry` | 发码/验码太频繁，等够秒数再来；`FLOOD_WAIT` 是账号级别的，换网络没用 |
| `Telegram returned error PHONE_CODE_INVALID (HTTP 400)` | 重新输入即可，还在同一步 |
| `Telegram returned error PHONE_CODE_EXPIRED (HTTP 400)` | 重新 `/login` 发新码 |
| `Telegram returned error PASSWORD_HASH_INVALID (HTTP 400)` | 重新输入 2FA 密码；忘记密码只能走 Telegram 官方找回流程 |
| `login session is no longer valid: log in again (CLI: tgshelf -login)` | 本地 session 被服务端作废（例如在手机上「终止所有会话」），重新登录 |

---

## 3. 命令行登录 `tgshelf -login`

适合本机部署，或容器里没有聊天入口时排障：

```bash
./tgshelf -config tgshelf_config.toml -login
```

交互顺序：手机号 → 验证码（明文回显，方便核对）→ 2FA 密码（回显为 `*`）。
成功后打印 `Logged in: id=… username=… dc=…`，并写出 session 文件。

已有 session 时的行为：

- 默认把旧文件**改名备份**为 `session.json.bak` 再重新登录。原因：残留但已被服务端
  丢弃的 auth key 会让 `auth.sendCode` 返回 `AUTH_RESTART`，重试没有意义。
- 加 `-keep-session` 则在原文件上直接重新登录（排查或换号时想保留旧文件用）。

容器里跑（不启动 bot，只登录一次，session 直接落到挂载的 `./data`）：

```bash
docker compose run --rm tgshelf -login
```

> 需要可交互的 stdin。`docker compose run` 默认就是交互模式；如果你把 stdin 重定向了，
> 命令会读不到手机号而失败。

---

## 4. session 文件：位置、权限、备份

- 默认路径 `data/session.json`（`[telegram] session` 可改；相对路径相对配置文件目录）。
- 权限固定 `0600`，`data/` 是 `0700`；文件里是 auth key，**等同于账号密码**。
- 旁边有 `data/session.json.lock`（进程级 flock），防止两个实例同时用同一 session
  触发 `AUTH_KEY_DUPLICATED`。
- 备份：直接复制 `data/session.json` 到安全的地方即可；恢复到新机器时放回同样路径，
  进程启动就能用（同样的 `identity`/凭据下）。
- 容器里 `data/` 归 uid `65532` 所有，宿主机普通用户读不了，备份要 `sudo`：

```bash
sudo cp data/session.json /path/to/backup/session-$(date +%F).json
```

session 损坏（JSON 坏了、AuthKeyID 与 AuthKey 不匹配）时程序会明确报
`session is corrupted, please log in again (CLI: tgshelf -login)`，不会静默重建——重新登录即可。

---

## 5. 退出登录 `/logout`

```
/logout  →  已删除 session 文件。
            注意：本次进程仍持有已建立的连接，需要重启后才会真正变为未登录；
            另外 Telegram 的「活跃会话」还需在手机端手动终止，本机器人无法代为撤销。
```

完整退出建议三步：

1. 聊天里发 `/logout`（删掉本地 session 文件）；
2. `docker compose restart`（或 `docker compose down && docker compose up -d`），
   让进程丢掉已建立的连接；
3. 手机 Telegram → 设置 → 设备（活跃会话）→ 终止对应会话。

---

## 6. 与「PC 端被顶掉」有关的事实

用本项目登录后，PC 端 Telegram Desktop 可能被服务端退出登录。仓库里的排查记录
（`internal/tg/identity.go`）显示：

- 旧会话（自有 `api_id`）在服务端被归类为 `platform="Web"`；
- 改用 Telegram Desktop 的 `api_id = 2040` 接入时，会话归类为 Desktop 应用族，PC 端可保持登录；
- 本项目代码里没有任何登出/重置授权调用。

因此默认 `identity = "tdesktop"` 走的就是这条接入路径。想确认当前账号上有哪些会话，
用：

```bash
./tgshelf -config tgshelf_config.toml -sessions
```

输出会列出每个会话的 app 名、设备、平台、`api_id` 和创建时间；没有 Desktop 会话时会明确提示
「列表里没有 Telegram Desktop 会话」。

---

## 7. 故障速查

| 现象 | 处理 |
|---|---|
| 启动即退出：`not logged in: session file … not found, run tgshelf -login first` | 这是旧行为；当前版本允许未登录启动。若仍看到，检查配置路径是否被 `-config`/`TGSHELF_CONFIG` 改到了别处 |
| 启动即退出：`another instance is already using this session … stop it first` | 同一 `data/` 被两个进程用（容器重复启动最常见）。`docker ps` 查一下，或删掉残留进程 |
| `/login` 没反应 | 你不是 `admin_user_ids` 里的人；看 bot 的回复是否「权限不足」 |
| 收不到验证码 | 先在手机上确认该账号能正常收到 Telegram 登录码；`FLOOD_WAIT` 时只能等 |
| 登录后下载报 `未登录` | 检查 `data/session.json` 是否真的是新登录写出的那个（`/show_config` 看账号；`-sessions` 看会话） |
| 换机器后要重新登录 | 复制 `data/session.json` 过去即可；`identity` 要一致 |
