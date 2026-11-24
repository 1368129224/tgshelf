# AGENTS.md

Guidance for AI coding agents working in `tgshelf`.

## Overview

`tgshelf` is a single-binary, self-hosted Telegram media archiver written in Go (`go 1.26+`). It combines a Telegram Bot UI (`go-telegram/bot`) with a built-in MTProto client (`gotd/td` + `iyear/tdl/core/dcpool`) to resolve `t.me` links or channel keyword searches and download media into per-tag folders with bitmap-based resume state.

## Build, Test & Lint Commands

Always use the `Makefile` targets (they isolate `GOPATH`, `GOMODCACHE`, and `GOCACHE` inside `./.gohome/`):

```bash
make fmt    # go fmt ./...
make build  # go build -o tgshelf ./cmd/tgshelf
make vet    # go vet ./...
make test   # go test -race ./...
make lint   # golangci-lint run ./... (v2 config in .golangci.yml)
```

**Pre-commit verification gate**: every code change MUST pass `make fmt && make build && make vet && make test && make lint` with zero warnings/errors, and leave no build binary (`./tgshelf`) staged.

## Architecture & Package Boundaries

```text
cmd/tgshelf/          CLI entry point, subcommands (-login, -resolve, -download, -sessions, -health), and dependency wiring
internal/
  bot/                Telegram Bot handlers, inline keyboards, task queue manager, /login wizard, /search confirmation flow
    progress/         Throttled message editor (<= 1 edit / 3s) and EWMA speed/ETA formatting
  tg/                 MTProto client wrapper, multi-DC pool, chunked downloader, FILE_REFERENCE refresher, peer resolver, searcher, error translation
  state/              Persistent task/entity store (data/state.json) and per-file chunk bitmap resume session (.parts/*.part + *.state)
  config/             TOML loading, env overrides (TGSHELF_CONFIG, TGSHELF_BOT_TOKEN, TG_API_ID/TG_API_HASH), defaults, validation
  fs/                 Atomic file writes + parent-dir fsync, flock single-instance/session locks, statfs free-space checks
  naming/             Filename template rendering, sanitization, collision deduplication, SafeJoin path-traversal & symlink guards
  logging/            Zap logger with rotating file writer and core-level secret/PII redaction
```

Key boundary rules:
- `internal/bot` MUST NOT import `gotd/td` directly. Wire MTProto operations in `cmd/tgshelf` via callback types (`DownloadFunc`, `SearchMediaFunc`, `SyncFunc`, `AccountFunc`, `LoginAPIGetter`).
- Keep `tgshelf_config.toml.example` synchronized with `defaultConfigTemplate` in `internal/config/load.go`.

## Security, Concurrency & Persistence Invariants

- **Secret & PII redaction**: never log `bot_token`, `api_hash`, `proxy_url` credentials, phone numbers, login codes, 2FA passwords, or raw `FileReference` bytes.
- **Login wizard**: user messages carrying phone numbers, login codes, and 2FA passwords are deleted immediately in chat; never `strings.TrimSpace` 2FA passwords (leading/trailing spaces are significant).
- **Authorization & ownership**:
  - `/login`, `/logout`, `/sync`, and `/show_config` (`/config`) are **admin-only** (`[bot] admin_user_ids`).
  - Non-admin allowed users may only cancel or retry tasks belonging to their own `ChatID`.
- **Filesystem safety**:
  - All user/template-derived paths under `downloads/` must go through `naming.Sanitize` and `naming.SafeJoin` (which also rejects symlinks escaping the root).
  - State, session, and generated config files use `0600` permissions and atomic write-rename (`fs.AtomicWriteFile`); `.part` files must be `Sync()`ed before persisting `.state` bitmaps.
- **Linter compliance (`.golangci.yml`, golangci-lint v2)**:
  - Enabled linters: `errcheck`, `errorlint`, `gosec`, `govet`, `ineffassign`, `staticcheck`, `unused`.
  - Wrap errors with `%w` (including multi-error `fmt.Errorf("%w ... %w", ...)`), compare errors with `errors.Is` / `errors.As`, and add narrow `// #nosec` comments with justification only when opening validated local paths.

## Code & Commit Style

- Write comments, log messages, and user-facing bot/CLI messages in clear English.
- Keep bilingual docs (`README.md` and `README_zh.md`) in sync when changing user-facing commands, flags, or configuration keys.
- Use Conventional Commits: `feat(scope): ...`, `fix(scope): ...`, `perf(scope): ...`, `chore(scope): ...`, `docs: ...`.
