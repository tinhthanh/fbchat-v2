// TC-08, TC-12, TC-14: khách mới nhắn lần đầu bằng Like (fixtures/frames-2026-10-31-newthread.json).
const { describe, test } = require("node:test");
const assert = require("node:assert/strict");
const { loadFixture, replay, PAGE_ID } = require("./helpers.js");

const events = replay(loadFixture("frames-2026-10-31-newthread.json"));

describe("khách mới", () => {
  test("TC-12 + TC-14 tin đầu tiên (Like) đến bằng upsertMessage, có tên khách", () => {
    const [first] = events;
    assert.equal(first.kind, "message");
    assert.equal(first.senderName, "Huỳnh Tính Thành");
    assert.equal(first.stickerId, "369239263222822");
    assert.deepEqual(
      first.attachments.map((a) => [a.kind, a.label]),
      [["sticker", "Like, thumbs up"]],
    );
  });

  test("TC-08 GIF là blob type 3, mime video/mp4", () => {
    const gif = events.find((e) => e.attachments.some((a) => a.kind === "gif"));
    assert.ok(gif);
    assert.equal(gif.attachments[0].mimeType, "video/mp4");
  });

  test("tên khách giữ cho các tin sau", () => {
    assert.ok(events.every((e) => e.senderName === "Huỳnh Tính Thành"));
  });

  test("tin cũ từ năm trước và tin trả lời tự động của Trang bị bỏ qua", () => {
    assert.deepEqual(
      events.map((e) => e.attachments.map((a) => a.kind).join() || "text"),
      ["sticker", "audio", "video", "gif"],
    );
    assert.ok(events.every((e) => e.senderId !== PAGE_ID));
  });
});
