// Giải mã frame và các quy tắc lọc, không cần fixture.
const { describe, test } = require("node:test");
const assert = require("node:assert/strict");
const { decodeFrame, collectCalls, parseCalls, eventKey, UPSERT_FRESH_MS, PAGE_ID } =
  require("./helpers.js");

// Frame như trên socket: header nhị phân + envelope JSON có payload là chuỗi JSON.
const frameOf = (step) =>
  "\r\u0003\u0000 " + JSON.stringify({ request_id: null, payload: JSON.stringify({ step }) });

// insertMessage tối thiểu: 0 text, 3 thread, 5 ts, 8 mid, 10 sender.
const insertMessage = (proc, { text = "x", ts = 1000, mid = "mid.a", sender = "42" } = {}) => [
  proc, text, null, "80", "42", "0", String(ts), String(ts), null, mid, "otid", sender,
];

describe("decodeFrame", () => {
  test("bỏ header, giải mã int64 [19,n] và null [9]", () => {
    const step = [1, [5, "editMessage", "mid.x", [19, "80"], "mới", [19, "2"]], [5, "noop", [9]]];
    assert.deepEqual(decodeFrame(frameOf(step)), [
      ["editMessage", "mid.x", "80", "mới", "2"],
      ["noop", null],
    ]);
  });

  test("nhận ArrayBuffer như WebSocket binaryType=arraybuffer", () => {
    const buf = new TextEncoder().encode(frameOf([5, "deleteReaction", "t", "m", "a"])).buffer;
    assert.deepEqual(decodeFrame(buf), [["deleteReaction", "t", "m", "a"]]);
  });

  test("frame không phải Lightspeed trả null", () => {
    assert.equal(decodeFrame("\u0000\u0001ping"), null);
    assert.equal(decodeFrame('{"request_id":1}'), null);
    assert.equal(decodeFrame('{"payload":"không phải json"}'), null);
  });

  test("collectCalls đi sâu qua block lồng nhau", () => {
    const tree = [1, [23, [2, 0], [1, [1, [5, "a"]], [26, [5, "b", 1]]]]];
    assert.deepEqual(collectCalls(tree, []), [["a"], ["b", 1]]);
  });
});

describe("quy tắc lọc", () => {
  test("TC-17 tin Trang tự gửi bị bỏ qua", () => {
    assert.deepEqual(parseCalls([insertMessage("insertMessage", { sender: PAGE_ID })], PAGE_ID), []);
  });

  test("cảm xúc của Trang bị bỏ qua", () => {
    const calls = [
      ["upsertReaction", "t", "1", "mid.a", PAGE_ID, "❤", "80"],
      ["deleteReaction", "t", "mid.a", PAGE_ID],
    ];
    assert.deepEqual(parseCalls(calls, PAGE_ID), []);
  });

  test(`upsertMessage chỉ là tin mới khi chưa quá ${UPSERT_FRESH_MS / 1000} giây`, () => {
    const ts = 10_000_000;
    const call = [insertMessage("upsertMessage", { ts })];
    assert.equal(parseCalls(call, PAGE_ID, { nowMs: ts + UPSERT_FRESH_MS }).length, 1);
    assert.equal(parseCalls(call, PAGE_ID, { nowMs: ts + UPSERT_FRESH_MS + 1 }).length, 0);
  });

  test("insertMessage luôn là tin mới, kể cả tin cũ", () => {
    const call = [insertMessage("insertMessage", { ts: 1 })];
    assert.equal(parseCalls(call, PAGE_ID, { nowMs: 1e13 }).length, 1);
  });

  test("tên khách lấy từ verifyContactRowExists hoặc deleteThenInsertContact", () => {
    const contacts = new Map();
    const verify = ["verifyContactRowExists", "42", "1", "<url>", "Khách A"];
    const insert = ["deleteThenInsertContact", "43", null, "", "", "", "", "", "", "", "Khách B"];
    parseCalls([verify, insert], PAGE_ID, { contacts });
    assert.deepEqual([...contacts], [["42", "Khách A"], ["43", "Khách B"]]);
  });

  test("procedure lạ không làm hỏng parse", () => {
    assert.deepEqual(parseCalls([["somethingNew", 1, 2], ["bumpThread"]], PAGE_ID), []);
  });
});

describe("eventKey (khử trùng)", () => {
  test("tin cùng mid trùng nhau, sửa lần khác nhau thì khác", () => {
    assert.equal(eventKey({ kind: "message", mid: "m" }), eventKey({ kind: "message", mid: "m" }));
    assert.notEqual(
      eventKey({ kind: "edit", mid: "m", editCount: 1 }),
      eventKey({ kind: "edit", mid: "m", editCount: 2 }),
    );
  });

  test("thả lại cùng emoji lúc khác vẫn là sự kiện mới", () => {
    const r = { kind: "reaction", mid: "m", actorId: "a", reaction: "❤" };
    assert.notEqual(eventKey({ ...r, timestampMs: 1 }), eventKey({ ...r, timestampMs: 2 }));
  });
});
