// Chế độ run (không ai nhìn): trang Business Suite đang "xem" tốn ~20% CPU vì animation
// CSS dựng khung 60 lần/giây và JS chạy như tab đang mở. Tắt animation/transition và
// báo trang đang ở nền (như tab nền của Chrome thường): Business Suite tự giảm việc,
// socket Lightspeed vẫn nhận tin. Đo 31/10/2026: renderer ~22% -> ~3% CPU.
(() => {
  const hidden = { get: () => "hidden", configurable: true };
  Object.defineProperty(Document.prototype, "visibilityState", hidden);
  Object.defineProperty(Document.prototype, "hidden", { get: () => true, configurable: true });
  Document.prototype.hasFocus = () => false;
  // Service worker của Facebook (push, cache) tốn ~1,3% CPU và bộ nhớ trong renderer,
  // không cần cho việc nghe tin (socket Lightspeed nằm ở trang). Gỡ bản đã đăng ký và
  // không cho đăng ký lại.
  if (navigator.serviceWorker) {
    const sw = navigator.serviceWorker;
    sw.register = () => Promise.reject(new DOMException("disabled by fb-bridge-web", "SecurityError"));
    sw.getRegistrations().then((rs) => rs.forEach((r) => r.unregister())).catch(() => {});
  }
  // Web worker của Facebook (static_resources/webworker_v1, "RSTWebWorker") tốn ~1% CPU và
  // heap riêng; socket Lightspeed nằm ở trang chính nên không cần. Trả về worker giả
  // không làm gì (ném lỗi thì khởi tạo trang có thể hỏng).
  const NativeWorker = window.Worker;
  window.Worker = function (url, opts) {
    if (String(url).includes("/webworker_v1/")) {
      const fake = new EventTarget();
      fake.postMessage = () => {};
      fake.terminate = () => {};
      fake.onmessage = fake.onerror = fake.onmessageerror = null;
      return fake;
    }
    return new NativeWorker(url, opts);
  };
  window.Worker.prototype = NativeWorker.prototype;
  const style = document.createElement("style");
  style.textContent = "*,*::before,*::after{animation:none!important;transition:none!important}";
  (document.head || document.documentElement).appendChild(style);
})();
