# bridge-web - nghe tin Fanpage từ socket của Business Suite

Ghi chú cho session sau: script làm gì, dữ liệu đi qua đâu, cách bắt lại frame khi Meta đổi định dạng. Không liên quan tới `omnichannel/` hay `fbpage-bot` (bot đó dùng Graph API chính thức).

## 📂 Nội dung

| File | Vai trò |
|---|---|
| `bs-inbox-listener.js` | Script chạy trong trang Hộp thư Business Suite, in tin khách ra console |
| `browser/` | Image Docker `fbweb-browser` (Chromium + extension + noVNC) để chạy trên Mac không cần Tampermonkey |
| `test-case/` | Test tự động (Node) và checklist test trên trang thật, xem [`test-case/README.md`](test-case/README.md) |
| `fixtures/frames-2026-10-31.json` | Tin chữ, ảnh, 3 ảnh, video, tin thoại, PDF, sticker, link, trả lời, thả cảm xúc |
| `fixtures/frames-2026-10-31-newthread.json` | Khách mới nhắn lần đầu (Like), tin thoại, video, GIF |
| `fixtures/frames-2026-10-31-edit.json` | Sửa tin, thả 👍, gỡ 👍 (viết tay từ frame thật, chỉ giữ lời gọi cần cho test) |

Trong fixture, URL CDN có chữ ký đã thay bằng `<url>`, MPD video thay bằng `<mpd>`.

```bash
node --test 'plan/bridge-web/test-case/*.test.js'   # pass 38, fail 0
```

## 🚀 Dùng

Trang: `https://business.facebook.com/latest/inbox/all/?asset_id=<page id>`. `asset_id` là ID Trang, script dùng nó để bỏ tin Trang tự gửi.

- **Tampermonkey**: cài file, header `@run-at document-start` bắt socket ngay từ đầu (cả đợt đồng bộ lúc tải trang, có tên khách của các hội thoại gần đây). `@sandbox JavaScript` + `unsafeWindow`: script chạy cạnh trang nhưng vẫn có `GM_*`.
- **DevTools Console**: dán nội dung file. Socket đang mở được bắt ở lần `ws.send()` kế tiếp (ping, vài giây). Chỉ in console, không chuyển tiếp (không có `GM_xmlhttpRequest`).
- Dừng: `window.__bsInboxStop()`.

### Chạy bằng Docker trên Mac (khuyến nghị)

`browser/` là image `fbweb-browser`: Chromium trên Alpine mở Hộp thư Business Suite với extension tự viết nạp sẵn (không cần Tampermonkey).

```bash
cd plan/bridge-web/browser
cp .env.example .env                          # điền FBWEB_TOKEN (lệnh lấy token ghi trong file), quyền 600
FBWEB_MODE=login docker compose up -d --build  # có noVNC để đăng nhập Facebook
open "http://localhost:6090/vnc.html?autoconnect=1&resize=scale"
docker compose up -d                          # đăng nhập xong: chế độ run (mặc định), không VNC
```

| Chế độ | Chạy gì | Trang |
|---|---|---|
| `run` (mặc định) | Xvfb + openbox + Chromium | Chặn video và ảnh của trang; `quiet.js` tắt animation và báo trang đang ở nền |
| `login` | thêm x11vnc + noVNC `127.0.0.1:6090` | Chỉ chặn video; giao diện bình thường để đăng nhập, xử lý checkpoint |

Hai chế độ dùng chung profile trong volume `fbweb-browser_profile`; xoá volume là đăng xuất.

| Phần | Vai trò |
|---|---|
| `extension/manifest.json` | MV3; `quiet.js` và `bs-inbox-listener.js` (chính file của bản Tampermonkey) ở world `MAIN`, `relay.js` ở isolated world, `document_start` trên `/latest/inbox/*` |
| `extension/quiet.js`, `quiet-run.js` | Bản rỗng cho `login`; `run` dùng `quiet-run.js`: CSS `animation/transition: none`; `document.visibilityState = "hidden"`, `hasFocus() = false`; gỡ và chặn service worker của Facebook; thay web worker `webworker_v1` bằng worker giả |
| `extension/rules.json` | `declarativeNetRequest` chặn `media` (và `image` ở `run`) do trang tự tải; yêu cầu của service worker không bị chặn |
| `extension/relay.js` | Nhận `postMessage` của script (`__bsInbox: "hello" | "event"`), chuyển cho service worker |
| `extension/sw.js` | Tải file (cookie trình duyệt), `POST /ingest` có hàng đợi và gửi lại, `POST /heartbeat` mỗi lần trang báo "hello" (30 giây). Fetch của extension không bị CSP của trang chặn |
| `entrypoint.sh` | Xvfb, openbox, (x11vnc, websockify ở `login`); ghi `config.json` (token) và luật chặn vào bản sao extension trong tmpfs; watchdog; mở lại Chromium nếu tắt |

