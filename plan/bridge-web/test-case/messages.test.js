// TC-01..TC-11: tin của khách trong hội thoại đã có (fixtures/frames-2026-10-31.json).
const { describe, test } = require("node:test");
const assert = require("node:assert/strict");
const { loadFixture, replay, messageByMid } = require("./helpers.js");

const events = replay(loadFixture("frames-2026-10-31.json"));
const msg = (mid) => messageByMid(events, mid);

describe("tin trong hội thoại đã có", () => {
  test("TC-01 tin chữ", () => {
    const m = msg("mid.$cAAA5nOZNOHanwiaX5WhkOFOkWzUU");
    assert.equal(m.text, "sua-tin");
    assert.equal(m.threadId, "100024107767037");
    assert.equal(m.senderId, "100024107767037");
    assert.deepEqual(m.attachments, []);
  });

  test("TC-02 một ảnh", () => {
    const [a] = msg("mid.$cAAA5nOZNOHanwiANImhkNrBJn_Ua").attachments;
    assert.deepEqual(
      [a.kind, a.mimeType, a.filename, a.size],
      ["image", "image/jpeg", "image-29564380916583879", 195964],
    );
  });

  test("TC-03 nhiều ảnh trong một tin", () => {
    const list = msg("mid.$cAAA5nOZNOHanwiEH2GhkNu2kjWGV").attachments;
    assert.equal(list.length, 3);
    assert.ok(list.every((a) => a.kind === "image"));
  });

  test("TC-04 video", () => {
    const [a] = msg("mid.$cAAA5nOZNOHanwiHSX2hkNyHOghzr").attachments;
    assert.deepEqual([a.kind, a.mimeType, a.durationMs], ["video", "video/mp4", 10000]);
  });

  test("TC-05 tin thoại", () => {
    const [a] = msg("mid.$cAAA5nOZNOHanwiJYpGhkN0FgEOam").attachments;
    assert.deepEqual([a.kind, a.mimeType, a.durationMs], ["audio", "audio/opus", 8600]);
  });

  test("TC-06 file PDF", () => {
    const [a] = msg("mid.$cAAA5nOZNOHanwiP2hWhkN6o6DZPt").attachments;
    assert.deepEqual(
      [a.kind, a.mimeType, a.filename, a.subtitle],
      ["file", "application/pdf", "Infectious_Diseases_of_the_Dog_and_Cat.pdf", "172 KiB"],
    );
  });

  test("TC-07 sticker", () => {
    const m = msg("mid.$cAAA5nOZNOHanwiRT3mhkN8KO6NE5");
    assert.equal(m.stickerId, "1148798091806176");
    assert.deepEqual(
      m.attachments.map((a) => [a.kind, a.stickerId, a.label]),
      [["sticker", "1148798091806176", "Realistic portrait of Superman"]],
    );
  });

  test("TC-09 link", () => {
    const m = msg("mid.$cAAA5nOZNOHanwiWaNGhkOBLAOs2g");
    assert.match(m.text, /https:\/\/vetgo\.tech/);
    const [a] = m.attachments;
    assert.deepEqual([a.kind, a.title], ["link", "vetgo.tech"]);
    assert.equal(a.url, "<url>"); // URL thật lấy từ insertAttachmentCta, fixture đã che
  });

  test("TC-10 trả lời tin của khách", () => {
    assert.deepEqual(msg("mid.$cAAA5nOZNOHanwiXwK2hkOCmp3Jg0").replyTo, {
      mid: "mid.$cAAA5nOZNOHanweGTkWhkJxJKi2ue",
      text: "hihi",
      senderId: "100024107767037",
    });
  });

  test("TC-10b trả lời tin của Trang", () => {
    const { replyTo } = msg("mid.$cAAA5nOZNOHanwiYLvGhkODCMG3jR");
    assert.equal(replyTo.senderId, "109282408574091");
  });

  test("TC-11 thả cảm xúc", () => {
    assert.deepEqual(
      events.filter((e) => e.kind === "reaction").map((e) => [e.mid, e.reaction, e.actorId]),
      [["mid.$cAAA5nOZNOHanwL-HL2hj3o9Xg7q3", "❤", "100024107767037"]],
    );
  });

  test("lịch sử tải lại khi mở hội thoại (upsertMessage cũ) không phải tin mới", () => {
    assert.equal(msg("mid.$cAAA5ktf3J8mnnkW5qGhbQBuKjTXa"), undefined);
  });

  test("frame gửi lặp chỉ ra một sự kiện", () => {
    const mids = events.filter((e) => e.kind === "message").map((e) => e.mid);
    assert.equal(new Set(mids).size, mids.length);
  });
});
