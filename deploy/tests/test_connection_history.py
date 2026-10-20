"""Lịch sử kết nối và chẩn đoán vì sao phiên Facebook hỏng (không gọi mạng thật)."""

from __future__ import annotations

import json
from pathlib import Path

import httpx
import pytest

from connection_history import (
    ConnectionHistory,
    classify_bridge_error,
    classify_raw_event,
    describe_run_error,
    diagnose_session,
)


def test_history_merges_repeats_and_survives_restart(tmp_path: Path) -> None:
    path = tmp_path / "acc" / "history.json"
    history = ConnectionHistory(path)
    history.record("connected", "Đã kết nối", at="2026-10-20T10:00:00Z")
    history.record("network", "mất mạng", at="2026-10-20T10:01:00Z")
    history.record("network", "mất mạng", at="2026-10-20T10:03:00Z")
    items = ConnectionHistory(path).list()  # đọc lại từ file
    assert [i["kind"] for i in items] == ["network", "connected"]  # mới nhất trước
    assert items[0]["count"] == 2 and items[0]["last_at"] == "2026-10-20T10:03:00Z"
    assert path.stat().st_mode & 0o777 == 0o600
    path.write_text("{hỏng", encoding="utf-8")
    assert ConnectionHistory(path).list() == []


def test_classify_bridge_and_raw_events() -> None:
    assert classify_bridge_error({"code": 2, "message": "x"}) is None
    assert "vĩnh viễn" in classify_bridge_error({"code": 1, "message": "x"})
    assert "mạng" in classify_bridge_error({"message": "read tcp: i/o timeout"})
    assert "từ chối" in classify_bridge_error({"message": "HTTP 401 Unauthorized"})
    raw = {
        "type": "raw",
        "data": {"type": "LoggedOut", "data": {"OnConnect": True, "Reason": 401}},
    }
    assert (
        classify_raw_event(raw) == "Messenger đăng xuất thiết bị E2EE của bot (mã 401)"
    )
    assert classify_raw_event({"type": "raw", "data": {"type": "Message"}}) is None
    assert describe_run_error(ConnectionError("x"))[0] == "network"


def _client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("respond", "cause"),
    [
        (
            lambda r: httpx.Response(
                400,
                headers=[
                    (
                        "set-cookie",
                        "c_user=deleted; expires=Thu, 01-Jan-1970 00:00:01 GMT",
                    ),
                    ("set-cookie", "xs=deleted; expires=Thu, 01-Jan-1970 00:00:01 GMT"),
                ],
            ),
            "revoked",
        ),
        (
            lambda r: (
                httpx.Response(
                    302, headers={"location": "https://www.facebook.com/checkpoint/123"}
                )
                if r.url.path == "/"
                else httpx.Response(200, text="verify")
            ),
            "checkpoint",
        ),
        (lambda r: httpx.Response(200, text='<form id="login_form">'), "login"),
        (lambda r: httpx.Response(200, text="<html>no tokens</html>"), "token"),
    ],
)
async def test_diagnose_session(respond, cause: str) -> None:
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen.setdefault("cookie", request.headers.get("cookie"))
        return respond(request)

    async with _client(handler) as client:
        assert await diagnose_session(client, "c_user=1; xs=x") == cause
    assert seen["cookie"] == "c_user=1; xs=x"


@pytest.mark.asyncio
async def test_diagnose_session_network_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("down")

    async with _client(handler) as client:
        assert await diagnose_session(client, "c_user=1") == "unknown"


def test_history_file_is_json_list(tmp_path: Path) -> None:
    history = ConnectionHistory(tmp_path / "history.json")
    history.record("session_expired", "Facebook đã huỷ phiên")
    data = json.loads((tmp_path / "history.json").read_text(encoding="utf-8"))
    assert data[0]["kind"] == "session_expired" and data[0]["at"].endswith("Z")
