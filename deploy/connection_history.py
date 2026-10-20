"""Lịch sử kết nối của từng tài khoản: rớt lúc nào, vì sao, nối lại sau bao lâu.

Hiển thị ở trang "Tài khoản kênh" (trường ``history`` của ``/accounts``) và lưu
ra ``<accounts>/<tên>/history.json`` (0600) để khởi động lại vẫn còn. Chỉ ghi
nhãn nguyên nhân do bot tự phân loại, không ghi chữ lỗi thô của bridge/Facebook
(có thể chứa URL, token) và không bao giờ ghi cookie.
"""

from __future__ import annotations

import json
import os
import tempfile
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import httpx

from _core._permissions import set_private_file_permissions
from _core._session import _build_home_request

HISTORY_FILE = "history.json"
HISTORY_MAX = 30

# kind: connected | network | disconnected | platform | session_expired
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
