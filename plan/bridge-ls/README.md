# bridge-ls - nghe tin Fanpage thẳng trên socket Lightspeed (không trình duyệt)

`fbweb-ls` là chương trình Go nhỏ thay cho `../bridge-web/browser` (Chromium khoảng 260 MiB). Nó tự mở `wss://gateway.facebook.com/ws/lightspeed` bằng phiên đã đăng nhập trong fbweb-browser, đọc frame Lightspeed của hộp thư Trang và gửi sang `fb-bridge-web` của omnichannel đúng như extension của trình duyệt (`/ingest`, `/heartbeat`). omnichannel không phải đổi gì.

**Trạng thái 01/11/2026:** chạy thật thay cho fbweb-browser từ 31/10 16:00, Trang VetGo; khoảng 11-14 MiB RAM, CPU 0,1%. Đã thử thật trên Trang thử (mục dưới): chữ, ảnh, PDF, tin thoại, video (kể cả 5 MB), sticker, Like, trả lời tin (kể cả tin của Trang), sửa tin, cảm xúc, khách mới, lấy lại tin sau khi mất kết nối (2 phút trên Trang thử, 11 giờ 38 phút trên Trang VetGo). Chưa chạy lâu.

## 🔍 Trình duyệt gửi gì (ghi bằng CDP 31/10/2026)

Socket dùng giao thức **DGW**, giống mautrix-meta dùng cho Messenger cá nhân (`bridge-e2ee/meta/pkg/messagix/dgw`). Khác biệt của hộp thư Trang:

| | Business Suite | mautrix-meta (Messenger) |
|---|---|---|
| `x-dgw-appid` | `DGWWebConfig.appId` của trang Business Suite (bằng `CurrentUserInitialData.APP_ID`) | của trang Messenger |
| `x-dgw-regionhint` | `PRN` (từ `setRegionHint` trong trang) | không gửi |
| `x-dgw-app-asset_ids` | `{"PAGE":"<page id>"}` | không có |
| Đăng nhập | chỉ cookie (`authtype 1:0`), không token trong frame | như vậy |
| Mở stream | tham số `{}` | để trống |

Các tham số còn lại (`appversion 0`, `version 5`, `uuid` = `c_user`, `tier prod`, `loggingid`, `deviceid` UUID ngẫu nhiên) như mautrix.

Trang mở 4 stream, mỗi stream một request `{"app_id","payload","request_id","type"}`:

| Stream | `type` | Nội dung | fbweb-ls |
|---|---|---|---|
| 0 | 3 (task) | 17 task `label 207` tra contact | không dùng |
| 1 | 2 | db 2, cursor | không dùng |
| 2 | 1 | db 26, `sync_params {"locale"}` | không dùng |
| 3 | 2 | **db 205** (hộp thư Trang), cursor | **chỉ stream này** |

Payload: `{"database":205,"epoch_id":<ms<<22|42>,"failure_count":null,"last_applied_cursor":"<cursor>","sync_params":null,"version":<LSVersion>}`. Server trả `executeFirstBlockForSyncTransactionV4 [205, epoch, currentCursor, nextCursor, ...]` rồi đẩy tin mới trên chính stream đó (frame `insertMessage`... như `../bridge-web/README.md`).

**Cursor đầu tiên lấy từ HTML trang Hộp thư.** Trang có preload GraphQL cho db 205 (`requestType 1`, `sync_params {locale, bloks_version}`) và kết quả nằm sẵn trong `RelayPrefetchedStreamCache`, có `nextCursor` 44 ký tự, đúng cursor trang gửi lên socket. Cùng HTML có `DGWWebConfig`, `CurrentUserInitialData.USER_ID`, `version` (LSVersion, 17 chữ số, trong biến preload chứ không theo mẫu `__d("LSVersion"...)` mautrix tìm) và contact (tên khách, tên Trang).

## 🧪 Kết quả chạy thật (31/10/2026)

