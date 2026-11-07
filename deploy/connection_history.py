"""Lịch sử kết nối của từng tài khoản: rớt lúc nào, vì sao, nối lại sau bao lâu.

Hiển thị ở trang "Tài khoản kênh" (trường ``history`` của ``/accounts``) và lưu
ra ``<accounts>/<tên>/history.json`` (0600) để khởi động lại vẫn còn. Chỉ ghi
nhãn nguyên nhân do bot tự phân loại, không ghi chữ lỗi thô của bridge/Facebook
(có thể chứa URL, token) và không bao giờ ghi cookie.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import httpx

from _core._permissions import set_private_file_permissions
from _core._session import _build_home_request

HISTORY_FILE = "history.json"
HISTORY_MAX = 30

# kind: connected | network | disconnected | platform | session_expired | decrypt
SESSION_CAUSES = {
    "revoked": (
        "Facebook đã huỷ phiên (xoá cookie c_user/xs): đăng xuất từ xa, đổi mật khẩu "
        "hoặc Facebook tự cắt vì nghi ngờ đăng nhập bất thường."
    ),
    "checkpoint": "Facebook yêu cầu xác minh tài khoản (checkpoint): mở Facebook trên điện thoại để xác nhận.",
    "login": "Facebook chuyển về trang đăng nhập: phiên không còn hiệu lực.",
    "token": "Trang Facebook không còn token phiên (có thể Facebook đổi giao diện hoặc phiên bị giới hạn).",
    "unknown": "Không kiểm tra được nguyên nhân (Facebook không trả lời bình thường).",
}

# whatsmeow raw events (E2EE) có ý nghĩa với việc rớt kết nối.
RAW_EVENT_CAUSES = {
    "LoggedOut": "Messenger đăng xuất thiết bị E2EE của bot",
    "StreamReplaced": "Một phiên Messenger khác chiếm kết nối E2EE của bot",
    "TemporaryBan": "Messenger tạm khoá tài khoản",
    "ConnectFailure": "Messenger từ chối kết nối E2EE",
    "KeepAliveTimeout": "Kết nối E2EE không phản hồi (mạng chập chờn)",
    "StreamError": "Messenger đóng luồng E2EE do lỗi",
}

_NETWORK_HINTS = (
    "eof",
    "timeout",
    "timed out",
    "connection reset",
    "connection refused",
    "no such host",
    "network is unreachable",
    "broken pipe",
    "temporary failure in name resolution",
)
_AUTH_HINTS = (
    "401",
    "unauthorized",
    "logged out",
    "not logged in",
    "login",
    "checkpoint",
)


# Lý do bridge gửi kèm event "error" code 2 (bridge-e2ee/bridge/decrypt_failures.go).
DECRYPT_REASONS = {
    "bad_mac": "MAC không khớp, phiên mã hoá với thiết bị gửi đã lệch",
    "no_session": "chưa có phiên mã hoá với thiết bị gửi",
    "no_sender_key": "thiếu khoá nhóm của thiết bị gửi",
    "untrusted_identity": "khoá danh tính của thiết bị gửi đã đổi",
    "prekey": "lỗi khoá dùng một lần (prekey)",
    "too_far_in_future": "tin vượt quá xa bộ đếm của phiên",
    "invalid_message": "tin hỏng hoặc sai phiên bản",
    "other": "lỗi giải mã khác",
    "unknown": "không rõ lý do",
}
# Thiết bị gửi chủ động không gửi bản mã cho bot (loại tin cố ý ẩn): không phải lỗi.
_DECRYPT_IGNORED = {"unavailable"}
DECRYPT_ALERT_AFTER = 3  # số tin khác nhau từ cùng một thiết bị
DECRYPT_ALERT_WINDOW = 24 * 3600.0
_DECRYPT_MAX_DEVICES = 256


def _now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class ConnectionHistory:
    """Các mốc kết nối của một tài khoản (mới nhất ở cuối), gộp mốc trùng liền nhau."""

    def __init__(self, path: Path | None) -> None:
        self.path = path
        self._lock = threading.Lock()
        self.items: list[dict[str, Any]] = []
        if path is not None:
            try:
                data = json.loads(path.read_text(encoding="utf-8"))
                if isinstance(data, list):
                    self.items = [i for i in data if isinstance(i, dict)][-HISTORY_MAX:]
            except (OSError, ValueError):
                pass

    def record(self, kind: str, message: str = "", *, at: str | None = None) -> None:
        at = at or _now()
        with self._lock:
            last = self.items[-1] if self.items else None
            if last and last.get("kind") == kind and last.get("message") == message:
                last["count"] = int(last.get("count") or 1) + 1
                last["last_at"] = at
            else:
                self.items.append({"at": at, "kind": kind, "message": message})
                del self.items[:-HISTORY_MAX]
            snapshot = [dict(i) for i in self.items]
        self._save(snapshot)

    def last(self) -> dict[str, Any] | None:
        with self._lock:
            return dict(self.items[-1]) if self.items else None

    def list(self) -> list[dict[str, Any]]:
        with self._lock:
            return [dict(i) for i in reversed(self.items)]

    def _save(self, items: list[dict[str, Any]]) -> None:
        if self.path is None:
            return
        tmp = None
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            fd, tmp = tempfile.mkstemp(
                dir=self.path.parent, prefix=f".{HISTORY_FILE}.", suffix=".tmp"
            )
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                json.dump(items, handle, ensure_ascii=False, indent=1)
            set_private_file_permissions(Path(tmp))
            os.replace(tmp, self.path)
            tmp = None
        except OSError:
            pass  # lịch sử chỉ để xem: lỗi ghi không được làm hỏng bot
        finally:
            if tmp is not None:
                Path(tmp).unlink(missing_ok=True)


def classify_bridge_error(data: Any) -> str | None:
    """Nhãn cho event "error" của bridge; None nếu không liên quan kết nối."""
    if not isinstance(data, dict):
        return None
    code = data.get("code")
    text = str(data.get("message") or "").lower()
    if code == 2:
        return None  # tin E2EE không giải mã được
    if code == 1:
        return "Messenger báo lỗi kết nối vĩnh viễn (thường do phiên bị huỷ)"
    if any(h in text for h in _AUTH_HINTS):
        return "Messenger từ chối phiên đăng nhập"
    if any(h in text for h in _NETWORK_HINTS):
        return "Kết nối Messenger bị ngắt (mạng)"
    return "Kết nối Messenger bị ngắt"


def classify_raw_event(event: dict[str, Any]) -> str | None:
    """Nhãn cho raw event whatsmeow (LoggedOut, StreamReplaced...), kèm mã lý do."""
    data = event.get("data")
    if not isinstance(data, dict):
        return None
    label = RAW_EVENT_CAUSES.get(str(data.get("type") or ""))
    if label is None:
        return None
    inner = data.get("data")
    reason = inner.get("Reason") if isinstance(inner, dict) else None
    return f"{label} (mã {reason})" if isinstance(reason, int) and reason else label


async def diagnose_session(http_client: httpx.AsyncClient, cookie: str) -> str:
    """Một request tới trang chủ để biết vì sao phiên hỏng; trả khoá của SESSION_CAUSES."""
    headers = {
        k: v
        for k, v in _build_home_request(cookie)["headers"].items()
        if k not in {"authority", "method", "path", "scheme"}
    }
    try:
        response = await http_client.get(
            "https://www.facebook.com/",
            headers=headers,
            follow_redirects=True,
            timeout=20,
        )
    except httpx.HTTPError:
        return "unknown"
    cleared = set()
    for header in response.headers.get_list("set-cookie"):
        name, _, rest = header.partition("=")
        if rest.split(";", 1)[0] == "deleted":
            cleared.add(name.strip())
    for hop in [*response.history, response]:
        path = hop.url.path
        if path.startswith("/checkpoint"):
            return "checkpoint"
    if cleared & {"c_user", "xs"}:
        return "revoked"
    if (
        response.url.path.startswith("/login")
        or "login_form" in response.text[:200_000]
    ):
        return "login"
    return "token"


def describe_run_error(error: BaseException) -> tuple[str, str]:
    """(kind, message) cho lỗi làm tài khoản dừng chạy, trừ hết phiên."""
    if isinstance(error, ConnectionError):
        return "network", "Không tới được Facebook (mất mạng hoặc DNS)."
    name = type(error).__name__
    text = str(error).lower()
    if name == "BridgeError" and "watchdog" in text:
        return "disconnected", "Mất kết nối Messenger, bridge không tự nối lại được."
    if name in {"ConnectError", "ConnectTimeout", "ReadTimeout", "RemoteProtocolError"}:
        return "network", f"Lỗi mạng khi gọi Facebook ({name})."
    return "disconnected", f"Bot dừng do lỗi {name}."


def device_tag(jid: str) -> str:
    """Mã ngắn cố định cho một thiết bị gửi: so được giữa các lần, không lộ ID."""
    return hashlib.sha256(jid.encode("utf-8")).hexdigest()[:8]


def decrypt_failure(data: Any) -> dict[str, Any] | None:
    """Thông tin của event "error" code 2 cần ghi; None nếu không phải lỗi giải mã."""
    if not isinstance(data, dict) or data.get("code") != 2:
        return None
    reason = str(data.get("reason") or "unknown")
    if reason in _DECRYPT_IGNORED:
        return None
    sender = data.get("senderJid")
    return {
        "reason": reason,
        "label": DECRYPT_REASONS.get(reason, DECRYPT_REASONS["unknown"]),
        "device": device_tag(sender) if isinstance(sender, str) and sender else "",
        "message_id": str(data.get("messageId") or ""),
        "group": bool(data.get("isGroup")),
        "from_me": bool(data.get("isFromMe")),
        "stateless_prekeys": _non_negative_int(data.get("statelessPrekeys")),
    }


def _non_negative_int(value: Any) -> int:
    return (
        value
        if isinstance(value, int) and not isinstance(value, bool) and value > 0
        else 0
    )


def decrypt_history_message(failure: dict[str, Any]) -> str:
    where = "nhóm" if failure["group"] else "chat 1-1"
    device = f", thiết bị {failure['device']}" if failure["device"] else ""
    own = " của chính tài khoản" if failure["from_me"] else ""
    message = f"Tin E2EE không giải mã được ({where}{device}{own}): {failure['label']}"
    stateless = failure.get("stateless_prekeys", 0)
    if stateless:
        message += (
            f"; thiết bị này đã gửi {stateless} tin mở phiên kiểu stateless"
            " (state=false) nên bot không lưu phiên"
        )
    return message


class DecryptFailureWatch:
    """Đếm tin không giải mã được theo thiết bị gửi để báo một lần khi lỗi lặp lại.

    Một thiết bị lỗi liên tục (phiên Signal lệch) không tự khỏi; bridge thử lại
    mỗi tin vài lần với cùng ID nên đếm theo ID tin, không theo event.
    """

    def __init__(
        self,
        *,
        threshold: int = DECRYPT_ALERT_AFTER,
        window: float = DECRYPT_ALERT_WINDOW,
        clock: Any = time.monotonic,
    ) -> None:
        self.threshold = threshold
        self.window = window
        self.clock = clock
        self._lock = threading.Lock()
        self._seen: dict[str, dict[str, float]] = {}  # thiết bị -> {ID tin: lúc}
        self._alerted: dict[str, float] = {}

    def observe(self, failure: dict[str, Any]) -> int:
        """Trả về số tin lỗi khi vừa chạm ngưỡng (cần báo), ngược lại 0."""
        device, message_id = failure.get("device"), failure.get("message_id")
        if not device or not message_id:
            return 0
        now = self.clock()
        with self._lock:
            seen = self._seen.setdefault(device, {})
            seen[message_id] = now
            for key, at in list(seen.items()):
                if now - at > self.window:
                    del seen[key]
            if len(self._seen) > _DECRYPT_MAX_DEVICES:
                oldest = min(
                    self._seen, key=lambda d: max(self._seen[d].values(), default=0)
                )
                self._seen.pop(oldest, None)
                self._alerted.pop(oldest, None)
            alerted_at = self._alerted.get(device)
            if alerted_at is not None and now - alerted_at <= self.window:
                return 0
            if len(seen) < self.threshold:
                return 0
            self._alerted[device] = now
            return len(seen)
