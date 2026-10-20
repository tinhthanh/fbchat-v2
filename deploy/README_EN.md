# fbchat-v2 - Docker deployment (fork)

[Tiếng Việt](README.md)

This directory is the fork-only part of the repository: it runs the sample bot in Docker, signs in to Facebook through a web-based browser (noVNC) and runs several accounts in one container. Everything lives in `deploy/` and no upstream file is modified, so syncing with the original repository does not conflict.

## 📂 Layout

```text
deploy/
├── docker-compose.yml          # project "fbchat-v2", volume fbchat-data
├── login/                      # Alpine + Chromium + noVNC, captures cookies then stops
│   ├── Dockerfile
│   ├── extract_cookie.py
│   └── control.py              # fb-login-ui service: browser on demand
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
- A failing account is retried with backoff (30 seconds, doubling) without stopping the other accounts. Only a **really expired session** (Facebook reachable but the cookie no longer yields dataFB) stops after 2 attempts; network loss, bridge errors… are retried forever, at most every 2 minutes, so the account reconnects once the network is back. `dataGetHome` also returns `None` when offline, so the bot fetches `facebook.com/robots.txt` (no cookie) to tell the two apart. The counter resets when the previous run lasted 10 minutes or more.
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
- E2EE kinds the bridge cannot read yet (contact cards, polls, view-once, live location...) arrive as empty messages: the bot sends the line `❓ Loại tin chưa hỗ trợ: xem trên Messenger` instead of dropping them. Empty regular messages are still skipped.
- Undecryptable E2EE messages: the bridge only emits a generic error, but the whatsmeow `raw` event (`UndecryptableMessage`) carries the chat, sender and message ID. The bot sends `⚠️ Tin nhắn mã hóa không giải mã được: xem trên Messenger` into the right conversation (own ID `undecryptable-<id>`, so the real message still gets through if it is decrypted later). Own messages and `DecryptFailMode=hide` are skipped.
- Regular (non-E2EE) messages sent while the bot was down: the bridge drops messages older than the connect time, and the sync on connect does not resend missed messages either (Messenger loads them only when a thread is opened), so they are not recovered yet. New Facebook chats are E2EE, which the bridge resyncs itself.
- A reply the bridge does not confirm (deliveryStatus UNKNOWN) still returns 200 so the gateway does not resend it and the customer does not get duplicates.

### Quoting and recalling agent replies

- `POST /send` also takes `reply_to` (messageID of a customer message): the bot sends a quoted reply (`send_e2ee_message(..., reply_to_id, reply_to_sender_jid)` or `send_message(..., reply_to_id)`), the sender JID coming from the cache of the last 4096 messages. It returns `{"ok": true, "message_ids": [...]}`.
- `POST /recall {account, thread_id, message_ids}`: `unsendE2EEMessage` / `unsendMessage` through `BridgeActions`; the `messageUnsend` event of that message is not noted twice.

### Editing, customer state and messaging first

- `POST /edit {account, thread_id, message_id, text}`: `editE2EEMessage`/`editMessage`; that message's own `messageEdit` event is not noted twice.
- Customer typing/seen (`typing`, `readReceipt`, `e2eeReceipt` of type `read`, whatsmeow `ChatPresence` in `raw`) → `POST /v1/presence`; the account's own events are skipped and typing is sent at most every 3 seconds.
- `POST /resolve {account, to}`: a numeric ID or profile link (`profile.php?id=…`, `facebook.com/<id>`) → `{thread_id: "e2ee:<id>@msgr", name, avatar}`.

### Account management (/accounts)

The bot's HTTP server (same `GATEWAY_TOKEN` as `/send`) also serves routes for the Libredesk **Channel accounts** page; the gateway calls them, browsers never do:

| Route | Action |
|---|---|
| `GET /accounts` | Accounts: `name`, `state` (`starting`, `running`, `retrying`, `dropped`, `stopped`), `profile` (`uid`, `displayName`, `avatar`), `error` (last error: `session_expired` when the cookie no longer yields dataFB, sign in again; `connection` otherwise; on expiry `message` adds "Nguyên nhân: …" and `cause`: `revoked` Facebook deleted the `c_user`/`xs` cookies, `checkpoint`, `login`, `token`, `unknown`), `history` (connection history, newest first: `at`, `kind` `connected`/`manual`/`network`/`disconnected`/`platform`/`session_expired`, `message`, `count`/`last_at` when repeated; saved in `<account>/history.json` 0600, at most 30 entries) |
| `POST /accounts/<name>/cookie` `{"cookie": "..."}` | Internal only (the UI signs in with the browser). The cookie must contain `c_user` and `xs`; the bot runs `dataGetHome` to check it with Facebook (422 when rejected, nothing saved), then writes `config.json` (0600) and starts the account |
| `POST /accounts/<name>/restart` | Reconnect (also used after a browser sign-in) |
| `DELETE /accounts/<name>` | Stop and delete the account directory |

The account's own name/picture comes from `/chat/user_info`; some accounts cannot look themselves up, so the LightSpeed contact tables (`LSDeleteThenInsertContact`, `LSVerifyContactRowExists`) sent on connect fill it in. The same tables also name customers found in the contact list.

`AccountManager` (in `multi_main.py`) runs each account in its own task; stopping or reconnecting cancels the task and `SimpleBot.run()` stops the bridge. With the gateway enabled the bot also starts with no accounts.

Browser sign-in uses the `fb-login-ui` service (`login/` image, `control.py` command): idle it is only the control server (~20 MiB); `POST /session {"account"}` runs `extract_cookie.py` with `FORCE_LOGIN=1` (Chromium + noVNC on port 6080), `GET /session` reports `starting` until noVNC accepts connections, then `running`, `saving` once `extract_cookie.py` saw the sign-in and closed Chromium to flush cookies, and `success` once the cookie is written; `DELETE /session` cancels. One session at a time; the Chromium profile is wiped before and after each session so the next account never inherits cookies. The control server requires `GATEWAY_TOKEN` and does not log URLs or headers.

### Rich content and events

- Quotes: E2EE messages only carry `replyTo.messageId`, so the bot caches the last 4096 messages per account (key `account:messageId`) to send `quote` = `Name: text`.
- Stickers are downloaded as images; locations become a Google Maps link; links carry their description.
- Avatars: 1-1 chats send `thread_avatar` = the sender's `thumbSrc` (from the same lookup as the name); groups send each member's `sender_avatar` so the UI can show member avatars.
- Groups: E2EE chats with a `<id>@g.us` JID are groups. E2EE messages carry no group name, so the bot reads group names/pictures from LightSpeed tables in `raw` events (`LSDeleteThenInsertThread`, `LSSyncUpdateThreadName`, `LSSetThreadImageURL`); when unknown it sends an empty name and the gateway uses `👥 Nhóm Facebook` (never the sender's name).
- Facebook group events (rename, member added/left/removed, admin changes) are also read from LightSpeed tables (`LSSyncUpdateThreadName`, `LSAddParticipantIdToGroupThread`, `LSRemoveParticipantFromThread`, `LSUpdateThreadParticipantAdminStatus`) and become `group_event` notes. These tables double as sync data on connect, so the bot only reports changes against known state: a rename when the name differs from the known one, member adds/admin changes only once the group's member list was seen and the batch is not a full thread snapshot. Missing an event beats a burst of false notes on every restart.
- `AccountBot._forward_listener_event` intercepts `e2eeReaction`, `reaction`, `messageEdit` and `messageUnsend` (dropped by the upstream SimpleBot) and sends them to `POST /v1/event`, where they become private notes. Reactions also carry `target_ids` (messageId), `reactor_id` (who reacted, `self` for the account owner) and `reaction` (emoji, empty when removed) so the gateway shows a badge on the right message; when the message is unknown it is still a note.
- Messages whose `senderId` is the account itself (sent from another device) become `owner_message` notes; messages the bot sent through `/send` are recognised by the returned `messageId` or by matching text within 60 seconds.
- Facebook names that cannot be looked up (hidden profile / not friends) use the label `Facebook <id>` and are retried after an hour.

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
