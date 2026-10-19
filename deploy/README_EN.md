# fbchat-v2 - Docker deployment (fork)

[Tiếng Việt](README.md)

This directory is the fork-only part of the repository: it runs the sample bot in Docker, signs in to Facebook through a web-based browser (noVNC) and runs several accounts in one container. Everything lives in `deploy/` and no upstream file is modified, so syncing with the original repository does not conflict.

## 📂 Layout

```text
deploy/
├── docker-compose.yml          # project "fbchat-v2", volume fbchat-data
├── login/                      # Alpine + Chromium + noVNC, captures cookies then stops
│   ├── Dockerfile
│   └── extract_cookie.py
├── bot/                        # python:3.12-alpine + statically built Go bridge
│   ├── Dockerfile
│   └── Dockerfile.dockerignore # per-Dockerfile ignore file (BuildKit), root untouched
├── multi_main.py               # multi-account runner, uses only src/main.py APIs
├── gateway_link.py             # omnichannel gateway link (when GATEWAY_URL is set)
└── tests/                      # runner tests + contract tests against upstream
```

## 🚀 Quick start

```bash
cd deploy
docker compose up -d --build
```

To run `docker compose` from the repository root without `-f`, create a root `.env` file (upstream already gitignores `.env`):

```bash
echo "COMPOSE_FILE=deploy/docker-compose.yml" >> .env
```

1. `login` opens a browser at <http://localhost:6080/vnc.html?autoconnect=1&resize=scale>. Sign in to Facebook there.
2. Once the `c_user` and `xs` cookies exist, `login` writes `/data/accounts/<ACCOUNT>/config.json` (mode `0600`, `ACCOUNT` defaults to `default`) to the volume and stops. It can take up to about 30 seconds for Chromium to flush cookies to disk; the "Aw, Snap!" screen shown at that point is expected.
3. `bot` starts after `login` succeeds and runs every account in `/data/accounts`.

## 👥 Multiple accounts

```bash
ACCOUNT=acc2 docker compose run --rm --service-ports login            # sign in an extra account acc2
docker compose restart bot                                            # load the new account
FORCE_LOGIN=1 ACCOUNT=acc2 docker compose run --rm --service-ports login  # sign in again after cookies expire
```

- All accounts share one Python process, event loop and `httpx.AsyncClient`. Each account still gets its own Go bridge process because the bridge supports a single client.
- Log lines carry the account name, for example `[acc2] [bot] ...`. Lines printed by the bridge itself are not labelled yet.
- A failing account is retried with backoff (30 seconds, doubling, capped at 10 minutes) and dropped after 5 consecutive failures without stopping the other accounts. The counter resets when the previous run lasted 10 minutes or more.
- Account names may contain only letters, digits, `_`, `.` and `-` (up to 64 characters, starting with a letter or digit). To remove an account, stop the bot, delete `/data/accounts/<name>` in the volume and start the bot again.

## 🩺 Healthcheck and restarts

- `multi_main.py` writes each account's state (`starting`, `running`, `retrying`, `dropped`) to `FBCHAT_HEALTH_FILE` (`/tmp/fbchat-health.json` in the image).
- The container is `healthy` only when every account is `running`; `docker compose ps` shows `unhealthy` while an account is retrying or dropped.
- The bot uses `restart: unless-stopped`. When every account is dropped, the process keeps running (it does not exit) so Docker does not restart it and call Facebook with broken cookies again. Sign in again and run `docker compose restart bot`.

## 📊 Resources

| Service | Image | Limits | Measured |
|---|---|---|---|
| `login` | `alpine:3.22` + Chromium (~880 MB) | 1 CPU, 768 MiB | ~225-325 MiB while signing in, then stops |
| `bot` | `python:3.12-alpine` (~95 MB) | `BOT_CPUS` (default 1), `BOT_MEMORY_LIMIT` (default 512M) | ~25 MiB shared Python + 15-25 MiB per account at idle; two accounts ~60 MiB, CPU < 1% |

