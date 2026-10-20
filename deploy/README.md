# fbchat-v2 - Triển khai bằng Docker (fork)

[English](README_EN.md)

Thư mục này là phần riêng của fork: chạy bot mẫu trong Docker, đăng nhập Facebook qua trình duyệt trên web (noVNC) và chạy nhiều tài khoản trong một container. Mọi thứ nằm trong `deploy/`, không sửa file nào của upstream, nên đồng bộ với repo gốc không bị conflict.

## 📂 Cấu trúc

```text
deploy/
├── docker-compose.yml          # project "fbchat-v2", volume fbchat-data
├── login/                      # Alpine + Chromium + noVNC, lấy cookie rồi tự dừng
│   ├── Dockerfile
│   ├── extract_cookie.py
│   └── control.py              # dịch vụ fb-login-ui: bật trình duyệt theo yêu cầu
├── bot/                        # python:3.12-alpine + bridge Go build tĩnh
│   ├── Dockerfile
│   └── Dockerfile.dockerignore # ignore file riêng (BuildKit), không đụng thư mục gốc
├── multi_main.py               # runner nhiều tài khoản, chỉ dùng API của src/main.py
├── gateway_link.py             # nối với omnichannel gateway (khi có GATEWAY_URL)
└── tests/                      # test runner + contract test với upstream
```

## 🚀 Bắt đầu nhanh

```bash
cd deploy
docker compose up -d --build
```

Muốn chạy lệnh `docker compose` từ thư mục gốc mà không cần `-f`, tạo file `.env` ở thư mục gốc (upstream đã gitignore `.env`):

```bash
echo "COMPOSE_FILE=deploy/docker-compose.yml" >> .env
```

1. `login` mở trình duyệt tại <http://localhost:6080/vnc.html?autoconnect=1&resize=scale>. Đăng nhập Facebook ở đó.
2. Khi có cookie `c_user` và `xs`, `login` ghi `/data/accounts/<ACCOUNT>/config.json` (quyền `0600`, `ACCOUNT` mặc định `default`) vào volume rồi tự dừng. Có thể phải chờ tới khoảng 30 giây để Chromium ghi cookie xuống đĩa; màn hình "Aw, Snap!" lúc đó là bình thường.
3. `bot` khởi động sau khi `login` thành công và chạy mọi tài khoản trong `/data/accounts`.

## 👥 Nhiều tài khoản

```bash
ACCOUNT=acc2 docker compose run --rm --service-ports login            # đăng nhập thêm acc2
docker compose restart bot                                            # nạp tài khoản mới
FORCE_LOGIN=1 ACCOUNT=acc2 docker compose run --rm --service-ports login  # đăng nhập lại khi cookie hết hạn
```

- Mọi tài khoản dùng chung một tiến trình Python, event loop và `httpx.AsyncClient`. Mỗi tài khoản vẫn có một tiến trình bridge Go riêng vì bridge chỉ hỗ trợ một client.
- Log có nhãn tài khoản, ví dụ `[acc2] [bot] ...`. Dòng log của chính bridge chưa có nhãn.
- Tài khoản lỗi được thử lại với backoff (30 giây, nhân đôi, tối đa 10 phút) và bị bỏ sau 5 lần lỗi liên tiếp mà không làm dừng tài khoản khác. Bộ đếm reset nếu lần chạy trước kéo dài từ 10 phút trở lên.
- Tên tài khoản chỉ gồm chữ, số, `_`, `.`, `-` (tối đa 64 ký tự, bắt đầu bằng chữ hoặc số). Xóa tài khoản: dừng bot, xóa `/data/accounts/<tên>` trong volume rồi khởi động lại.

## 🩺 Healthcheck và restart

