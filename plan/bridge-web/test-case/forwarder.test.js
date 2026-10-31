// TC-19..TC-21: phần chuyển tiếp sang fb-bridge-web. Chạy cả script trong node:vm
// như Tampermonkey: window, WebSocket và GM_* giả; frame thật đi qua socket giả.
const { describe, test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const SCRIPT = fs.readFileSync(path.join(__dirname, "..", "bs-inbox-listener.js"), "utf8");
const PAGE_ID = "109282408574091";
const CFG = { url: "http://127.0.0.1:7008/", token: "T".repeat(64), account: "vetgo" };

// Dựng môi trường trang + Tampermonkey. `answer(req)` quyết định phản hồi từng request.
function load({ answer = () => ({ status: 200 }), cfg = CFG } = {}) {
  const requests = [];
  const sockets = [];
  class FakeWS {
    constructor(url) {
      this.url = url;
      this.listeners = [];
      sockets.push(this);
    }
    addEventListener(type, fn) {
      if (type === "message") this.listeners.push(fn);
    }
    send() {}
    receive(data) {
      this.listeners.forEach((fn) => fn({ data }));
    }
  }
  const page = {
    WebSocket: FakeWS,
    dispatchEvent() {},
  };
  const sandbox = {
    unsafeWindow: page,
    window: {},
    location: { search: `?asset_id=${PAGE_ID}` },
    console: { log() {}, warn() {} },
    URLSearchParams,
    TextDecoder,
    CustomEvent: class {
      constructor(type, init) {
        Object.assign(this, { type }, init);
      }
    },
    btoa: (s) => Buffer.from(s, "binary").toString("base64"),
    setTimeout: (fn) => fn(), // bỏ thời gian chờ giữa các lần gửi lại
    setInterval: () => 1,
    clearInterval() {},
    prompt: () => null,
    GM_getValue: () => cfg,
    GM_setValue() {},
    GM_registerMenuCommand() {},
    GM_xmlhttpRequest(opts) {
      const req = { method: opts.method, url: opts.url, headers: opts.headers, body: opts.data && JSON.parse(opts.data) };
      requests.push(req);
      const r = answer(req);
      queueMicrotask(() => (r.status === 0 ? opts.onerror() : opts.onload(r)));
    },
  };
  vm.runInNewContext(SCRIPT, sandbox);
  const ws = new page.WebSocket("wss://gateway.facebook.com/ws/lightspeed?x=1");
  return { requests, ws, page };
}

const frameOf = (step) =>
  "\r\u0003\u0000" + JSON.stringify({ request_id: null, payload: JSON.stringify({ step }) });

// insertMessage + insertBlobAttachment như frame ảnh thật (fixtures/frames-2026-10-31.json).
function imageFrame(mid) {
  const msg = ["insertMessage", null, null, "80", "100024107767037", "0", "1793431618850", "1793431618850", null, mid, "otid", "100024107767037"];
  const blob = new Array(33).fill(null);
  blob[0] = "image-1";
  blob[3] = "https://scontent.example/img.jpg";
  blob[29] = [19, "2"];
  blob[30] = "image/jpeg";
  blob[32] = mid;
  return frameOf([1, [5, ...msg], [5, "insertBlobAttachment", ...blob]]);
}

const settle = () => new Promise((ok) => setTimeout(ok, 20));
const ingests = (requests) => requests.filter((r) => r.url.endsWith("/ingest"));

describe("chuyển tiếp sang fb-bridge-web", () => {
  test("TC-19 heartbeat khi nạp script, đúng token và Trang", async () => {
    const { requests } = load();
    await settle();
    const hb = requests.find((r) => r.url === "http://127.0.0.1:7008/heartbeat");
    assert.ok(hb, "không có heartbeat");
    assert.equal(hb.headers.Authorization, `Bearer ${CFG.token}`);
    assert.equal(hb.body.account, "vetgo");
    assert.equal(hb.body.page_id, PAGE_ID);
  });

  test("TC-20 tin có ảnh: script tải file rồi gửi base64 kèm sự kiện", async () => {
    const { requests, ws } = load({
      answer: (r) => (r.method === "GET" ? { status: 200, response: new Uint8Array([1, 2, 3]).buffer } : { status: 200 }),
    });
    ws.receive(imageFrame("mid.img"));
    await settle();
    assert.ok(requests.some((r) => r.method === "GET" && r.url === "https://scontent.example/img.jpg"));
    const [ing] = ingests(requests);
    assert.equal(ing.body.event.kind, "message");
    assert.equal(ing.body.event.mid, "mid.img");
    assert.equal(ing.body.event.attachments[0].kind, "image");
    assert.equal(ing.body.event.attachments[0].data, "AQID");
  });

  test("TC-20b không tải được file: gửi kèm lý do, không bỏ tin", async () => {
    const { requests, ws } = load({ answer: (r) => (r.method === "GET" ? { status: 403 } : { status: 200 }) });
    ws.receive(imageFrame("mid.403"));
    await settle();
    const [ing] = ingests(requests);
    assert.equal(ing.body.event.attachments[0].error, "HTTP 403");
    assert.equal(ing.body.event.attachments[0].data, undefined);
  });

  test("TC-21 502 hoặc lỗi mạng thì gửi lại, 4xx thì thôi", async () => {
    let n = 0;
    const flaky = load({ answer: (r) => (r.url.endsWith("/ingest") ? { status: [502, 0, 200][n++] } : { status: 200 }) });
    flaky.ws.receive(frameOf([5, "editMessage", "mid.e", [19, "80"], "mới", [19, "1"]]));
    await settle();
    assert.equal(ingests(flaky.requests).length, 3);

    const rejected = load({ answer: (r) => (r.url.endsWith("/ingest") ? { status: 403 } : { status: 200 }) });
    rejected.ws.receive(frameOf([5, "editMessage", "mid.e", [19, "80"], "mới", [19, "1"]]));
    await settle();
    assert.equal(ingests(rejected.requests).length, 1);
  });

  test("chưa cấu hình thì không gửi gì ra ngoài", async () => {
    const { requests, ws } = load({ cfg: null });
    ws.receive(imageFrame("mid.x"));
    await settle();
    assert.deepEqual(requests, []);
  });
});