**Đo ngày 31/10/2026 (trang Business Suite còn sống, Trang VetGo):**

| Cấu hình | CPU | RAM |
|---|---|---|
| Có giao diện, chưa tối ưu | 17-20%, **renderer crash sau 1-3 phút** | khoảng 400 MiB |
| `--headless=new` | khoảng 45% (GPU process dựng khung liên tục; `--enable-begin-frame-control` không có tác dụng) | khoảng 500 MiB |
| + chặn ảnh/video, `quiet.js` (animation, hidden) | khoảng 5% | 420-480 MiB |
| + gỡ service worker của Facebook | khoảng 3-4% | khoảng 436 MiB |
| + `--enable-low-end-device-mode`, `--max-semi-space-size=2` | khoảng 3-4% | khoảng 385 MiB |
| **+ worker giả cho `webworker_v1`** (hiện tại) | **khoảng 1,5-3%** | **khoảng 388 MiB** (renderer trang khoảng 250 MiB, Chromium khoảng 110-170 MiB, Xvfb khoảng 40 MiB) |

Bài học khi đo:
- **Renderer crash khi giải mã H.264** (dump có `av:h264`, `subresource_url` là `.mp4` của fbcdn, vd. GIF Facebook đổi thành mp4) trên Chromium Alpine. Vì vậy trang không được tải video. Số "180 MiB, 0% CPU" đo trước khi biết là của một tab đã chết; kiểm tra luôn có renderer của trang (không có `--extension-process`) trước khi tin số đo.
- `--in-process-gpu` làm Chromium crash ngay. `--no-zygote` không phải nguyên nhân crash nhưng đã bỏ cho an toàn.
- Openbox thu nhỏ cửa sổ (`<iconic>`) không làm trang thành `hidden`; phải giả `visibilityState` trong trang.
- Đo trang bằng CDP (`Performance.getMetrics` và CPU profile 15 giây): JS của trang rảnh khoảng 90%, JS heap chỉ khoảng 72 MiB. CPU còn lại là service worker và web worker của Facebook; đo theo luồng (`/proc/<pid>/task/*/stat`) mới thấy, đo theo tiến trình thì chỉ thấy "renderer".
- Gỡ service worker và thay web worker vẫn nhận đủ tin (thử 31/10: chữ, 4 ảnh, cảm xúc vào Libredesk).
- Watchdog bằng `chrome.alarms` trong service worker không chạy được (SW ngủ, alarm không đánh thức được dù không gọi `create` lặp lại); watchdog nằm trong `entrypoint.sh`.

**Watchdog** (`entrypoint.sh`): mỗi phút xem còn renderer của trang không; mất 2 phút liền (tab "Aw, Snap") thì khởi động lại Chromium (thử 31/10: crash giả 11:19:40, trang chạy lại 11:22:12). Cứ `RESTART_HOURS` (mặc định 6) giờ khởi động lại để xả bộ nhớ. Tin đến lúc khởi động lại vẫn tới: Facebook gửi lại dạng `upsertMessage` còn mới khi trang đồng bộ. Bị đăng xuất thì watchdog không phát hiện; bot báo `dropped` và Telegram.

Gỡ lỗi: `FBWEB_DEBUG=1 docker compose up -d` in console của trang ra `docker compose logs` và mở cổng CDP `127.0.0.1:9222` **bên trong** container (đọc bằng `docker exec ... wget -qO- http://127.0.0.1:9222/json`). Console của service worker không ra log; xem phía bot bằng `docker compose logs fb-bridge-web` trên server3 (`ingest forwarded/skipped/rejected`).

Container gọi `host.docker.internal:7008`, tức SSH tunnel của Mac tới server3. Đây là một phiên Facebook riêng (trình duyệt Linux trên IP nhà), song song với Chrome thường và fb-bot trên server3.

### Chuyển vào omnichannel (fb-bridge-web)

Khi chạy bằng Tampermonkey và đã cấu hình, mỗi sự kiện được gửi sang bot `fb-bridge-web` của omnichannel (`omnichannel/fb-bridge-web/README.md`), vào Libredesk ở inbox `Fanpage Web - <tên>`:

- Menu Tampermonkey → **Cấu hình fb-bridge-web**: địa chỉ cổng ingest (`http://127.0.0.1:7008`, SSH tunnel tới server3), tên tài khoản trong `FBWEB_PAGES`, `FBWEB_INGEST_TOKEN`. Lưu bằng `GM_setValue`, không nằm trong file.
- File đính kèm được tải ngay trong trình duyệt (`GM_xmlhttpRequest`, cookie của trình duyệt, tối đa 20 MiB) rồi gửi base64; tải lỗi thì gửi kèm `error`.
- Hàng đợi tuần tự giữ thứ tự tin; 502 hoặc lỗi mạng thì gửi lại sau 2, 5, 15, 30, 60 giây; 4xx thì bỏ và ghi `console.warn`.
- Heartbeat `POST /heartbeat` 30 giây một lần; quá 3 phút không có thì bot báo `dropped`.
- `GM_xmlhttpRequest` cần vì CSP `connect-src` của business.facebook.com chặn `fetch` tới địa chỉ khác.