- `multi_main.py` ghi trạng thái từng tài khoản (`starting`, `running`, `retrying`, `dropped`) vào `FBCHAT_HEALTH_FILE` (`/tmp/fbchat-health.json` trong image).
- Container chỉ `healthy` khi mọi tài khoản là `running`; `docker compose ps` hiện `unhealthy` khi có tài khoản đang thử lại hoặc bị bỏ.
- Bot dùng `restart: unless-stopped`. Khi mọi tài khoản bị bỏ, tiến trình vẫn chạy (không thoát) để Docker không restart và gọi lại Facebook bằng cookie hỏng. Hãy đăng nhập lại rồi `docker compose restart bot`.

## 📊 Tài nguyên

| Dịch vụ | Image | Giới hạn | Đo thực tế |
|---|---|---|---|
| `login` | `alpine:3.22` + Chromium (~880 MB) | 1 CPU, 768 MiB | ~225-325 MiB khi đang đăng nhập, sau đó dừng |
| `bot` | `python:3.12-alpine` (~95 MB) | `BOT_CPUS` (mặc định 1), `BOT_MEMORY_LIMIT` (mặc định 512M) | Python ~25 MiB chung + 15-25 MiB mỗi tài khoản lúc rảnh; 2 tài khoản ~60 MiB, CPU < 1% |

Tăng `BOT_MEMORY_LIMIT` khi thêm nhiều tài khoản hoặc gửi media lớn, vì bridge giữ cả file media trong RAM khi gửi hoặc tải. Sau khi đăng nhập xong có thể xóa image login: `docker image rm fbchat-v2-login`.

Biến môi trường (shell hoặc `.env`): `ACCOUNT`, `FORCE_LOGIN`, `LOGIN_TIMEOUT` (giây, mặc định 900), `VNC_PASSWORD`, `BOT_ADMINS` (ID cách nhau bằng dấu phẩy), `BOT_PREFIX`, `BOT_CPUS`, `BOT_MEMORY_LIMIT`.

## 🛡️ Bảo mật

- noVNC chỉ bind `127.0.0.1`. Profile Chromium nằm trên tmpfs và mất khi container dừng.
- Chromium không bật remote debugging hay automation flag; script đọc và giải mã file SQLite `Cookies` trong profile (`--password-store=basic`). Policy tắt DevTools và trình quản lý mật khẩu.
- Chromium chạy với `--no-sandbox` vì seccomp mặc định của Docker chặn sandbox của nó; container chỉ dùng để mở Facebook.
- Bot chạy user không phải root, root FS read-only, `cap_drop: ALL`, `no-new-privileges`. Cookie nằm trong volume `fbchat-data`, không nằm trong image hay thư mục repo.

## 🔗 Chế độ omnichannel (gateway)

Khi đặt `GATEWAY_URL` và `GATEWAY_TOKEN` (ít nhất 32 ký tự), `multi_main.py` chuyển mọi tin khách sang omnichannel gateway (`POST /v1/inbound`) thay vì chạy lệnh `/ping`..., và mở `POST /send` tại cổng `BOT_HTTP_PORT` (mặc định 7002) để nhận tin nhân viên trả lời. Đặt `BOT_COMMANDS=1` để vẫn chạy lệnh. Code nằm trong `gateway_link.py`; stack đầy đủ (Libredesk + gateway + bot) ở thư mục `omnichannel/` riêng.