Raise `BOT_MEMORY_LIMIT` when adding many accounts or sending large media, because the bridge holds whole media files in memory while sending or downloading. Once signed in you can remove the login image: `docker image rm fbchat-v2-login`.

Environment variables (shell or `.env`): `ACCOUNT`, `FORCE_LOGIN`, `LOGIN_TIMEOUT` (seconds, default 900), `VNC_PASSWORD`, `BOT_ADMINS` (comma-separated IDs), `BOT_PREFIX`, `BOT_CPUS`, `BOT_MEMORY_LIMIT`.

## 🛡️ Security

- noVNC binds to `127.0.0.1` only. The Chromium profile lives on tmpfs and is discarded when the container stops.
- Chromium runs without remote debugging or automation flags; the script reads and decrypts the profile's SQLite `Cookies` file (`--password-store=basic`). Policies disable DevTools and the password manager.
- Chromium runs with `--no-sandbox` because Docker's default seccomp profile blocks its sandbox; the container is only used to open Facebook.
- The bot runs as a non-root user with a read-only root FS, `cap_drop: ALL` and `no-new-privileges`. Cookies live in the `fbchat-data` volume, not in the image or the repository.

## 🔗 Omnichannel mode (gateway)

With `GATEWAY_URL` and `GATEWAY_TOKEN` (at least 32 characters) set, `multi_main.py` forwards every customer message to the omnichannel gateway (`POST /v1/inbound`) instead of running `/ping`-style commands, and serves `POST /send` on `BOT_HTTP_PORT` (default 7002) for agent replies. Set `BOT_COMMANDS=1` to keep the commands. The code lives in `gateway_link.py`; the full stack (Libredesk + gateway + bots) lives in a separate `omnichannel/` directory.

- `thread_id` sent to the gateway: `e2ee:<chatJid>` for encrypted chats, `thread:<threadId>` for regular chats; groups are detected from `threadType`.
- Sender names come from `_get_user_info` and are cached.
- Attachments (image, video, audio, file): the bot downloads them through `downloadE2EEMedia` (encrypted chats) or `downloadMedia` (regular chats) and sends them to the gateway as base64; agent files go out through `sendE2EEImage`/`sendE2EEVideo`/`sendE2EEAudio`/`sendE2EEDocument` or `sendImage`/`sendFile`. Stickers, links and locations become a one-line description. Per-file limit `GATEWAY_MAX_FILE_MB` (default 20).
- A reply the bridge does not confirm (deliveryStatus UNKNOWN) still returns 200 so the gateway does not resend it and the customer does not get duplicates.

### E2EE device state

`AccountBot` stores the E2EE device state in `/data/accounts/<name>/e2ee_device.json` (mode `0600`). Upstream keeps it in memory by default (`e2ee_memory_only=True`): every restart becomes a new E2EE device and incoming messages often fail with `E2EE message could not be decrypted or was unavailable`. Set `FBCHAT_E2EE_PERSIST=0` to restore the upstream behaviour. The file holds the account's encryption keys: do not copy it out of the volume.

## 🔄 Syncing with upstream

The `upstream` remote points to the original repository and `origin` to the fork:

```bash
git fetch upstream
git merge upstream/main          # or: git rebase upstream/main
cd deploy && docker compose build && docker compose up -d
```

Because the fork only adds files under `deploy/`, merges do not conflict. The remaining risk is upstream changing an API that `multi_main.py` relies on; to catch that, run:

```bash
pytest deploy/tests
```

Upstream touch points (checked by `test_upstream_contract_used_by_fork`):

- `src/main.py`: `SimpleBot` (parameters `prefix`, `admins`, `log_message_content`, `debug_errors`, `http_client`; method `_queue_event`), `log`, `DEFAULT_HTTP_TIMEOUT`, `is_valid_datafb`.
- `listeningE2EEEvent`: attributes `device_path`, `startup_timeout`, `wait_until_connected(timeout, require_e2ee=...)`.
- `dataGetHome(storage=...)`, `FileSessionStorage`, `set_private_file_permissions`.
