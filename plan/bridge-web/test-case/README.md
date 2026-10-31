# Test case - bs-inbox-listener

Hai lớp test cho [`bs-inbox-listener.js`](../bs-inbox-listener.js):

1. **Tự động** (Node, không cần mạng): cho frame thật trong `../fixtures/` chạy qua bộ parse. Chạy sau mỗi lần sửa script.
2. **Trên trang thật**: một tài khoản khách nhắn vào Trang, xem console. Chạy khi nghi Meta đổi định dạng, hoặc trước khi dùng script cho việc thật.

Cùng một mã TC dùng cho cả hai lớp.

## 📂 File

| File | Nội dung |
|---|---|
| `helpers.js` | Nạp fixture, `replay()` chạy các frame như một phiên trang (danh bạ dùng chung, khử trùng giống `emit()`) |
| `messages.test.js` | TC-01..TC-11 trên hội thoại đã có |
| `new-thread.test.js` | TC-08, TC-12, TC-14: khách mới nhắn lần đầu |
| `events.test.js` | TC-11, TC-13, TC-15: thả cảm xúc, sửa tin, gỡ cảm xúc |
| `frame.test.js` | Giải mã frame, lọc tin của Trang (TC-17), ngưỡng `upsertMessage`, khử trùng |
| `forwarder.test.js` | TC-19..TC-21: chạy cả script trong `node:vm` với `window`, WebSocket, `GM_*` giả; frame qua socket giả tới request `/ingest` |
| `live-check.js` | Dán vào Console trang thật, đánh dấu TC đạt |

## 🧪 Chạy test tự động

Từ thư mục gốc repo (cần Node 18 trở lên, đã thử với 22):

```bash
node --test 'plan/bridge-web/test-case/*.test.js'
```

Kết quả mong đợi: `# pass 37`, `# fail 0`. Phải có dấu nháy quanh glob; Node 22 không nhận thư mục làm tham số.

Chạy một file hoặc một TC:

```bash
node --test plan/bridge-web/test-case/events.test.js
node --test --test-name-pattern='TC-13' 'plan/bridge-web/test-case/*.test.js'
```

## 📋 Danh sách test case

"Tự động" là TC đã có frame thật trong fixture. Cột "Mong đợi" là sự kiện `[BS-INBOX]` in ra console.

| TC | Thao tác của khách | Mong đợi | Tự động |
|---|---|---|---|
| TC-01 | Gửi tin chữ | `kind: "message"`, `text` đúng, `attachments: []` | ✅ |
| TC-02 | Gửi 1 ảnh | 1 attachment `image`, `image/jpeg`, có `url` | ✅ |
| TC-03 | Gửi 3 ảnh cùng lúc | 1 sự kiện, 3 attachment `image` | ✅ |
| TC-04 | Gửi video | `video`, `video/mp4`, có `durationMs` | ✅ |
| TC-05 | Ghi âm tin thoại | `audio`, `audio/opus`, có `durationMs` | ✅ |
| TC-06 | Gửi file PDF | `file`, `application/pdf`, `filename`, `subtitle` là dung lượng | ✅ |
| TC-07 | Gửi sticker | `stickerId`, attachment `sticker` có `label` | ✅ |
| TC-08 | Gửi GIF (nút GIF) | `gif`, mime `video/mp4` | ✅ |
| TC-09 | Gửi một URL | `text` có URL, attachment `link` có `title`, `url` | ✅ |
| TC-10 | Trả lời (reply) một tin | `replyTo: { mid, text, senderId }` của tin gốc | ✅ |
| TC-11 | Thả cảm xúc vào một tin | `kind: "reaction"`, `reaction` là emoji, `mid` của tin được thả | ✅ |
| TC-12 | Tài khoản **chưa từng nhắn** Trang gửi tin đầu tiên | Có sự kiện (đến bằng `upsertMessage`), `senderName` có giá trị | ✅ |
| TC-13 | Sửa tin đã gửi (chỉ có trên điện thoại) | `kind: "edit"`, `text` mới, `editCount` | ✅ |
| TC-14 | Bấm nút Like 👍 (điện thoại) | `stickerId: "369239263222822"`, `label: "Like, thumbs up"` | ✅ |
| TC-15 | Gỡ cảm xúc đã thả | `kind: "unreaction"`, `mid`, `actorId` | ✅ |
| TC-16 | Thu hồi tin | **Không có sự kiện**: Messenger chỉ cho khách "gỡ ở phía bạn" với tin gửi Trang | - |
| TC-17 | Trang trả lời khách (Business Suite hoặc trả lời tự động) | **Không có sự kiện** | ✅ |
| TC-18 | Để tab chạy nền 10-30 phút rồi khách nhắn | Vẫn có sự kiện (socket mở lại được bắt lại) | - |
| TC-19 | Tampermonkey đã cấu hình, mở trang | `POST /heartbeat` với đúng token, account, `page_id`; Libredesk: Fanpage Web "Đang chạy" | ✅ (vm) |
| TC-20 | Khách gửi ảnh | Script tải file, `/ingest` có `attachments[].data` base64; tải lỗi thì có `error` | ✅ (vm) |
| TC-21 | Bot trả 502 / lỗi mạng / 4xx | Gửi lại tới 5 lần với 502 và lỗi mạng; 4xx thì thôi | ✅ (vm) |