- `thread_id` gửi cho gateway: `e2ee:<chatJid>` cho chat mã hóa, `thread:<threadId>` cho chat thường; nhóm nhận biết theo `threadType`.
- Tên người gửi lấy bằng `_get_user_info` và được cache.
- File đính kèm (ảnh, video, audio, file): bot tải qua `downloadE2EEMedia` (chat mã hóa) hoặc `downloadMedia` (chat thường) rồi gửi kèm base64 cho gateway; file nhân viên gửi đi bằng `sendE2EEImage`/`sendE2EEVideo`/`sendE2EEAudio`/`sendE2EEDocument` hoặc `sendImage`/`sendFile`. Sticker, link, vị trí chỉ chuyển thành dòng mô tả. Giới hạn mỗi tệp `GATEWAY_MAX_FILE_MB` (mặc định 20).
- Loại tin E2EE bridge chưa đọc được (danh thiếp, bình chọn, xem một lần, vị trí trực tiếp...) đến dưới dạng tin rỗng: bot gửi dòng `❓ Loại tin chưa hỗ trợ: xem trên Messenger` thay vì bỏ qua. Tin thường rỗng vẫn bỏ qua.
- Tin E2EE không giải mã được: bridge chỉ phát lỗi chung, nhưng sự kiện `raw` của whatsmeow (`UndecryptableMessage`) có chat, người gửi và ID tin. Bot gửi tin `⚠️ Tin nhắn mã hóa không giải mã được: xem trên Messenger` vào đúng hội thoại (ID riêng `undecryptable-<id>`, nên nếu sau đó giải mã lại được thì tin thật vẫn vào). Bỏ qua tin của chính mình và loại `DecryptFailMode=hide`.
- Tin chat thường (không E2EE) gửi lúc bot không chạy: bridge bỏ tin cũ hơn lúc kết nối, và đợt đồng bộ khi kết nối cũng không gửi lại tin bị lỡ (Messenger chỉ tải khi mở hội thoại), nên chưa lấy lại được. Chat Facebook mới đều là E2EE, bridge tự đồng bộ lại.
- Tin trả lời không có xác nhận từ bridge (deliveryStatus UNKNOWN) vẫn trả 200 để gateway không gửi lại, tránh khách nhận trùng.

### Quản lý tài khoản (/accounts)

Server HTTP của bot (cùng `GATEWAY_TOKEN` với `/send`) có thêm các route cho trang **Channel accounts** của Libredesk; gateway gọi, trình duyệt không gọi trực tiếp:

| Route | Việc |
|---|---|
| `GET /accounts` | Danh sách: `name`, `state` (`starting`, `running`, `retrying`, `dropped`, `stopped`), `profile` (`uid`, `displayName`, `avatar`), `error` (lỗi gần nhất: `session_expired` khi cookie không còn cho ra dataFB, cần đăng nhập lại; `connection` cho lỗi khác) |
| `POST /accounts/<tên>/cookie` `{"cookie": "..."}` | Chỉ dùng nội bộ (giao diện chỉ đăng nhập bằng trình duyệt). Cookie phải có `c_user` và `xs`; bot gọi `dataGetHome` để kiểm tra với Facebook (422 nếu bị từ chối, không lưu), rồi ghi `config.json` (0600) và chạy tài khoản |
| `POST /accounts/<tên>/restart` | Kết nối lại (cũng dùng sau khi đăng nhập bằng trình duyệt) |
| `DELETE /accounts/<tên>` | Dừng và xoá thư mục tài khoản |

`AccountManager` (trong `multi_main.py`) chạy mỗi tài khoản trong một task; dừng hoặc kết nối lại là huỷ task, `SimpleBot.run()` tự dừng bridge. Khi bật gateway, bot vẫn khởi động khi chưa có tài khoản nào.

Đăng nhập bằng trình duyệt dùng dịch vụ `fb-login-ui` (image `login/`, lệnh `control.py`): rảnh chỉ có server điều khiển (~20 MiB); `POST /session {"account"}` chạy `extract_cookie.py` với `FORCE_LOGIN=1` (Chromium + noVNC cổng 6080), `GET /session` trả `starting` tới khi noVNC sẵn sàng rồi `running`, `saving` khi `extract_cookie.py` đã thấy đăng nhập và đóng Chromium để ghi cookie, `success` khi đã ghi cookie, `DELETE /session` huỷ. Mỗi lúc một phiên; hồ sơ Chromium bị xoá trước và sau mỗi phiên để tài khoản sau không nhận nhầm cookie. Server điều khiển cần `GATEWAY_TOKEN` và không log URL/header.

### Tin giàu nội dung và sự kiện

