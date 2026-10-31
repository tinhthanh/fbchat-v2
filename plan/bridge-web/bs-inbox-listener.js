// ==UserScript==
// @name         fb-bridge-web (Business Suite inbox -> omnichannel)
// @match        https://business.facebook.com/latest/inbox/*
// @run-at       document-start
// @sandbox      JavaScript
// @grant        unsafeWindow
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_registerMenuCommand
// @connect      127.0.0.1
// @connect      localhost
// @connect      fbcdn.net
// @connect      fbsbx.com
// @connect      facebook.com
// ==/UserScript==
//
// Nghe tin nhắn Fanpage ngay trên socket wss://gateway.facebook.com/ws/lightspeed
// của Business Suite. Mỗi sự kiện của khách in ra console "[BS-INBOX]", phát
// sự kiện window "bs-inbox-message" và (Tampermonkey, đã cấu hình) chuyển sang
// fb-bridge-web của omnichannel kèm file đã tải. Dữ liệu sự kiện:
//   { kind: "message", mid, threadId, senderId, senderName, text, timestampMs, stickerId,
//     replyTo: { mid, text, senderId } | null, attachments: [...] }
//   { kind: "reaction", mid, threadId, actorId, reaction, timestampMs }
//   { kind: "unreaction", mid, threadId, actorId }
//   { kind: "edit", mid, text, editCount }
// attachments[i].kind: "image" | "video" | "audio" | "file" | "gif" | "sticker" | "link"
//
// Cách nạp:
//   - Tampermonkey (@run-at document-start): bắt socket ngay từ đầu.
//   - Dán vào DevTools Console khi trang đã mở: bắt socket đang chạy ở lần
//     ws.send() kế tiếp (ping, vài giây là có).
// Dừng: window.__bsInboxStop()
//
// Thứ tự tham số của stored procedure Lightspeed lấy từ mautrix-meta
// (bridge-e2ee/meta/pkg/messagix/table) và đã đối chiếu với frame thật trong
// fixtures/frames-2026-10-31.json.
(() => {
  // AttachmentType của Lightspeed (messagix/table/enums.go).
  const BLOB_KINDS = { 1: "sticker", 2: "image", 3: "gif", 4: "video", 5: "audio", 6: "file" };

  // [19,"123"] là int64, [9] là null; còn lại giữ nguyên.
  const val = (v) => {
    if (Array.isArray(v) && v[0] === 19) return v[1];
    if (Array.isArray(v) && v[0] === 9) return null;
    return v;
  };

  // Gom mọi lời gọi [5, "tênProcedure", ...tham số] thành [tên, ...tham số].
  function collectCalls(node, out) {
    if (!Array.isArray(node)) return out;
    if (node[0] === 5 && typeof node[1] === "string") {
      out.push([node[1], ...node.slice(2).map(val)]);
      return out;
    }
    for (const child of node) collectCalls(child, out);
    return out;
  }

  function decodeFrame(data) {
    const text =
      typeof data === "string" ? data : new TextDecoder().decode(new Uint8Array(data));
    const start = text.indexOf("{");
    const end = text.lastIndexOf("}");
    if (start < 0 || end <= start) return null;
    try {
      const envelope = JSON.parse(text.slice(start, end + 1));
      if (typeof envelope.payload !== "string") return null;
      return collectCalls(JSON.parse(envelope.payload).step, []);
    } catch {
      return null;
    }
  }

  // upsertMessage vừa là tin đầu tiên của khách mới (đi cùng updateOrInsertThread),
  // vừa là lịch sử tải lại khi mở hội thoại; chỉ coi là tin mới nếu đủ gần hiện tại.
  const UPSERT_FRESH_MS = 2 * 60 * 1000;

  // Biến danh sách lời gọi của một frame thành sự kiện. `contacts` (Map id -> tên)
  // được cập nhật từ frame và dùng để điền senderName.
  function parseCalls(calls, pageId, { nowMs = Date.now(), contacts = new Map() } = {}) {
    const attachments = {};
    const ctaUrls = {};
    const add = (mid, item) => (attachments[mid] ||= []).push(item);

    for (const [name, ...a] of calls) {
      switch (name) {
        case "verifyContactRowExists":
          if (a[0] && a[3]) contacts.set(a[0], a[3]);
          break;
        case "deleteThenInsertContact":
          if (a[0] && a[9]) contacts.set(a[0], a[9]);
          break;
        case "insertBlobAttachment":
          add(a[32], {
            kind: BLOB_KINDS[a[29]] || "file",
            mimeType: a[30] || a[6],
            filename: a[0],
            size: a[1] ? Number(a[1]) : null,
            url: a[3],
            previewUrl: a[8],
            durationMs: a[22] ? Number(a[22]) : null,
          });
          break;
        case "insertXmaAttachment":
          // File dạng tài liệu có playableUrl; link chia sẻ thì không (URL nằm ở CTA).
          add(a[30], {
            kind: a[4] ? "file" : "link",
            id: a[32],
            mimeType: a[7] || null,
            filename: a[4] ? a[58] : null,
            title: a[58],
            subtitle: a[59],
            url: a[4] || null,
          });
          break;
        case "insertAttachmentCta":
          if (a[7] === "xma_web_url" && a[9]) ctaUrls[a[1]] = a[9];
          break;
        case "insertStickerAttachment":
          add(a[18], {
            kind: "sticker",
            stickerId: a[19],
            label: a[13],
            url: a[4],
          });
          break;
      }
    }
    for (const list of Object.values(attachments)) {
      for (const item of list) if (item.kind === "link") item.url = ctaUrls[item.id] || null;
    }

    const events = [];
    for (const [name, ...a] of calls) {
      if (name === "insertMessage" || name === "upsertMessage") {
        if (pageId && a[10] === pageId) continue; // tin Trang tự gửi
        if (name === "upsertMessage" && nowMs - Number(a[5]) > UPSERT_FRESH_MS) continue;
        events.push({
          kind: "message",
          mid: a[8],
          threadId: a[3],
          senderId: a[10],
          senderName: contacts.get(a[10]) || null,
          text: a[0] || "",
          timestampMs: Number(a[5]),
          stickerId: a[11],
          replyTo: a[23] ? { mid: a[23], text: a[28], senderId: a[29] } : null,
          attachments: attachments[a[8]] || [],
        });
      } else if (name === "upsertReaction") {
        if (pageId && a[3] === pageId) continue;
        events.push({
          kind: "reaction",
          mid: a[2],
          threadId: a[0],
          actorId: a[3],
          reaction: a[4],
          // a[1] là thời điểm của tin được thả; a[6] (nếu có) là lúc thả cảm xúc.
          timestampMs: Number(a[6] || a[1]),
        });
      } else if (name === "deleteReaction") {
        if (pageId && a[2] === pageId) continue;
        events.push({ kind: "unreaction", mid: a[1], threadId: a[0], actorId: a[2] });
      } else if (name === "editMessage") {
        events.push({ kind: "edit", mid: a[0], text: a[2], editCount: Number(a[3]) });
      }
    }
    return events;
  }

  // Facebook gửi lặp cùng một sự kiện vài lần; mỗi khoá chỉ phát một lần.
  const eventKey = (e) =>
    e.kind === "message" ? e.mid
    : e.kind === "edit" ? `edit:${e.mid}:${e.editCount}`
    : `${e.kind}:${e.mid}:${e.actorId}:${e.reaction || ""}:${e.timestampMs || ""}`;

  // Node: chỉ xuất hàm parse để test.
  if (typeof module !== "undefined" && module.exports) {
    module.exports = { collectCalls, decodeFrame, parseCalls, eventKey, UPSERT_FRESH_MS };
    return;
  }

  // Tampermonkey chạy script trong sandbox: socket và sự kiện nằm ở window của trang.
  const W = typeof unsafeWindow !== "undefined" ? unsafeWindow : window;
  if (W.__bsInboxStop) W.__bsInboxStop();

  const PAGE_ID = new URLSearchParams(location.search).get("asset_id") || "";
  const contacts = new Map();
  const seen = new Set();
  const hooked = new WeakSet();
  let stopped = false;

  const forwarder = createForwarder();

  function emit(event) {
    const key = eventKey(event);
    if (seen.has(key)) return;
    seen.add(key);
    if (seen.size > 2000) seen.delete(seen.values().next().value);
    console.log("[BS-INBOX]", event);
    W.dispatchEvent(new CustomEvent("bs-inbox-message", { detail: event }));
    if (forwarder) forwarder.push(event);
    else post("event", { event });
  }

  // Không có Tampermonkey (extension của image fbweb-browser): gửi qua postMessage
  // cho content script ở isolated world (structured clone đi được giữa hai world).
  function post(kind, extra) {
    W.postMessage({ __bsInbox: kind, pageId: PAGE_ID, pageName: contacts.get(PAGE_ID) || "", ...extra }, location.origin);
  }
  // Nhịp sống cho extension: chỉ phát khi trang còn chạy, extension đổi thành heartbeat.
  let helloTimer = null;
  if (!forwarder) {
    post("hello", {});
    helloTimer = setInterval(() => post("hello", {}), 30000);
  }

  // Chuyển sự kiện sang fb-bridge-web (omnichannel). Chỉ có khi chạy bằng
  // Tampermonkey: GM_xmlhttpRequest vượt được CSP của business.facebook.com và
  // tải file CDN bằng cookie của trình duyệt. Cấu hình lưu trong Tampermonkey
  // (menu "Cấu hình fb-bridge-web"), không nằm trong file này.
  function createForwarder() {
    if (typeof GM_xmlhttpRequest !== "function" || typeof GM_getValue !== "function") return null;
    const VERSION = "1";
    const MAX_FILE = 20 * 1024 * 1024;
    const RETRY_S = [2, 5, 15, 30, 60];
    const cfg = () => GM_getValue("fbweb", null);

    GM_registerMenuCommand("Cấu hình fb-bridge-web", () => {
      const cur = cfg() || { url: "http://127.0.0.1:7008", token: "", account: "" };
      const url = prompt("Địa chỉ cổng ingest (SSH tunnel tới server)", cur.url);
      if (url === null) return;
      const account = prompt(`Tên tài khoản trong FBWEB_PAGES cho Trang ${PAGE_ID}`, cur.account);
      if (account === null) return;
      const token = prompt("FBWEB_INGEST_TOKEN (để trống: giữ token cũ)", "");
      if (token === null) return;
      GM_setValue("fbweb", { url: url.trim(), account: account.trim(), token: token.trim() || cur.token });
      console.log("[BS-INBOX] fb-bridge-web configured", { url: url.trim(), account: account.trim() });
      heartbeat();
    });

    const request = (opts) =>
      new Promise((resolve) => {
        GM_xmlhttpRequest({
          timeout: 120000,
          ...opts,
          onload: (r) => resolve({ status: r.status, response: r.response }),
          onerror: () => resolve({ status: 0 }),
          ontimeout: () => resolve({ status: 0 }),
        });
      });

    const configured = () => {
      const c = cfg();
      return !!(c && c.url && c.token && c.account);
    };

    const post = (path, body) => {
      const c = cfg();
      if (!configured()) return Promise.resolve({ status: -1 });
      return request({
        method: "POST",
        url: c.url.replace(/\/+$/, "") + path,
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${c.token}` },
        data: JSON.stringify({ account: c.account, page_id: PAGE_ID, page_name: contacts.get(PAGE_ID) || "", version: VERSION, ...body }),
      });
    };

    const toBase64 = (buf) => {
      const bytes = new Uint8Array(buf);
      let bin = "";
      for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
      return btoa(bin);
    };

    // Tải file ngay: URL CDN có chữ ký và hết hạn, server không có cookie.
    async function withFiles(event) {
      if (event.kind !== "message" || !event.attachments.length) return event;
      const attachments = [];
      for (const a of event.attachments) {
        if (a.kind === "link" || (a.kind === "sticker" && a.stickerId === "369239263222822") || !a.url) {
          attachments.push(a);
          continue;
        }
        const r = await request({ method: "GET", url: a.url, responseType: "arraybuffer" });
        if (r.status !== 200 || !r.response) attachments.push({ ...a, error: `HTTP ${r.status || "lỗi mạng"}` });
        else if (r.response.byteLength > MAX_FILE) attachments.push({ ...a, error: "file quá 20 MiB" });
        else attachments.push({ ...a, data: toBase64(r.response) });
      }
      return { ...event, attachments };
    }

    // Hàng đợi tuần tự: giữ thứ tự tin; 502 hoặc lỗi mạng thì gửi lại (bot và
    // gateway chống trùng theo id nên gửi lại an toàn), 4xx thì bỏ và ghi log.
    const queue = [];
    let busy = false;
    async function drain() {
      if (busy) return;
      busy = true;
      while (queue.length) {
        if (!configured()) {
          console.warn("[BS-INBOX] fb-bridge-web chưa cấu hình (menu Tampermonkey), bỏ", queue.shift().kind);
          continue;
        }
        const event = await withFiles(queue[0]);
        let r = { status: 0 };
        for (let i = 0; i <= RETRY_S.length; i++) {
          r = await post("/ingest", { event });
          if (r.status === -1 || (r.status >= 200 && r.status < 500)) break;
          if (i < RETRY_S.length) await new Promise((ok) => setTimeout(ok, RETRY_S[i] * 1000));
        }
        if (r.status === -1) console.warn("[BS-INBOX] fb-bridge-web chưa cấu hình (menu Tampermonkey)");
        else if (r.status !== 200) console.warn("[BS-INBOX] fb-bridge-web từ chối", r.status, event.kind, event.mid);
        queue.shift();
      }
      busy = false;
    }

    async function heartbeat() {
      const r = await post("/heartbeat", {});
      if (r.status > 0 && r.status !== 200) console.warn("[BS-INBOX] heartbeat", r.status);
    }
    const timer = setInterval(heartbeat, 30000);
    heartbeat();

    return {
      push(event) {
        queue.push(event);
        drain();
      },
      stop() {
        clearInterval(timer);
      },
    };
  }

  function hook(ws) {
    if (hooked.has(ws) || !String(ws.url).includes("/ws/lightspeed")) return;
    hooked.add(ws);
    ws.addEventListener("message", (e) => {
      if (stopped) return;
      const calls = decodeFrame(e.data);
      if (calls && calls.length) parseCalls(calls, PAGE_ID, { contacts }).forEach(emit);
    });
    console.log("[BS-INBOX] hooked", String(ws.url).split("?")[0]);
  }

  // Socket tạo sau khi script chạy (document-start).
  const NativeWS = W.WebSocket;
  W.WebSocket = new Proxy(NativeWS, {
    construct(target, args) {
      const ws = Reflect.construct(target, args);
      hook(ws);
      return ws;
    },
  });
  // Socket đã mở trước đó: bắt ở lần send() tiếp theo.
  const nativeSend = NativeWS.prototype.send;
  NativeWS.prototype.send = function (...args) {
    hook(this);
    return nativeSend.apply(this, args);
  };

  W.__bsInboxStop = () => {
    stopped = true;
    if (forwarder) forwarder.stop();
    clearInterval(helloTimer);
    W.WebSocket = NativeWS;
    NativeWS.prototype.send = nativeSend;
    delete W.__bsInboxStop;
    console.log("[BS-INBOX] stopped");
  };
  console.log(`[BS-INBOX] listening, page ${PAGE_ID || "?"}, forward ${forwarder ? "on" : "off"}`);
})();