Mỗi sự kiện in `[BS-INBOX] {...}` và phát `window` event `bs-inbox-message`:

```js
window.addEventListener("bs-inbox-message", (e) => console.log(e.detail));
```

| `kind` | Trường |
|---|---|
| `message` | `mid, threadId, senderId, senderName, text, timestampMs, stickerId, replyTo {mid, text, senderId}, attachments[]` |
| `reaction` | `mid, threadId, actorId, reaction, timestampMs` |
| `unreaction` | `mid, threadId, actorId` |
| `edit` | `mid, text, editCount` |

`attachments[].kind`: `image`, `gif`, `video`, `audio`, `file`, `link`, `sticker`.

## 🔌 Dữ liệu đi qua đâu

Business Suite mở nhiều WebSocket **trên trang chính** (không phải trong worker):

| Socket | Nội dung |
|---|---|
| `wss://gateway.facebook.com/ws/lightspeed` | **Tin nhắn Trang**, dạng Lightspeed. Script chỉ nghe socket này |
| `wss://edge-chat.facebook.com/chat` | MQTT, khi theo dõi chỉ thấy ping (PINGRESP) |
| `gateway.facebook.com/ws/realtime`, `rpsignaling`, `streamcontroller` | Framing riêng, không chứa tin |

Không có GraphQL cho tin đến; `/api/graphql/` chỉ dùng khi thao tác.

### Định dạng frame Lightspeed

```text
<vài byte header nhị phân>{"request_id":null,"payload":"<chuỗi JSON>","sp":["tênProcedure", ...]}
```

`payload` parse ra `{"name":null,"step":[...]}`. `step` là cây bytecode:

- `[5, "tênProcedure", ...tham số]`: gọi stored procedure. Script gom mọi node này.
- `[19, "123"]`: int64 dạng chuỗi.
- `[9]`: null.
- Các số `1`, `23`, `26`... ở đầu mảng khác là block/điều kiện, bỏ qua.

Mỗi frame có `executeFirstBlockForSyncTransactionV4` (sync db `205`) và `executeFinallyBlockForSyncTransaction` bao quanh; `upsertSequenceId [seq, pageId, 205]` cho biết page ID.

Facebook **gửi lặp cùng một sự kiện 2 lần** (khác sequence id). Script khử trùng theo `mid` (tin), `edit:mid:editCount`, `kind:mid:actor:reaction:ts`.

## 📋 Procedure và chỉ số tham số

Chỉ số tính từ tham số đầu tiên sau tên procedure. Nguồn: `bridge-e2ee/meta/pkg/messagix/table/` (`messages.go`, `attachments.go`, `enums.go`), đã đối chiếu với frame thật.

| Procedure | Dùng cho | Chỉ số quan trọng |
|---|---|---|
| `insertMessage` | Tin mới trong hội thoại đã có | 0 text, 3 threadKey, 5 timestampMs, 8 mid, 9 otid, 10 senderId, 11 stickerId, 23 replySourceId (mid gốc), 27 replySnippet, 28 replyMessageText, 29 replyToUserId, 58 replySourceTimestampMs |
| `upsertMessage` | Tin đầu tiên của khách mới **và** lịch sử khi mở hội thoại | Giống `insertMessage`. Script chỉ nhận nếu `timestampMs` chưa quá 2 phút |
| `insertBlobAttachment` | Ảnh, GIF, video, tin thoại | 0 filename, 1 filesize, 3 playableUrl, 6 playableMime, 8 previewUrl, 14/15 rộng/cao, 22 durationMs, 27 threadKey, 29 attachmentType, 30 mime, 32 mid, 34 fbid |
| `insertXmaAttachment` | File (PDF...) và link chia sẻ | 4 playableUrl (có = file, không có = link), 7 mime, 25 threadKey, 27 type (7 = XMA), 30 mid, 32 attachmentFbid, 58 title/tên file, 59 subtitle ("172 KiB" hoặc domain), 110/111 `generic_share` với link |
| `insertAttachmentCta` | URL thật của link | 1 attachmentFbid, 7 `xma_web_url`, 9 url |
| `insertStickerAttachment` | Sticker, Like | 4 previewUrl, 13 mô tả ("Like, thumbs up"), 14 threadKey, 18 mid, 19 stickerId |
| `upsertReaction` | Thả cảm xúc | 0 threadKey, 1 timestamp **của tin được thả**, 2 mid, 3 actorId, 4 emoji, 6 lúc thả (có thể null) |
| `deleteReaction` | Gỡ cảm xúc | 0 threadKey, 1 mid, 2 actorId |
| `editMessage` | Sửa tin | 0 mid, 2 text mới, 3 editCount |
| `verifyContactRowExists` | Tên khách | 0 id, 3 name |
| `deleteThenInsertContact` | Tên khách | 0 id, 9 name |

