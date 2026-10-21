from __future__ import annotations

import asyncio
import json
from pathlib import Path

import httpx
import pytest

from _core._utils import BROWSER_USER_AGENT, parse_cookie_string
from session_keeper import keep_session, merge_cookies, refresh_once

COOKIE = "c_user=1001; xs=old_xs; fr=old_fr; datr=d;"
HOME = '<html>..."actorID":"1001"...</html>'


def write_config(tmp_path: Path, cookie: str = COOKIE) -> Path:
    path = tmp_path / "config.json"
    path.write_text(json.dumps({"cookies": cookie, "prefix": "/"}))
    return path


def saved(path: Path) -> dict[str, str]:
    return parse_cookie_string(json.loads(path.read_text())["cookies"])


def test_merge_cookies_rules() -> None:
    assert (
        parse_cookie_string(merge_cookies(COOKIE, {"fr": "new"}) or "")["fr"] == "new"
    )
    assert merge_cookies(COOKIE, {"fr": "old_fr"}) is None  # không đổi
    assert merge_cookies(COOKIE, {"c_user": "999", "xs": "x"}) is None  # tài khoản khác
    assert merge_cookies(COOKIE, {"xs": ""}) is None  # giá trị rỗng bị bỏ qua
    assert merge_cookies(COOKIE, {"xs": "deleted"}) is None  # Facebook xoá cookie
    assert merge_cookies("fr=1;", {"fr": "2"}) is None  # chưa đăng nhập
    # Không bao giờ xoá khoá cũ.
    assert set(parse_cookie_string(merge_cookies(COOKIE, {"sb": "s"}) or "")) == {
        "c_user",
        "xs",
        "fr",
        "datr",
        "sb",
    }


@pytest.mark.asyncio
async def test_refresh_saves_bridge_and_homepage_cookies(tmp_path: Path) -> None:
    path = write_config(tmp_path)
    seen: list[httpx.Request] = []

    def home(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(
            200, text=HOME, headers={"set-cookie": "xs=new_xs; Path=/; Secure"}
        )

    async def bridge() -> dict[str, str]:
        return {"c_user": "1001", "xs": "old_xs", "fr": "bridge_fr"}

    async with httpx.AsyncClient(transport=httpx.MockTransport(home)) as client:
        changed = await refresh_once(path, bridge, client=client)

    assert changed == ["fr", "xs"]
    cookies = saved(path)
    assert (
        cookies["fr"] == "bridge_fr"
        and cookies["xs"] == "new_xs"
        and cookies["datr"] == "d"
    )
    # Trang chủ được mở bằng cookie mới từ bridge và cùng danh tính trình duyệt.
    assert "fr=bridge_fr" in seen[0].headers["cookie"]
    assert seen[0].headers["user-agent"] == BROWSER_USER_AGENT
    assert json.loads(path.read_text())["prefix"] == "/"  # giữ phần config khác


@pytest.mark.asyncio
async def test_refresh_keeps_old_cookie_when_logged_out(tmp_path: Path) -> None:
    path = write_config(tmp_path)

    def login_page(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200, text="<html>login</html>", headers={"set-cookie": "xs=deleted"}
        )

    async def bridge() -> dict[str, str]:
        return {"fr": "bridge_fr"}

    async with httpx.AsyncClient(transport=httpx.MockTransport(login_page)) as client:
        assert await refresh_once(path, bridge, client=client) == []
    assert saved(path) == parse_cookie_string(COOKIE)


@pytest.mark.asyncio
async def test_refresh_ignores_cookies_of_another_account(tmp_path: Path) -> None:
    path = write_config(tmp_path)

    async def bridge() -> dict[str, str]:
        return {"c_user": "2002", "xs": "other"}

    transport = httpx.MockTransport(lambda r: httpx.Response(200, text=HOME))
    async with httpx.AsyncClient(transport=transport) as client:
        assert await refresh_once(path, bridge, client=client) == []
    assert saved(path) == parse_cookie_string(COOKIE)


@pytest.mark.asyncio
async def test_keep_session_logs_without_values_and_survives_errors(
    tmp_path: Path,
) -> None:
    path = write_config(tmp_path)
    logs: list[str] = []
    calls = 0

    async def bridge() -> dict[str, str]:
        nonlocal calls
        calls += 1
        if calls == 1:
            raise RuntimeError("bridge not ready")
        return {"fr": "secret_new_fr"}

    transport = httpx.MockTransport(lambda r: httpx.Response(200, text=HOME))
    async with httpx.AsyncClient(transport=transport) as client:
        task = asyncio.create_task(
            keep_session(
                path,
                bridge,
                lambda tag, msg: logs.append(msg),
                client=client,
                first_delay=0,
                every=0.01,
                jitter=0,
            )
        )
        while calls < 2 or len(logs) < 2:
            await asyncio.sleep(0.01)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    assert logs[0] == "Không làm mới được phiên: RuntimeError"
    assert logs[1] == "Đã lưu cookie mới: fr"
    assert all("secret" not in line for line in logs)
    assert saved(path)["fr"] == "secret_new_fr"
