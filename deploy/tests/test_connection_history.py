"""Lịch sử kết nối và chẩn đoán vì sao phiên Facebook hỏng (không gọi mạng thật)."""

from __future__ import annotations

import json
from pathlib import Path

import httpx
import pytest

from connection_history import (
    ConnectionHistory,
    DecryptFailureWatch,
    classify_bridge_error,
    classify_raw_event,
    decrypt_failure,
    decrypt_history_message,
    describe_run_error,
    device_tag,
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


def _decrypt_error(**extra: object) -> dict[str, object]:
    return {
        "message": "E2EE message could not be decrypted or was unavailable",
        "code": 2,
        "reason": "bad_mac",
        "messageId": "M1",
        "senderJid": "100012.0:3@msgr",
        "isGroup": True,
        **extra,
    }


def test_decrypt_failure_labels_reason_without_raw_ids() -> None:
    failure = decrypt_failure(_decrypt_error())
    assert failure is not None
    assert failure["reason"] == "bad_mac"
    assert failure["device"] == device_tag("100012.0:3@msgr")
    message = decrypt_history_message(failure)
    assert "MAC không khớp" in message and "nhóm" in message
    assert failure["device"] in message and "100012" not in message

    # Bridge cũ không gửi reason: vẫn ghi, nhãn "không rõ".
    old = decrypt_failure({"message": "x", "code": 2})
    assert old is not None and old["label"] == "không rõ lý do" and old["device"] == ""
    # Thiết bị gửi cố ý không gửi bản mã cho bot, và lỗi khác code 2: bỏ qua.
    assert decrypt_failure(_decrypt_error(reason="unavailable")) is None
    assert decrypt_failure({"message": "x", "code": 1}) is None
    assert decrypt_failure(None) is None


def test_decrypt_history_message_names_failed_ciphertext() -> None:
    failure = decrypt_failure(
        _decrypt_error(reason="no_valid_session", encType="pkmsg")
    )
    assert failure is not None and failure["enc_type"] == "pkmsg"
    assert "tin mở phiên" in decrypt_history_message(failure)

    unknown = decrypt_failure(_decrypt_error(encType="weird"))
    assert unknown is not None and unknown["enc_type"] == ""
    assert "tin thường" not in decrypt_history_message(unknown)


def test_decrypt_history_message_mentions_stateless_prekeys() -> None:
    failure = decrypt_failure(_decrypt_error(reason="no_session", statelessPrekeys=2))
    assert failure is not None and failure["stateless_prekeys"] == 2
    assert "2 tin mở phiên kiểu stateless" in decrypt_history_message(failure)

    for value in (0, -1, True, "3", None):
        plain = decrypt_failure(_decrypt_error(statelessPrekeys=value))
        assert plain is not None and plain["stateless_prekeys"] == 0
        assert "stateless" not in decrypt_history_message(plain)


def test_decrypt_watch_alerts_once_per_device_after_distinct_messages() -> None:
    now = [0.0]
    watch = DecryptFailureWatch(threshold=3, window=100, clock=lambda: now[0])

    def observe(message_id: str, sender: str = "a@msgr") -> int:
        failure = decrypt_failure(
            _decrypt_error(messageId=message_id, senderJid=sender)
        )
        assert failure is not None
        return watch.observe(failure)

    # Bridge thử lại một tin vài lần với cùng ID: chỉ tính một.
    assert [observe("M1") for _ in range(5)] == [0] * 5
    assert observe("M2") == 0
    assert observe("X1", sender="b@msgr") == 0  # thiết bị khác đếm riêng
    assert observe("M3") == 3
    assert observe("M4") == 0  # đã báo trong cửa sổ
    now[0] = 250.0  # hết cửa sổ: các tin cũ không còn tính
    assert observe("M5") == 0 and observe("M6") == 0
    assert observe("M7") == 3

    # Thiếu thiết bị hoặc ID tin (bridge cũ): không đếm được, không báo.
    assert watch.observe({"device": "", "message_id": "M9"}) == 0
