"""Giữ phiên Facebook của một tài khoản như một trình duyệt dùng hằng ngày.

Facebook cấp lại cookie (``fr``, ``xs``...) qua Set-Cookie; bridge E2EE (messagix)
áp dụng chúng trong bộ nhớ, nhưng bot khởi động lại sẽ quay về cookie lúc đăng
nhập. Keeper định kỳ:

1. lấy cookie hiện tại từ bridge (``getCookies``),
2. mở trang chủ Facebook một lần bằng chính cookie đó và cùng danh tính trình
   duyệt với bridge (như người dùng mở Facebook mỗi ngày, không thêm phiên mới),
3. gộp cookie Facebook trả về và lưu vào config.json của tài khoản.

Chỉ lưu khi ``c_user`` không đổi và ``xs`` còn: phiên đã bị huỷ thì giữ nguyên
cookie cũ để chẩn đoán nguyên nhân. Không bao giờ xoá khoá, không log giá trị.
"""

from __future__ import annotations

import asyncio
import gc
import random
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import Any

import httpx

from _core._session import _build_home_request
from _core._storage import FileSessionStorage
from _core._utils import parse_cookie_string, send_get_request_async

FIRST_REFRESH_SECONDS = 10 * 60
REFRESH_EVERY_SECONDS = 24 * 60 * 60
REFRESH_JITTER_SECONDS = 2 * 60 * 60
REQUIRED = ("c_user", "xs")


def merge_cookies(current: str, updates: dict[str, str]) -> str | None:
    """Cookie string sau khi áp ``updates``; None nếu không đổi hoặc không an toàn.

    Không an toàn: thiếu c_user/xs sau khi gộp, hoặc c_user khác tài khoản.
    """
    old = parse_cookie_string(current)
    if not all(old.get(name) for name in REQUIRED):
        return None
    fresh = {k: v for k, v in updates.items() if k and v and v != "deleted"}
    if fresh.get("c_user", old["c_user"]) != old["c_user"]:
        return None
    merged = {**old, **fresh}
    if merged == old or not all(merged.get(name) for name in REQUIRED):
        return None
    return "; ".join(f"{k}={v}" for k, v in merged.items()) + ";"


def logged_in(html: str, uid: str) -> bool:
    """Trang chủ vẫn là của tài khoản đã đăng nhập (không phải trang login).

    Cùng dấu hiệu dataGetHome dùng để lấy FacebookID.
    """
    return bool(uid) and f'"actorID":"{uid}"' in html


async def refresh_once(
    config_path: Path,
    get_bridge_cookies: Callable[[], Awaitable[dict[str, str]]],
    *,
    client: httpx.AsyncClient | None = None,
) -> list[str]:
    """Một lần làm mới; trả về tên các cookie đã đổi (rỗng nếu không lưu gì)."""
    storage = FileSessionStorage(str(config_path), key="cookies")
    current = storage.load() or ""
    uid = parse_cookie_string(current).get("c_user", "")
    cookie = merge_cookies(current, await get_bridge_cookies()) or current

    response = await send_get_request_async(_build_home_request(cookie), client=client)
    try:
        if response.status_code != 200 or not logged_in(response.text, uid):
            return []
        returned = {name: value for name, value in response.cookies.items()}
    finally:
        await response.aclose()
        del response
        gc.collect()  # HTML trang chủ vài MB: giải phóng ngay

    merged = merge_cookies(cookie, returned) or cookie
    if merged == current:
        return []
    before, after = parse_cookie_string(current), parse_cookie_string(merged)
    changed = sorted(k for k in after if before.get(k) != after[k])
    storage.save(merged)
    return changed


async def keep_session(
    config_path: Path,
    get_bridge_cookies: Callable[[], Awaitable[dict[str, str]]],
    log: Callable[[str, str], Any],
    *,
    client: httpx.AsyncClient | None = None,
    first_delay: float = FIRST_REFRESH_SECONDS,
    every: float = REFRESH_EVERY_SECONDS,
    jitter: float = REFRESH_JITTER_SECONDS,
) -> None:
    """Chạy tới khi bị huỷ: làm mới sau ``first_delay`` rồi mỗi ``every`` (± jitter)."""
    delay = first_delay
    while True:
        await asyncio.sleep(delay)
        try:
            changed = await refresh_once(config_path, get_bridge_cookies, client=client)
        except (httpx.HTTPError, RuntimeError, OSError) as error:
            log("session", f"Không làm mới được phiên: {type(error).__name__}")
        else:
            if changed:
                log("session", f"Đã lưu cookie mới: {', '.join(changed)}")
            else:
                log("session", "Phiên còn hiệu lực, cookie không đổi")
        delay = every + random.uniform(-jitter, jitter)
