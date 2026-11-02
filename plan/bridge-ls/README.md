# bridge-ls - nghe tin Fanpage thẳng trên socket Lightspeed (không trình duyệt)

> **Đã chuyển (02/11/2026)**: `fbweb-ls` nay là thư mục `fbweb-ls/` của repo omnichannel (service `fbweb-agent`, chỉ chế độ agent, dùng mautrix-meta bản phát hành), quyết định D20 ở `omnichannel/docs/DECISIONS.md`. Bản ở đây không còn được sửa; chỉ giữ cho đường lui trên Mac (container `ls`, `data/`) tới đợt dọn khoảng 04/11/2026 rồi xoá.

`fbweb-ls` là chương trình Go nhỏ thay cho `../bridge-web/browser` (Chromium khoảng 260 MiB). Nó tự mở `wss://gateway.facebook.com/ws/lightspeed` bằng phiên đã đăng nhập trong fbweb-browser, đọc frame Lightspeed của hộp thư Trang và gửi sang `fb-bridge-web` của omnichannel đúng như extension của trình duyệt (`/ingest`, `/heartbeat`). omnichannel không phải đổi gì.

**Trạng thái 01/11/2026:** chạy thật thay cho fbweb-browser từ 31/10 16:00, Trang VetGo; khoảng 11-14 MiB RAM, CPU 0,1%. Đã thử thật trên Trang thử (mục dưới): chữ, ảnh, PDF, tin thoại, video (kể cả 5 MB), sticker, Like, trả lời tin (kể cả tin của Trang), sửa tin, cảm xúc, khách mới, lấy lại tin sau khi mất kết nối (2 phút trên Trang thử, 11 giờ 38 phút trên Trang VetGo). Gửi từ Libredesk (`FBLS_SEND=1`, chỉ Trang thử): chữ, trích dẫn, ảnh, video, mp3, PDF, Word, Excel, cảm xúc; mục "Gửi tin". Chưa chạy lâu. Chế độ agent (nhiều phiên, nhiều Trang, cấu hình trên trang Tài khoản kênh, D18) đã viết và test bằng server giả; liệt kê Trang đã thử thật, nghe Trang qua agent chưa chạy thật.

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
| `lsparse.go` | Parse frame thành sự kiện (giữ đồng bộ với `../bridge-web/bs-inbox-listener.js`; riêng bản Go phát thêm `page_message`) |
| `forward.go` | Gửi fb-bridge-web, tải file (tối đa 20 MiB) |
| `send.go` | Gửi tin trả lời: hỏi `/outbox`, task 46 trên stream dùng một lần, báo `/outbox/result` |
| `upload.go` | Tải tệp lên `upload.php` trước khi gửi, dựng task tệp |
| `react.go` | Thả, gỡ cảm xúc (task 29), sticker Like |
| `store.go` | `data/cookies.json`, `data/state.json`, ghi tạm rồi đổi tên, quyền 600 |
| `pages.go` | Liệt kê Trang của phiên (GraphQL của ô chọn Trang) |
| `pagesquery.go` | Tìm lại `doc_id` và biến của query danh sách Trang trong JS công khai khi Facebook đổi |
| `agent.go` | Chế độ agent: nhiều phiên, nhiều Trang, cấu hình từ fb-bridge-web |
| `export-cookies.sh` | Lấy cookie từ fbweb-browser qua CDP |

```bash
cd plan/bridge-ls
go test -race ./...                               # parser (fixture thật), trang, server DGW giả, forwarder
(cd ../bridge-web/browser && FBWEB_DEBUG=1 docker compose up -d)
./export-cookies.sh                               # data/cookies.json, chỉ in tên cookie
docker compose run --rm ls check                  # một lần tải trang, in độ dài/so khớp, không mở socket
docker compose run --rm ls pages                  # liệt kê Trang của phiên (id, tên), không mở socket
FBLS_DRY_RUN=1 docker compose up -d --build       # chỉ log loại/mid sự kiện, chưa gửi fb-bridge-web
docker compose up -d                              # gửi thật (dùng .env của ../bridge-web/browser)
```

