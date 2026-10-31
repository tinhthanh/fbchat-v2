// Service worker: tải file đính kèm và gửi sự kiện sang fb-bridge-web. Fetch của
// extension không bị CSP của business.facebook.com chặn và mang cookie của trình
// duyệt tới các host trong host_permissions. config.json do entrypoint của image
// ghi lúc khởi động từ biến môi trường (không nằm trong image).

const MAX_FILE = 20 * 1024 * 1024;
const RETRY_S = [2, 5, 15, 30, 60];
const LIKE_STICKER = "369239263222822";

let configPromise = null;
const config = () =>
  (configPromise ||= fetch(chrome.runtime.getURL("config.json"))
    .then((r) => r.json())
    .catch(() => null));

async function post(path, meta, body) {
  const c = await config();
  if (!c || !c.url || !c.token || !c.account) return 0;
  try {
    const r = await fetch(c.url.replace(/\/+$/, "") + path, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${c.token}` },
      body: JSON.stringify({ account: c.account, page_id: meta.pageId, page_name: meta.pageName, version: "ext-1", ...body }),
    });
    return r.status;
  } catch {
    return 0;
  }
}

function toBase64(buf) {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(bin);
}

// Tải file ngay: URL CDN có chữ ký và hết hạn, server không có cookie.
async function withFiles(event) {
  if (event.kind !== "message" || !event.attachments || !event.attachments.length) return event;
  const attachments = [];
  for (const a of event.attachments) {
    if (a.kind === "link" || (a.kind === "sticker" && a.stickerId === LIKE_STICKER) || !a.url) {
      attachments.push(a);
      continue;
    }
    try {
      const r = await fetch(a.url, { credentials: "include" });
      if (!r.ok) {
        attachments.push({ ...a, error: `HTTP ${r.status}` });
        continue;
      }
      const buf = await r.arrayBuffer();
      if (buf.byteLength > MAX_FILE) attachments.push({ ...a, error: "file quá 20 MiB" });
      else attachments.push({ ...a, data: toBase64(buf) });
    } catch {
      attachments.push({ ...a, error: "lỗi mạng" });
    }
  }
  return { ...event, attachments };
}

// Hàng đợi tuần tự giữ thứ tự tin; 502 hoặc lỗi mạng thì gửi lại (bot và gateway
// chống trùng theo id), 4xx thì bỏ.
const queue = [];
let busy = false;
async function drain() {
  if (busy) return;
  busy = true;
  while (queue.length) {
    const msg = queue[0];
    const event = await withFiles(msg.event);
    let status = 0;
    for (let i = 0; i <= RETRY_S.length; i++) {
      status = await post("/ingest", msg, { event });
      if (status >= 200 && status < 500) break;
      if (i < RETRY_S.length) await new Promise((ok) => setTimeout(ok, RETRY_S[i] * 1000));
    }
    console.log("fb-bridge-web ingest", status, event.kind, event.mid);
    queue.shift();
  }
  busy = false;
}

chrome.runtime.onMessage.addListener((msg) => {
  if (msg.__bsInbox === "hello") post("/heartbeat", msg, {});
  else if (msg.__bsInbox === "event" && msg.event) {
    console.log("fb-bridge-web queued", msg.event.kind, msg.event.mid, "queue", queue.length + 1);
    queue.push(msg);
    drain();
  }
});
