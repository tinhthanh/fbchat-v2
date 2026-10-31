// Dùng chung cho các file *.test.js: nạp fixture và chạy parse như script trong trang.
const path = require("node:path");
const listener = require("../bs-inbox-listener.js");

const PAGE_ID = "109282408574091";

function loadFixture(name) {
  return require(path.join(__dirname, "..", "fixtures", name));
}

// Chạy mọi frame của fixture, mỗi frame lấy "hiện tại" là lúc frame đến (f.t),
// danh bạ dùng chung như một phiên trang thật. Khử trùng giống emit() trong trang.
function replay(frames, { pageId = PAGE_ID } = {}) {
  const contacts = new Map();
  const seen = new Set();
  const events = [];
  for (const f of frames) {
    for (const e of listener.parseCalls(f.calls, pageId, { nowMs: f.t, contacts })) {
      const key = listener.eventKey(e);
      if (seen.has(key)) continue;
      seen.add(key);
      events.push(e);
    }
  }
  return events;
}

const messageByMid = (events, mid) => events.find((e) => e.kind === "message" && e.mid === mid);

module.exports = { PAGE_ID, loadFixture, replay, messageByMid, ...listener };