Biến môi trường: `FBWEB_PAGE_ID`, `FBWEB_ACCOUNT`, `FBWEB_TOKEN`, `FBWEB_URL` (như fbweb-browser), `FBLS_DRY_RUN`, `FBLS_DEBUG`, `FBLS_PAGE_NAME` (dự phòng), `FBLS_USER_AGENT`, `FBLS_DATA` (mặc định `/data`), `FBLS_STATE` (tên file cursor trong `FBLS_DATA`, mặc định `state.json`), `FBLS_SEND` (`1`: gửi tin trả lời từ Libredesk).

Image `scratch` khoảng 12 MB, giới hạn RAM 64 MiB (`GOMEMLIMIT` 40 MiB).

## 📤 Gửi tin (bắt frame 01/11/2026)

Bắt bằng CDP trong fbweb-browser khi người dùng trả lời trên Trang thử (chỉ đọc). Business Suite gửi trên **chính socket Lightspeed**, mỗi tin một stream dùng một lần: frame mở stream tham số `{}` rồi data `{"app_id","payload","request_id","type":3}`, `request_id` chung bộ đếm với request đồng bộ. Payload:

```json
{"epoch_id": <ms<<22|...>, "tasks": [{"failure_count": null, "label": "46",
  "payload": "{\"thread_id\":<id khách>,\"otid\":\"<ms<<22|rand>\",\"source\":4456448,\"send_type\":1,\"sync_group\":205,\"mark_thread_read\":1,\"text\":\"...\"}",
  "queue_name": "<id khách>", "task_id": <n>}], "version_id": "<LSVersion>"}
```

- Trả lời một tin: thêm `"reply_metadata":{"reply_source_id":"<mid>","reply_source_type":1,"reply_type":0,"reply_source_attachment_id":null}`.
- Ảnh: `POST upload-business.facebook.com/ajax/mercury/upload.php` (form có `fb_dtsg`, `jazoest`, `lsd`) trả `fbid`, rồi task 46 với `source 0`, `send_type 3`, `text null`, `attachment_fbids [fbid]`; chữ đi kèm là một task 46 riêng trong cùng request. Bắt lần hai (ảnh, PDF): `request_user_id` là **ID Trang**; thân multipart chỉ một trường `upload_<1024+n>`; `fb_dtsg`, `jazoest`, `lsd`, `__user`, `__a` nằm trên URL; ảnh trả `image_id` (có lúc số, có lúc chuỗi), PDF trả `file_id`. `upload.go` làm đúng như vậy; `fb_dtsg`, `lsd` đọc từ trang Hộp thư (`DTSGInitialData`, `LSD`), chỉ giữ trong bộ nhớ.
- Kèm theo trang còn gửi task `21` (đánh dấu đã đọc) và `160012` (kiểm tra khách còn nhận được tin); không bắt buộc, `fbweb-ls` không gửi.
- Server trả lời trên chính stream đó, cùng `request_id`: `taskExists`, **`replaceOptimsiticMessage [otid, mid]`** (Facebook viết sai chính tả), `mailboxTaskCompletionApiOnTaskCompletion`, `removeTask`. Khoảng 1 giây sau tin của Trang quay lại trên stream db 205 (`insertMessage`, sender là Trang, `otid` ở tham số 9).
- Không có trường riêng cho Trang: Trang xác định bằng `x-dgw-app-asset_ids` lúc mở socket. Task giống `SendMessageTask` của mautrix-meta (`messagix/socket/threads.go`).
- Cảm xúc (bắt lần ba): task label `29` trên hàng đợi `reaction:<mid>`, payload `{"thread_key","timestamp_ms","message_id","actor_id":<id Trang>,"reaction":"😄","reaction_style":null,"sync_group":205,"send_attribution":4456448,"dataclass_params":null,"attachment_fbid":null,"offline_threading_id":null}`; server trả `replaceOptimisticReaction`. Chưa bắt được frame gỡ (trang crash); `fbweb-ls` gỡ bằng `reaction ""` như mautrix-meta.
- Sticker, Like: task 46 `{"thread_id","otid","source":4456448,"send_type":2,"sync_group":205,"mark_thread_read":1,"sticker_id":<id>}` (không có khoá `text`); Like là sticker `369239263222822`.
- Sửa, thu hồi tin của Trang: Business Suite trên web không có nút.
- Chromium Alpine của fbweb-browser crash renderer khi mở bảng cảm xúc hoặc sticker có hình động; watchdog mở lại tab ở Trang mặc định trong `.env` (Trang thật), nên sau crash phải xem lại đang ở Trang nào.