- Chỉ mở **một** stream db 205 là đủ; không cần db 2, db 26, task hay socket `/ws/realtime`.
- Cursor **không dùng lại được giữa các kết nối**: cursor của HTML hay cursor đã lưu (kể cả 41 giây trước) đều nhận về `executeFirstBlock` với `SyncStatus 3`, `SendSyncParams true`, `CurrentSeqID -1`; cursor đi kèm lần đó gửi lại thì bị `handleSyncFailure`. Trình duyệt dùng được cursor của HTML có lẽ vì cùng một lần tải trang.
- Đồng bộ lại: `type 1`, cursor `null`, `sync_params` nguyên chuỗi của preload (`locale`, `bloks_version`). Server trả ảnh chụp hộp thư (khoảng 160-240 lời gọi, `SyncStatus 2`).
- **Server chỉ đẩy tin sau khi client theo kịp**: gửi tiếp `type 2` với `nextCursor` cho tới khi `nextCursor == currentCursor` (trình duyệt làm vậy với db 2). Thiếu bước này thì socket sống nhưng không có tin (thử thật hai lần).
- Tin đẩy: frame data không `request_id`, mỗi frame một `executeFirstBlockForSyncTransactionV4` (seq tăng dần) + `insertMessage`/`insertBlobAttachment`..., như frame của trình duyệt.

- Frame trả lời request đầu tiên có thể đến trước khi `EstablishStream` trả về: request đồng bộ tiếp phải chờ stream sẵn sàng (thử thật 01/11).
- Ảnh chụp liệt kê tin mới nhất trước; tin lấy lại được sắp theo thời gian trước khi gửi.
- Link video (và có khi file khác) trỏ tới facebook.com rồi chuyển hướng `302` sang CDN: client tải file theo chuyển hướng, chỉ tới facebook.com hoặc fbcdn.net qua https.

Hệ quả: mỗi lần nối lại là một ảnh chụp lịch sử. Ảnh chụp không được đổ vào Libredesk; chỉ lấy lại **tin của khách mới hơn mốc còn sống cuối cùng** (`live_until_ms` trong `state.json`, ghi mỗi 30 giây khi đã theo kịp, lùi 1 phút, tối đa 24 giờ). Lần chạy đầu không có mốc nên không lấy gì. Cảm xúc và sửa tin trong lúc mất kết nối không lấy lại được. Ảnh chụp chỉ có vài tin gần nhất của mỗi hội thoại: mất kết nối lâu mà một khách nhắn nhiều tin thì có thể thiếu tin cũ hơn.

## ⚙️ Cách chạy

1. Tải trang `https://business.facebook.com/latest/inbox/all/?asset_id=<page>` một lần (như trình duyệt mở trang), đọc `appId`, `USER_ID` (phải bằng `c_user`), `version`, cursor db 205, region, contact.
2. Mở socket DGW (TLS giả Chrome bằng uTLS, User-Agent và client hints đúng Chromium 142 Linux của fbweb-browser), mở một stream db 205 `type 2` với cursor. Server đòi đồng bộ lại thì gửi `type 1` (tối đa 2 lần), rồi `type 2` với `nextCursor` cho tới khi theo kịp (tối đa 10 lần).
3. Mỗi frame: cập nhật cursor (`data/state.json`), parse sự kiện (`lsparse.go`, bản Go của `parseCalls` trong `bs-inbox-listener.js`), khử trùng, tải file đính kèm, `POST /ingest`. Heartbeat 30 giây khi stream còn mở.
4. Rớt socket: nối lại với cursor đang có (nhận tiếp tin bị lỡ), chờ 2 s đến 5 phút. Hỏng ngay khi nối hoặc quá 6 giờ thì tải lại trang trước. Chạy lại tiến trình: dùng cursor đã lưu nếu chưa quá 12 giờ. `handleSyncFailure` thì lấy cursor mới của trang.
5. Hết phiên (trang chuyển sang đăng nhập hoặc `USER_ID` = 0): ghi lỗi, 30 phút thử lại một lần, không heartbeat nên fb-bridge-web báo `dropped`. Đăng nhập lại trong fbweb-browser (`FBWEB_MODE=login`) rồi xuất lại cookie; từng bước ở `omnichannel/docs/OPERATIONS.md`, mục "Fanpage Web: đăng nhập lại".

