// Isolated world: nhận postMessage của bs-inbox-listener.js (world MAIN) và chuyển
// cho service worker. Trang không gọi được chrome.runtime, nên cần lớp này.
window.addEventListener("message", (e) => {
  if (e.source !== window || !e.data || typeof e.data.__bsInbox !== "string") return;
  chrome.runtime.sendMessage(e.data).catch(() => {}); // SW đang khởi động lại: nhịp sau gửi tiếp
});
