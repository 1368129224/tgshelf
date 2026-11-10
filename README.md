<h1 align="center">tgshelf</h1>

<p align="center">
A self-hosted Telegram media archiver: send a <code>t.me</code> link, pick a tag, and the media lands in that folder.<br>
One static Go binary with its own MTProto client — nothing else to install.
</p>

<p align="center">
English | <a href="README_zh.md">简体中文</a>
</p>

---

## Highlights

- **Fast** — 1 MiB chunks, `threads` in parallel per file, over a multi-DC connection pool.
- **Resumable** — a chunk bitmap in `downloads/<tag>/.parts/` survives crashes, restarts and `/cancel`.
- **Albums, private channels, discussion groups** — expanded and resolved transparently.
- **Keyword search & batch download** — `/search @channel keyword` finds photo/video messages
  server-side (no full-history scan), previews the hit count and total size, then downloads them all.
- **Readable failures** — `FLOOD_WAIT`, `FILE_REFERENCE_EXPIRED`, `ENOSPC` … become Chinese explanations
  instead of raw codes.
- **Live progress** — one edited message per task (bar, percentage, speed, ETA), at most one edit per 3 s.
- **No babysitting** — tasks survive restarts (`/status`, `/cancel`, retry), single-instance lock,
  path-traversal-proof filenames, secrets redacted from logs.

## Quick start (Docker)

1. **Create the bot.** Run `/newbot` on [@BotFather](https://t.me/BotFather) and keep the token; get your
   numeric Telegram ID (e.g. from [@userinfobot](https://t.me/userinfobot)) for `admin_user_ids` —
   `/login` is admin-only.

2. **Configure.**

   ```bash
   cp tgshelf_config.toml.example tgshelf_config.toml
   ```

   ```toml
   bot_token = "123456789:AA...your-token..."   # from @BotFather
   admin_user_ids = [123456789]                 # your numeric Telegram ID
   allowed_user_ids = [123456789]               # optional whitelist; empty = everyone
   ```

   `identity = "tdesktop"` (the default) uses Telegram Desktop's built-in credentials, so **no
   `api_id`/`api_hash` is needed** — see [docs/login.md](docs/login.md) for the alternative.

3. **Start.** The container runs as uid/gid `65532` (`nonroot`), so the mounts must be writable by it:

   ```bash
   cp docker-compose.yml.example docker-compose.yml
   mkdir -p data downloads
   sudo chown -R 65532:65532 data downloads
   chmod 644 tgshelf_config.toml     # the container user must be able to read it
   docker compose up -d
   docker compose ps                 # wait for "Up (healthy)"
   ```

   The first start has no session yet (`not logged in: session file ... not found, run tgshelf -login first`) — that is expected, and the
   health probe treats it as healthy so the in-chat `/login` can happen.

4. **Log in.** Send `/login`, then your phone number in international format, the login code, and your
   2FA password if enabled. These three messages are deleted automatically. The CLI (`./tgshelf -login`)
   works too — see [docs/login.md](docs/login.md).

5. **Download.** Send any `t.me` link, e.g. `https://t.me/telegram/193`, and pick a tag on the inline
   keyboard; the file lands in `./downloads/<tag>/`.

```bash
docker compose down          # stop
docker compose up -d --build # update
```

## Commands

| Command | Who | What it does |
|---|---|---|
| `/start`, `/help` | allowed users | usage summary |
| `/search <channel> <keyword>` | allowed users | search a channel's photo/video messages by keyword (server-side `messages.search`), preview the hit count and total size, then batch download via a tag |
| `/status` | allowed users | queued and running tasks (admins see every task) |
| `/cancel <id>` / `/cancel all` | allowed users | cancel a task; also exits an in-progress login wizard |
| `/show_config` (alias `/config`) | allowed users | current configuration with secrets masked |
| `/sync` | **admin** | refresh the dialog/`access_hash` cache (needed for private channels) |
| `/login` | **admin** | phone → code → 2FA login wizard |
| `/logout` | **admin** | delete the session file (restart required; revoke the device in Telegram separately) |

Admins are the IDs listed in `[bot] admin_user_ids`.

## Run without Docker

Requirements: Go 1.26+ (the `Makefile` sets `GOTOOLCHAIN=auto`, so an older toolchain downloads 1.26
automatically).

```bash
make build                                         # produces ./tgshelf
cp tgshelf_config.toml.example tgshelf_config.toml # then edit bot_token / admin_user_ids
./tgshelf -config tgshelf_config.toml              # start the bot
```

CLI subcommands (all read the same config file):

```bash
./tgshelf -config tgshelf_config.toml -login                       # interactive login in the terminal
./tgshelf -config tgshelf_config.toml -resolve https://t.me/telegram/193
./tgshelf -config tgshelf_config.toml -download https://t.me/telegram/193 -tag video -threads 4
./tgshelf -config tgshelf_config.toml -sessions                    # list active Telegram sessions
./tgshelf -config tgshelf_config.toml -health                      # health probe (also used by Docker)
```

Run `./tgshelf -h` for the full flag list (`-to`, `-tag`, `-threads`, `-file-concurrency`,
`-keep-parts`, `-chunk-bytes`, `-keep-session`).

## Configuration

`tgshelf_config.toml`, with environment overrides:

| Variable | Overrides |
|---|---|
| `TGSHELF_CONFIG` | config file path (default `tgshelf_config.toml`) |
| `TGSHELF_BOT_TOKEN` | `bot_token` |
| `TG_API_ID` / `TG_API_HASH` | `[telegram] api_id` / `api_hash` (must be set together; only with `identity = "custom"`) |

Every key, its default and its constraints: [docs/configuration.md](docs/configuration.md).

## Files on disk

```
tgshelf_config.toml                  configuration
data/session.json                    MTProto session (0600 — treat it like a password)
data/session.json.lock               per-session flock
data/session.lock                    single-instance lock
data/state.json                      tasks + cached dialog entities
data/tgshelf.log                     rotating log (1 MB × 3)
downloads/<tag>/<file>               finished downloads
downloads/<tag>/.parts/<file>.part   sparse partial file (resumable)
downloads/<tag>/.parts/<file>.state  chunk bitmap + metadata
```

Never delete `.parts/` while a download is unfinished: that is the resume state. `keep_parts = false`
(default) removes `.state` after success but always keeps it on failure.

## Documentation

| Document | Contents |
|---|---|
| [docs/configuration.md](docs/configuration.md) | every config key, defaults and constraints |
| [docs/login.md](docs/login.md) | `api_id`/`api_hash`, `/login` wizard, 2FA, session handling |
| [docs/faq.md](docs/faq.md) | `FLOOD_WAIT`, private channels, slow downloads, full disk |
| [docs/architecture.md](docs/architecture.md) | layering, data flow, concurrency and recovery |
| [docs/performance.md](docs/performance.md) | measured throughput and tuning |
| [CHANGELOG.md](CHANGELOG.md) | release history |

## License

AGPL-3.0. See [LICENSE](LICENSE).

Built on [gotd/td](https://github.com/gotd/td) (MIT) and
[go-telegram/bot](https://github.com/go-telegram/bot) (MIT). The multi-DC connection pool and the
retry/recovery middlewares are reused from [iyear/tdl](https://github.com/iyear/tdl) `core` (AGPL-3.0).