## 🖐️ Chạy test trên trang thật

Cần: Chrome đã đăng nhập tài khoản quản trị Trang, và **một tài khoản khác** làm khách (nên dùng tài khoản thử). Không gửi tin thay khách từ phía Trang.

1. Mở `https://business.facebook.com/latest/inbox/all/?asset_id=<page id>`.
2. Mở DevTools Console (Cmd+Opt+J), dán `bs-inbox-listener.js`. Chờ thấy dòng `[BS-INBOX] hooked wss://gateway.facebook.com/ws/lightspeed` (vài giây). Nếu cài bằng Tampermonkey thì chỉ cần tải lại trang.
3. Dán `test-case/live-check.js`.
4. Từ tài khoản khách, làm lần lượt các thao tác trong bảng, cách nhau vài giây. TC-13, TC-14 làm trên app điện thoại. TC-12 cần tài khoản chưa từng nhắn Trang.
5. Gọi `__bsCheck()` để xem bảng đạt/chưa. `__bsCheck.events` là toàn bộ sự kiện, `__bsCheck.reset()` để chạy lại.
6. TC-16, TC-17, TC-18 xem tay: thu hồi và Trang trả lời thì console **không** in gì; tab nền lâu vẫn in tin mới.

Để tab ở phía trước trong lúc test. Tab nền làm Facebook mở lại socket thường xuyên hơn; script chịu được (TC-18) nhưng dễ làm rối khi đối chiếu.

## ❌ Khi một TC hỏng

1. Ghi lại frame của thao tác đó: các bước trong mục "Debug khi script im lặng" của [`../README.md`](../README.md).
2. Lưu vào `../fixtures/` với tên `frames-<ngày>-<mô tả>.json`. Thay URL (`https://`, `/messaging/`, `ls://`) bằng `<url>`. Mỗi phần tử: `{ "t": <ms lúc nhận>, "calls": [["tênProcedure", ...tham số đã giải mã]] }`.
3. Sửa chỉ số trong `parseCalls()` theo frame mới (đối chiếu `bridge-e2ee/meta/pkg/messagix/table/*.go`).
4. Thêm hoặc sửa test cùng mã TC, chạy lại cả bộ.
5. Cập nhật bảng procedure trong `../README.md`.

## ➕ Thêm test case mới

- Thao tác mới (ví dụ vị trí, nút bấm postback): đặt mã TC tiếp theo, bắt frame thật, thêm fixture và test, thêm dòng vào bảng trên và vào `CASES` trong `live-check.js`.
- Test chỉ cần logic (không cần frame thật) đặt trong `frame.test.js`, dựng lời gọi tối thiểu như `insertMessage()` trong file đó.