| File | Vai trò |
|---|---|
| `page.go` | Tải và đọc trang Hộp thư |
| `transport.go` | Danh tính trình duyệt, uTLS, viết lại URL và header bắt tay theo đúng trình duyệt |
| `listener.go` | Socket DGW, stream db 205, cursor, nối lại |
| `lsparse.go` | Parse frame thành sự kiện (giữ đồng bộ với `../bridge-web/bs-inbox-listener.js`) |
| `forward.go` | Gửi fb-bridge-web, tải file (tối đa 20 MiB) |
| `store.go` | `data/cookies.json`, `data/state.json`, ghi tạm rồi đổi tên, quyền 600 |
| `export-cookies.sh` | Lấy cookie từ fbweb-browser qua CDP |

```bash
cd plan/bridge-ls
go test -race ./...                               # parser (fixture thật), trang, server DGW giả, forwarder
(cd ../bridge-web/browser && FBWEB_DEBUG=1 docker compose up -d)
./export-cookies.sh                               # data/cookies.json, chỉ in tên cookie
docker compose run --rm ls check                  # một lần tải trang, in độ dài/so khớp, không mở socket
FBLS_DRY_RUN=1 docker compose up -d --build       # chỉ log loại/mid sự kiện, chưa gửi fb-bridge-web
docker compose up -d                              # gửi thật (dùng .env của ../bridge-web/browser)
```

Biến môi trường: `FBWEB_PAGE_ID`, `FBWEB_ACCOUNT`, `FBWEB_TOKEN`, `FBWEB_URL` (như fbweb-browser), `FBLS_DRY_RUN`, `FBLS_DEBUG`, `FBLS_PAGE_NAME` (dự phòng), `FBLS_USER_AGENT`, `FBLS_DATA` (mặc định `/data`), `FBLS_STATE` (tên file cursor trong `FBLS_DATA`, mặc định `state.json`).

Image `scratch` khoảng 12 MB, giới hạn RAM 64 MiB (`GOMEMLIMIT` 40 MiB).

## 🧪 Trang thử

Trang thử `1364936880042409` (tài khoản quản lý: `default`; khách thử là `acc2`) có inbox riêng "Fanpage Web - vetgo-test" trong Libredesk (`FBWEB_PAGES` trên server3). Service `ls-test` trong `docker-compose.yml` nghe Trang này bằng chung cookie với `ls`, cursor riêng (`FBLS_STATE=state-test.json`). Chỉ bật khi thử rồi tắt, vì hai socket chạy trên cùng một phiên; tắt thì fb-bridge-web hiện `vetgo-test` là `dropped`, không phải sự cố.

```bash
docker compose --profile test up -d ls-test   # bật
docker compose --profile test stop ls-test    # tắt
```

Số trên URL `profile.php?id=` của Trang không phải `asset_id` của Business Suite; lấy `asset_id` trên URL Hộp thư.

## ⚠️ Chưa biết, cần thử thật

- Chạy lâu: phiên có bền không, socket có bị server đóng định kỳ không, `pageRefresh` 6 giờ có cần không.
- Lưu cursor vào `state.json` hiện vô ích (xem trên); giữ lại cho việc lấy tin trong lúc mất kết nối.
- Phiên dùng ở hai nơi: không chạy fbweb-ls song song với fbweb-browser lâu dài. Dừng trình duyệt khi chạy fbweb-ls, giữ volume profile để đăng nhập lại.
- TLS là Chrome 120 của uTLS (giống mautrix), User-Agent là Chrome 142; lệch nhỏ, mautrix chạy lâu với cách này.

## 🧪 Lấy lại bằng chứng khi Meta đổi

Bật CDP trong fbweb-browser (`FBWEB_DEBUG=1`), ghi `Network.webSocketCreated/FrameSent/FrameReceived` của socket `/ws/lightspeed` rồi tải lại trang. Frame gửi đi: `0x0f` mở stream (header `{}` + frame data lồng), `0x0d` data (`type u8, stream u16 LE, len u24 LE, ackId u16 | 0x8000 nếu cần ack`), `0x0c` ack, `0x0e` đóng stream, `0x09/0x0a` ping/pong. Che giá trị dài trước khi ghi ra file; cookie không nằm trong `webSocketWillSendHandshakeRequest`.