- Trích dẫn: tin E2EE chỉ có `replyTo.messageId`, nên bot nhớ 4096 tin gần nhất mỗi tài khoản (khoá `tài khoản:messageId`) để gửi `quote` = `Tên: nội dung`.
- Sticker tải về như ảnh; vị trí thành link Google Maps; link kèm mô tả.
- Avatar: chat 1-1 gửi `thread_avatar` = `thumbSrc` của người nhắn (cùng lần tra tên); nhóm gửi `sender_avatar` của từng người để giao diện hiện avatar thành viên.
- Nhóm: chat E2EE có JID `<id>@g.us` là nhóm. Tin E2EE không mang tên nhóm, nên bot đọc tên/ảnh nhóm từ bảng LightSpeed trong sự kiện `raw` (`LSDeleteThenInsertThread`, `LSSyncUpdateThreadName`, `LSSetThreadImageURL`); chưa biết thì gửi tên trống và gateway đặt `👥 Nhóm Facebook` (không bao giờ lấy tên người gửi).
- Sự kiện nhóm Facebook (đổi tên, thêm/rời/xoá thành viên, quản trị viên) cũng đọc từ bảng LightSpeed (`LSSyncUpdateThreadName`, `LSAddParticipantIdToGroupThread`, `LSRemoveParticipantFromThread`, `LSUpdateThreadParticipantAdminStatus`) thành ghi chú `group_event`. Các bảng này cũng là dữ liệu đồng bộ lúc kết nối, nên bot chỉ báo khi khác trạng thái đã biết: đổi tên khi tên khác tên đã biết, thêm thành viên/đổi quản trị chỉ khi đã thấy danh sách thành viên nhóm đó và lô không phải ảnh chụp cả hội thoại. Thà bỏ sót còn hơn báo sai cả loạt mỗi lần khởi động.
- `AccountBot._forward_listener_event` chặn `e2eeReaction`, `reaction`, `messageEdit`, `messageUnsend` (SimpleBot upstream lọc bỏ) và gửi `POST /v1/event`, thành ghi chú nội bộ.
- Tin có `senderId` là chính tài khoản (gửi từ thiết bị khác) thành ghi chú `owner_message`; tin do bot gửi qua `/send` được nhận ra bằng `messageId` trả về hoặc trùng nội dung trong 60 giây.
- Tên Facebook không tra được (ẩn hồ sơ / không phải bạn bè) dùng nhãn `Facebook <id>` và được tra lại sau 1 giờ.

### Trạng thái thiết bị E2EE

`AccountBot` lưu trạng thái thiết bị E2EE vào `/data/accounts/<tên>/e2ee_device.json` (quyền `0600`). Upstream mặc định giữ trạng thái này trong RAM (`e2ee_memory_only=True`): mỗi lần restart là một thiết bị E2EE mới và tin đến thường báo `E2EE message could not be decrypted or was unavailable`. Đặt `FBCHAT_E2EE_PERSIST=0` để quay lại hành vi upstream. File này là khóa mã hóa của tài khoản: không copy ra ngoài volume.

## 🔄 Đồng bộ với upstream

Remote `upstream` trỏ tới repo gốc, `origin` trỏ tới fork:

```bash
git fetch upstream
git merge upstream/main          # hoặc: git rebase upstream/main
cd deploy && docker compose build && docker compose up -d
```

Vì fork chỉ thêm file trong `deploy/`, merge không conflict. Rủi ro còn lại là upstream đổi API mà `multi_main.py` dựa vào; khi đó chạy:

```bash
pytest deploy/tests
```

Các điểm bám vào upstream (được `test_upstream_contract_used_by_fork` kiểm tra):

- `src/main.py`: `SimpleBot` (tham số `prefix`, `admins`, `log_message_content`, `debug_errors`, `http_client`; method `_queue_event`), `log`, `DEFAULT_HTTP_TIMEOUT`, `is_valid_datafb`.
- `listeningE2EEEvent`: thuộc tính `device_path`, `startup_timeout`, `wait_until_connected(timeout, require_e2ee=...)`.
- `dataGetHome(storage=...)`, `FileSessionStorage`, `set_private_file_permissions`.
