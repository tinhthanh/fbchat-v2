// Dán vào DevTools Console của trang Hộp thư SAU khi đã nạp bs-inbox-listener.js.
// Ghi lại mọi sự kiện [BS-INBOX] và đánh dấu test case nào đã đạt.
//   __bsCheck()        bảng TC: đạt / chưa, kèm mid làm bằng chứng
//   __bsCheck.events   toàn bộ sự kiện đã nhận
//   __bsCheck.reset()  xoá để chạy lại
// TC-12 (khách mới) và TC-16, TC-18 cần xem tay, xem test-case/README.md.
(() => {
  const LIKE_STICKER = "369239263222822";
  const kinds = (e) => (e.attachments || []).map((a) => a.kind);

  // Mỗi TC: mô tả và hàm nhận một sự kiện, trả true nếu sự kiện đó chứng minh TC đạt.
  const CASES = {
    "TC-01": ["Tin chữ", (e) => e.kind === "message" && e.text && !kinds(e).length && !e.replyTo],
    "TC-02": ["Một ảnh", (e) => kinds(e).join() === "image"],
    "TC-03": ["Nhiều ảnh", (e) => kinds(e).length > 1 && kinds(e).every((k) => k === "image")],
    "TC-04": ["Video", (e) => kinds(e).includes("video")],
    "TC-05": ["Tin thoại", (e) => kinds(e).includes("audio")],
    "TC-06": ["File", (e) => kinds(e).includes("file")],
    "TC-07": ["Sticker", (e) => kinds(e).includes("sticker") && e.stickerId !== LIKE_STICKER],
    "TC-08": ["GIF", (e) => kinds(e).includes("gif")],
    "TC-09": ["Link", (e) => kinds(e).includes("link")],
    "TC-10": ["Trả lời", (e) => e.kind === "message" && !!e.replyTo],
    "TC-11": ["Thả cảm xúc", (e) => e.kind === "reaction"],
    "TC-12": ["Khách mới có tên", (e) => e.kind === "message" && !!e.senderName],
    "TC-13": ["Sửa tin", (e) => e.kind === "edit"],
    "TC-14": ["Like 👍", (e) => e.stickerId === LIKE_STICKER],
    "TC-15": ["Gỡ cảm xúc", (e) => e.kind === "unreaction"],
  };

  const events = [];
  const onEvent = (ev) => events.push(ev.detail);
  if (window.__bsCheck) window.removeEventListener("bs-inbox-message", window.__bsCheck.onEvent);
  window.addEventListener("bs-inbox-message", onEvent);

  function check() {
    const rows = {};
    for (const [id, [label, match]] of Object.entries(CASES)) {
      const hit = events.find(match);
      rows[id] = { "Mô tả": label, "Kết quả": hit ? "✅" : "⏳", Mid: hit ? hit.mid : "" };
    }
    console.table(rows);
    const done = Object.values(rows).filter((r) => r["Kết quả"] === "✅").length;
    return `${done}/${Object.keys(rows).length} TC đạt, ${events.length} sự kiện`;
  }
  check.events = events;
  check.onEvent = onEvent;
  check.reset = () => events.splice(0);
  window.__bsCheck = check;
  console.log("[BS-CHECK] đang ghi; gọi __bsCheck() để xem bảng");
})();