`send.go`, `react.go` làm đúng như trên với chữ, trích dẫn, tệp, cảm xúc và "👍" (nút Like) (tới 25 MB mỗi tệp; mỗi tệp một otid, tệp trước chữ sau, cần xác nhận đủ mọi task). Bật bằng `FBLS_SEND=1` (fb-bridge-web cần Trang trong `FBWEB_SEND`). `fbweb-ls` chỉ hỏi hàng đợi (`POST /outbox`, long-poll) khi socket đã theo kịp; chỉ báo thành công khi có `replaceOptimsiticMessage` đúng `otid`. Lỗi trước khi gửi lên socket là `retry` (fb-bridge-web trả `503`, gateway thử lại); đã gửi mà không có xác nhận là `unknown` (`422`, không gửi lại). Tin của Trang trên db 205 thành sự kiện `page_message` (kèm `otid`): fb-bridge-web bỏ tin chính `fbweb-ls` gửi, tin khác thành ghi chú "chủ tài khoản trả lời". Chi tiết phía server: `omnichannel/fb-bridge-web/README.md`, mục "Đường gửi"; quyết định: `omnichannel/docs/DECISIONS.md` D17.

## 🧪 Trang thử

Trang thử `1364936880042409` (tài khoản quản lý: `default`; khách thử là `acc2`) có inbox riêng "Fanpage Web - vetgo-test" trong Libredesk (`FBWEB_PAGES` trên server3). Service `ls-test` trong `docker-compose.yml` nghe Trang này bằng chung cookie với `ls`, cursor riêng (`FBLS_STATE=state-test.json`). Chỉ bật khi thử rồi tắt, vì hai socket chạy trên cùng một phiên; tắt thì fb-bridge-web hiện `vetgo-test` là `dropped`, không phải sự cố.

```bash
docker compose --profile test up -d ls-test   # bật
docker compose --profile test stop ls-test    # tắt
```

Số trên URL `profile.php?id=` của Trang không phải `asset_id` của Business Suite; lấy `asset_id` trên URL Hộp thư.

## 📋 Danh sách Trang (bắt frame 01/11/2026, `pages.go`)

Cho D18 của omnichannel (agent liệt kê Trang để admin chọn). Bắt bằng CDP khi người dùng mở ô chọn Trang ở góc trên bên trái Hộp thư (chỉ đọc, Trang thử), rồi đối chiếu với module JS công khai của trang:

- Tải `latest/inbox/all/` không có `asset_id` (Business Suite chuyển sang Trang mặc định, đi theo một lần) để lấy `fb_dtsg`, `lsd`, `USER_ID`.
- GraphQL `POST business.facebook.com/api/graphql/`, `fb_api_req_friendly_name=NorthStarBusinessUnifiedScopingSelectorFirstAndZeroLevelScopesSectionAllZeroLevelScopesQuery`, `doc_id=37857269907251700` (`FBLS_PAGES_DOC_ID` để thay). Biến chính: `businessToolName "MBS_INBOX"`, `contextUri` là URL Hộp thư, `firstLevelScopeId` và `scopeIDs [..]` là id người dùng (`c_user`), `isPersonalAccount true`, `countLimit 201`, `fetchNumberForPersonalChildrenScopes 40`.
- Biến bắt buộc `__relay_internal__pv__IsBusinessPortfolioOverviewLinkEnabledrelayprovider: true`. Thiếu nó server chỉ trả `missing_required_variable_value`, không nói tên biến; tên lấy từ module `<query>$Parameters` (`providedVariables`) trong JS `static.xx.fbcdn.net` mà trang Hộp thư nạp. `doc_id` cũng nằm ở module `<query>_facebookRelayOperation`.
- **Tự tìm lại** (`pagesquery.go`): query lỗi (không phải hết phiên hay lỗi mạng) thì agent tải các tệp JS công khai mà trang Hộp thư liệt kê (không gửi cookie, 8 luồng, dừng khi thấy đủ hai module), thử lại với `doc_id` và biến mới, đúng thì lưu `FBLS_DATA/pages-query.json` để lần sau dùng ngay. Thử trên JS thật 01/11/2026: khoảng 1.000 tệp, tìm thấy sau 32 giây. Không tìm được thì lỗi hiện ở phiên trên trang Chọn Trang, kèm lý do.
- Phản hồi: `data.viewer.meta_business_scoping.personal_scopes {count, page_info, edges[].node{scope_id, scope_name, scope_type "PAGE", scope_profile_picture{uri}}}`. `scope_id` chính là `asset_id`. Thử thật 01/11/2026 bằng `fbweb-ls pages` với phiên `default`: đủ 5 Trang, khớp `count`.
- Trang thuộc portfolio doanh nghiệp nằm ở `business_scopes` của `NorthStarBusinessUnifiedScopingSelectorPopoverContainerAllFirstLevelScopesQuery` (`doc_id=28183223691347905`); tài khoản này có `count 0` nên chưa làm.

