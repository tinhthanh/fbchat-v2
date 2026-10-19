# fbchat-v2 - Triển khai bằng Docker (fork)

[English](README_EN.md)

Thư mục này là phần riêng của fork: chạy bot mẫu trong Docker, đăng nhập Facebook qua trình duyệt trên web (noVNC) và chạy nhiều tài khoản trong một container. Mọi thứ nằm trong `deploy/`, không sửa file nào của upstream, nên đồng bộ với repo gốc không bị conflict.

## 📂 Cấu trúc

```text
deploy/
├── docker-compose.yml          # project "fbchat-v2", volume fbchat-data
├── login/                      # Alpine + Chromium + noVNC, lấy cookie rồi tự dừng
│   ├── Dockerfile
│   └── extract_cookie.py
├── bot/                        # python:3.12-alpine + bridge Go build tĩnh
│   ├── Dockerfile
│   └── Dockerfile.dockerignore # ignore file riêng (BuildKit), không đụng thư mục gốc
├── multi_main.py               # runner nhiều tài khoản, chỉ dùng API của src/main.py
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
