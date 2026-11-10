# deploy/

Image build and deployment notes. For the full walkthrough see the root [README.md](../README.md).

## Dockerfile highlights
- **Multi-stage build**: `golang:1.26` build stage → a minimal `gcr.io/distroless/static-debian12:nonroot` runtime.
- **Security and permissions**: the container runs as `USER nonroot:nonroot` (uid:gid 65532:65532) without root, and the distroless image has no shell.
- **Health check**: a built-in `HEALTHCHECK` runs `/tgshelf -health` periodically. Not being logged in (no session) still counts as healthy; only an unreadable config, an unreadable session or insufficient disk space fails it.
- **Size**: the binary is built static with symbols stripped (about 45 MB, mostly the generated code in `gotd/td`); the image is about 49 MB (measured with `docker images`).
- **Multi-arch**: cross-compiled from TARGETOS/TARGETARCH for both `linux/amd64` and `linux/arm64`.

## Directory and volume conventions
- `data/` and `downloads/` live under the container working directory `/`; mount them from the host with volumes.
- The `.parts/` temp chunk directory stays inside the download directory (`downloads/<tag>/.parts/`); do not delete it before a download finishes.
- The container runs as uid 65532, so the host directories must be granted first:
  ```bash
  mkdir -p data downloads
  sudo chown -R 65532:65532 data downloads
  ```

## Starting the container

### Option A: Docker Compose (recommended)
Use `docker-compose.yml.example` from the repository root:
```bash
cp tgshelf_config.toml.example tgshelf_config.toml   # set bot_token and admin_user_ids
cp docker-compose.yml.example docker-compose.yml
mkdir -p data downloads && sudo chown -R 65532:65532 data downloads
chmod 644 tgshelf_config.toml                     # the nonroot user inside must be able to read it
docker compose up -d
docker compose logs -f
```

Then send `/login` to the bot in Telegram to finish logging in (see [docs/login.md](../docs/login.md)).

### Option B: plain Docker
```bash
docker build -f deploy/Dockerfile -t tgshelf:local .
docker run -d --name tgshelf \
  --restart unless-stopped \
  --stop-timeout 30 \
  -v "$PWD/data:/data" \
  -v "$PWD/downloads:/downloads" \
  -v "$PWD/tgshelf_config.toml:/tgshelf_config.toml:ro" \
  tgshelf:local
```

To reach a proxy running on the host, add `--add-host host.docker.internal:host-gateway` and set `proxy_url = "socks5://host.docker.internal:1080"` in the config.

### Logging in from the container (optional)
```bash
docker compose run --rm tgshelf -login
```
Logging in from the CLI is equivalent to `/login` in chat; the session is written straight to the mounted `data/`.