## 🤖 Chế độ agent (`fbweb-ls agent`, omnichannel D18)

Một tiến trình giữ nhiều phiên Facebook, mỗi Trang một listener, cấu hình lấy từ fb-bridge-web thay cho env. Chạy trên server3 là service `fbweb-agent` của omnichannel.

- Phiên: `FBLS_LOGINS/<tên>/config.json` do `fb-login-ui` ghi khi admin bấm "Đăng nhập Facebook" ở kênh Fanpage Web (target `fbweb`). Agent chép cookie sang `FBLS_DATA/logins/<tên>/cookies.json` (một kho cookie cho mọi Trang của phiên), liệt kê Trang ngay, rồi 6 giờ một lần (lỗi thì 10 phút). Cookie nguồn đổi (đăng nhập lại) thì chép lại và khởi động lại các Trang của phiên.
- Đồng bộ: `POST /agent/sync {agent, version, logins[{name, user_id, state, error, pages}]}` tới cổng ingest (long-poll 25 giây). Phản hồi `{version, pages[{account, page_id, login, send}], relist[]}`. Có gì mới (liệt kê xong, hết phiên) thì agent cắt long-poll để báo ngay. Cookie không lên server.
- Mỗi Trang chạy đúng `Listener` như chế độ một Trang, cursor `FBLS_DATA/state-<page id>.json`. Đổi cấu hình thì dừng hẳn bản cũ trước khi chạy bản mới.

Biến: `FBLS_AGENT` (tên agent, bắt buộc), `FBLS_LOGINS` (mặc định `/logins`), `FBLS_DATA`, `FBWEB_URL` (mặc định `http://fb-bridge-web:7008`), `FBWEB_TOKEN`, `FBLS_USER_AGENT`, `FBLS_PAGES_DOC_ID`. Chạy bằng uid 1000 như `fb-login-ui` (cookie quyền 600).

## ⚠️ Chưa biết, cần thử thật

- Chạy lâu: phiên có bền không, socket có bị server đóng định kỳ không, `pageRefresh` 6 giờ có cần không.
- Lưu cursor vào `state.json` hiện vô ích (xem trên); giữ lại cho việc lấy tin trong lúc mất kết nối.
- Phiên dùng ở hai nơi: không chạy fbweb-ls song song với fbweb-browser lâu dài. Dừng trình duyệt khi chạy fbweb-ls, giữ volume profile để đăng nhập lại.
- TLS là Chrome 120 của uTLS (giống mautrix), User-Agent là Chrome 142; lệch nhỏ, mautrix chạy lâu với cách này.

## 🧪 Lấy lại bằng chứng khi Meta đổi

Bật CDP trong fbweb-browser (`FBWEB_DEBUG=1`), ghi `Network.webSocketCreated/FrameSent/FrameReceived` của socket `/ws/lightspeed` rồi tải lại trang. Frame gửi đi: `0x0f` mở stream (header `{}` + frame data lồng), `0x0d` data (`type u8, stream u16 LE, len u24 LE, ackId u16 | 0x8000 nếu cần ack`), `0x0c` ack, `0x0e` đóng stream, `0x09/0x0a` ping/pong. Che giá trị dài trước khi ghi ra file; cookie không nằm trong `webSocketWillSendHandshakeRequest`.
