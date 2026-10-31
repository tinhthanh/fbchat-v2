// TC-11, TC-13, TC-15: thả cảm xúc, sửa tin, gỡ cảm xúc (fixtures/frames-2026-10-31-edit.json).
const { describe, test } = require("node:test");
const assert = require("node:assert/strict");
const { loadFixture, replay } = require("./helpers.js");

const MID = "mid.$cAAA5kHeMT52nwlJIPmhkQz-pseO6";
const events = replay(loadFixture("frames-2026-10-31-edit.json"));
const byKind = (kind) => events.find((e) => e.kind === kind);

describe("sự kiện trên tin đã có", () => {
  test("TC-13 sửa tin", () => {
    assert.deepEqual(byKind("edit"), {
      kind: "edit",
      mid: MID,
      text: "Tin nhắn gửi, đã chỉnh sửa",
      editCount: 1,
    });
  });

  test("TC-11 thả 👍, timestampMs là lúc thả (tham số 6)", () => {
    assert.deepEqual(byKind("reaction"), {
      kind: "reaction",
      mid: MID,
      threadId: "100005036221206",
      actorId: "100005036221206",
      reaction: "👍",
      timestampMs: 1793434954603,
    });
  });

  test("TC-15 gỡ cảm xúc", () => {
    assert.deepEqual(byKind("unreaction"), {
      kind: "unreaction",
      mid: MID,
      threadId: "100005036221206",
      actorId: "100005036221206",
    });
  });
});