`attachmentType` (enum): 1 sticker, 2 image, 3 animated image (GIF, mime `video/mp4`), 4 video, 5 audio (`audio/opus`), 6 file, 7 XMA.

Like 👍 là sticker `369239263222822`.

Procedure thấy trong frame nhưng script bỏ qua: `bumpThread`, `updateThreadSnippet`, `updateReadReceipt`, `updateTypingIndicator`, `setMessageDisplayedContentTypes`, `setForwardScore`, `updateOrInsertThread`, `insertNewMessageRange`, `insertAttachmentItem`...

### Thread của khách mới

Khách nhắn Trang lần đầu không đến bằng `insertMessage` mà bằng một gói: `updateOrInsertThread`, `addParticipantIdToGroupThread`, `setBusinessThreadInfo`, `deleteMessageRequest`, nhiều `upsertMessage` (tin mới + vài tin cũ từ năm trước), `insertMessage` (tin trả lời tự động của Trang, `senderId` = page), `verifyContactRowExists` (khách và Trang). Đây là lý do có ngưỡng 2 phút cho `upsertMessage`.

## 🚫 Không có / giới hạn

- **Thu hồi**: Messenger chỉ cho khách "gỡ ở phía bạn" với tin gửi Trang, Trang không nhận frame nào.
- `senderName` chỉ có khi trang đã thấy frame contact của khách (tin đầu tiên của khách mới, hoặc khi mở hội thoại). Khách cũ có thể `null`.
- URL ảnh/file là link CDN có chữ ký, hết hạn (`playableUrlExpirationTimestampMs`). Cần tải ngay nếu muốn lưu.
- Định dạng Lightspeed là nội bộ của Meta, có thể đổi không báo trước.

## 🔍 Debug khi script im lặng

1. Mở Console trang Hộp thư, xem có dòng `[BS-INBOX] hooked wss://gateway.facebook.com/ws/lightspeed`. Không có: Meta đổi URL socket, hoặc socket không còn trên trang chính.
2. Tìm socket đang chạy: patch `WebSocket.prototype.send` để lấy `this` (xem hàm `hook` trong script), in `ws.url` của mọi socket.
3. Ghi frame thô của socket Lightspeed vào một mảng, nhờ khách thử gửi một câu dễ nhận ra, rồi tìm câu đó trong frame. Không thấy chữ thô: payload có thể đã bị nén hoặc đổi sang nhị phân.
4. Thấy frame nhưng sự kiện sai: in danh sách `[tên, ...tham số]` (dùng `collectCalls`), so chỉ số với bảng trên và với `messagix/table/*.go` (struct `LS<TênProcedure>`, tag `index:"n"`). mautrix-meta upstream thường cập nhật trước khi mình kịp phát hiện.
5. Lưu frame mới vào `fixtures/`, thay URL bằng `<url>`, thêm test cùng mã TC trong `test-case/` (xem [`test-case/README.md`](test-case/README.md)).

### Gotcha khi điều khiển Chrome bằng berth-chrome (CDP)

- Mỗi lệnh `cdp` là một phiên debugger mới, nên `Page.addScriptToEvaluateOnNewDocument` **mất khi reload**. Không hook được lúc trang tải; phải hook socket đang chạy qua `WebSocket.prototype.send` (app ping vài giây một lần).
- `Runtime.evaluate` có `awaitPromise` chờ quá khoảng 15 giây thì tool báo `i/o timeout`. Chờ ngắn rồi đọc lại.
- Tab chạy nền làm Facebook đóng và mở lại socket nhiều lần. Bộ ghi chỉ gắn một lần sẽ lỡ frame trên socket mới; phải gắn lại ở mỗi `send()`.
- Gỡ một bản script (`__bsInboxStop`) trả `WebSocket.prototype.send` về giá trị trước nó, nên cũng gỡ luôn các wrapper cài sau. Cài lại wrapper ghi frame sau khi nạp bản mới.
- Lấy frame về máy: tạo Blob rồi click `<a download>`, file vào `~/Downloads`. Chrome chặn từ lần tải thứ ba của cùng một trang (hỏi "tải nhiều file").
- Không gửi tin thay khách; cần một tài khoản khác (nên dùng tài khoản thử) nhắn vào Trang.
