# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2.0.0] — 2026-11-10

First stable release: one static Go binary that talks MTProto directly and files
Telegram media into per-tag folders.

### Added

- **Download kernel** — native MTProto downloads over shared upstream protocol
  middlewares: multi-chunk concurrency, per-file thread count and cross-file
  concurrency limits, dynamic chunk scheduling, and checksum verification.
- **Resumable downloads** — bitmap-based progress that survives restarts, a
  `.parts` temp directory per tag, and atomic final rename so a half-written file
  never appears under its final name.
- **Connection pool** — one connection per data centre plus several TCP
  connections inside a single DC, which lifts throughput well past the
  single-connection server-side cap (see [docs/performance.md](docs/performance.md)).
- **Reference refresh** — expired file references (`FILE_REFERENCE_EXPIRED`) are
  re-resolved in place, so long downloads continue instead of failing.
- **Single-instance guard** — an OS file lock (`flock`) prevents two processes
  from sharing one Telegram session directory.
- **Persistent state** — tasks, the download queue and resolved entities are kept
  on disk; unfinished work is picked up again after a restart and stale tasks are
  closed on startup.
- **Bot UI** — link resolution for single messages and albums, tag keyboards
  using short callback IDs (well inside the 64-byte limit), throttled progress
  cards with smoothed speed and a persistent cancel button, and a final summary
  with elapsed time, succeeded/total count and average speed.
- **Task manager** — `PENDING`/`RUNNING` queue with real cancellation of queued
  items, a `max_concurrency` gate, and per-link progress routing.
- **Commands** — `/start`, `/help`, `/status`, `/cancel [id|all]`,
  `/show_config` (secrets masked), `/sync`, `/login`, `/logout`.
- **Login wizard** — in-chat phone number, login code and 2FA password entry,
  plus an equivalent `-login` CLI flow.
- **Download filters** — extension allow/block lists, size bounds, media kinds,
  `skip_same`, extension rewriting from file headers, optional description files,
  takeout sessions and configurable filename templates.
- **Safe naming** — per-tag folders with sanitised, length-limited names and path
  traversal protection.
- **Observability** — structured logs with `task_id`/`link`/`tag`, size-based
  rotation, sensitive-value redaction at the zap core level, and panic recovery
  around both task goroutines and handlers.
- **Configuration** — TOML with validation and defaults, `TGSHELF_CONFIG` and
  `TGSHELF_BOT_TOKEN` overrides, and a generated config template on first run.
- **Deployment** — multi-stage distroless image running as `nonroot` with a
  built-in `-health` probe, amd64/arm64 builds, a Compose example, and a
  zero-dependency static binary for non-Docker use.
- **Docs** — [architecture](docs/architecture.md), [configuration](docs/configuration.md),
  [login](docs/login.md), [performance](docs/performance.md) and [FAQ](docs/faq.md).

### Security

- Bot token, API hash, proxy credentials, phone numbers, login codes and 2FA
  passwords never reach the logs; `/show_config` shows masked values only.
- Optional allowlist (`allowed_user_ids`) and a separate admin list
  (`admin_user_ids`) for account-level management commands.

[2.0.0]: https://github.com/1368129224/tgshelf/releases/tag/v2.0.0
