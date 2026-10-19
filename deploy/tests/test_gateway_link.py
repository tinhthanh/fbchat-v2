from __future__ import annotations

import asyncio
import base64
import json
from typing import Any

import httpx
import pytest

import gateway_link
import main as upstream_main
import multi_main
from gateway_link import (
    GatewayLink,
    describe_message,
    fetch_attachment,
    media_rpc,
    thread_ref,
)

TOKEN = "0123456789abcdef0123456789abcdef"


class _Listener:
    startup_timeout = 1.0
    device_path: str | None = None

    def __init__(self) -> None:
        self.e2ee: list[tuple[str, str]] = []
        self.regular: list[tuple[int, str]] = []
        self.fail = False
        self.result: dict[str, Any] = {"messageId": "mid.out"}

    def on_message(self, callback: Any) -> None:
        pass

    async def send_e2ee_message(
        self, chat_jid: str, text: str, *args: Any
    ) -> dict[str, Any]:
        if self.fail:
            raise RuntimeError("bridge down")
        self.e2ee.append((chat_jid, text))
        return self.result

    async def send_message(
        self, thread_id: int, text: str, *args: Any
    ) -> dict[str, Any]:
        self.regular.append((thread_id, text))
        return self.result


class _FakeBridge:
    def __init__(self, data: bytes = b"FILEDATA") -> None:
        self.calls: list[tuple[str, dict[str, Any]]] = []
        self.data = base64.b64encode(data).decode()

    async def call(
        self, method: str, params: dict[str, Any] | None = None, timeout: float = 60.0
    ) -> dict[str, Any]:
        self.calls.append((method, params or {}))
        if method.startswith("download"):
            return {"data": self.data, "mimeType": "image/jpeg"}
        return {"messageId": "mid.media"}


class _Bot:
    def __init__(self) -> None:
        self.dataFB = {"FacebookID": "100"}
        self.listener = _Listener()
        self.listener._bridge = _FakeBridge()  # type: ignore[attr-defined]


def _link(handler: Any) -> GatewayLink:
    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return GatewayLink("http://gateway:8080", TOKEN, client, lambda tag, msg: None)


def test_thread_ref_distinguishes_e2ee_one_to_one_and_group() -> None:
    assert thread_ref({"chatJid": "123@msgr"}) == ("e2ee:123@msgr", False)
    assert thread_ref({"raw": {"threadId": 55, "threadType": 1}}) == (
        "thread:55",
        False,
    )
    assert thread_ref({"raw": {"threadId": 56, "threadType": 2}}) == ("thread:56", True)
    assert thread_ref({"raw": {}}) is None


def test_describe_message_includes_attachments() -> None:
    message = {
        "body": "xem ảnh",
        "attachments": [{"type": "image", "url": "https://x/y.jpg"}],
    }
    assert describe_message(message) == "xem ảnh\n[image] https://x/y.jpg"
    assert describe_message({"body": "", "attachments": []}) == ""


@pytest.mark.asyncio
async def test_forward_posts_inbound_with_names(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    sent: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.headers["authorization"] == f"Bearer {TOKEN}"
        sent.append(json.loads(request.content))
        return httpx.Response(200, json={})

    async def fake_user_info(
        dataFB: dict[str, Any], user_id: str, **_: Any
    ) -> dict[str, Any]:
        return {"nameUser": f"User {user_id}"}

    monkeypatch.setattr(gateway_link._get_user_info, "func", fake_user_info)
    link = _link(handler)
    group = {
        "messageID": "m1",
        "userID": "7",
        "body": "chào",
        "raw": {"threadId": 9, "threadType": 2, "threadName": "Nhóm A"},
    }
    await link.forward("acc1", _Bot(), group)
    direct = {"messageID": "m2", "userID": "8", "body": "hi", "chatJid": "8@msgr"}
    await link.forward("acc1", _Bot(), direct)

    assert sent[0] == {
        "channel": "facebook",
        "account": "acc1",
        "thread_id": "thread:9",
        "is_group": True,
        "thread_name": "Nhóm A",
        "sender_name": "User 7",
        "text": "chào",
        "message_id": "m1",
        "attachments": [],
    }
    assert sent[1]["thread_id"] == "e2ee:8@msgr" and sent[1]["thread_name"] == "User 8"


@pytest.mark.asyncio
async def test_send_reply_routes_by_thread_prefix_and_dedupes() -> None:
    link = _link(lambda r: httpx.Response(200))
    bot = _Bot()
    link.bots["acc1"] = bot  # type: ignore[assignment]

    assert await link.send_reply(
        {"account": "acc1", "thread_id": "e2ee:8@msgr", "text": "a", "message_id": "r1"}
    ) == (200, "ok")
    assert await link.send_reply(
        {"account": "acc1", "thread_id": "e2ee:8@msgr", "text": "a", "message_id": "r1"}
    ) == (200, "duplicate")
    assert await link.send_reply(
        {"account": "acc1", "thread_id": "thread:9", "text": "b", "message_id": "r2"}
    ) == (200, "ok")
    assert (
        await link.send_reply(
            {
                "account": "acc1",
                "thread_id": "thread:abc",
                "text": "c",
                "message_id": "r3",
            }
        )
    )[0] == 400
    assert (
        await link.send_reply({"account": "nope", "thread_id": "thread:9", "text": "c"})
    )[0] == 404
    assert bot.listener.e2ee == [("8@msgr", "a")]
    assert bot.listener.regular == [(9, "b")]


@pytest.mark.asyncio
async def test_send_reply_bridge_error_allows_retry() -> None:
    link = _link(lambda r: httpx.Response(200))
    bot = _Bot()
    link.bots["acc1"] = bot  # type: ignore[assignment]
    bot.listener.fail = True
    payload = {
        "account": "acc1",
        "thread_id": "e2ee:8@msgr",
        "text": "a",
        "message_id": "r1",
    }
    assert (await link.send_reply(payload))[0] == 502
    bot.listener.fail = False
    assert await link.send_reply(payload) == (200, "ok")


@pytest.mark.asyncio
async def test_http_server_requires_token_and_sends() -> None:
    link = _link(lambda r: httpx.Response(200))
    bot = _Bot()
    link.bots["acc1"] = bot  # type: ignore[assignment]
    server = await asyncio.start_server(link._handle_connection, "127.0.0.1", 0)
    port = server.sockets[0].getsockname()[1]
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}") as client:
            body = {
                "account": "acc1",
                "thread_id": "thread:9",
                "text": "xin chào",
                "message_id": "r9",
            }
            bad = await client.post(
                "/send", json=body, headers={"Authorization": "Bearer wrong"}
            )
            good = await client.post(
                "/send", json=body, headers={"Authorization": f"Bearer {TOKEN}"}
            )
            missing = await client.post(
                "/other", json=body, headers={"Authorization": f"Bearer {TOKEN}"}
            )
    finally:
        server.close()
        await server.wait_closed()
    assert (bad.status_code, good.status_code, missing.status_code) == (401, 200, 404)
    assert bot.listener.regular == [(9, "xin chào")]


@pytest.mark.asyncio
async def test_account_bot_forwards_to_gateway_instead_of_commands(
    monkeypatch: pytest.MonkeyPatch, mock_dataFB: dict[str, Any]
) -> None:
    listener = _Listener()
    monkeypatch.setattr(
        upstream_main, "listeningE2EEEvent", lambda data, **options: listener
    )
    forwarded: list[str] = []

    class _FakeGateway:
        async def forward(
            self, account: str, bot: Any, message: dict[str, Any]
        ) -> None:
            forwarded.append(message["body"])

    monkeypatch.setattr(multi_main, "_gateway", _FakeGateway())
    monkeypatch.setattr(multi_main, "COMMANDS_FLAG", False)
    bot = multi_main.AccountBot(mock_dataFB, account="acc1", device_path="/d.json")

    await bot._dispatch(
        {"messageID": "m1", "userID": "555", "body": "/ping", "chatJid": "555@msgr"}
    )
    await bot._dispatch(
        {
            "messageID": "m2",
            "userID": mock_dataFB["FacebookID"],
            "body": "own",
            "chatJid": "x",
        }
    )

    assert forwarded == [
        "/ping"
    ]  # chuyển nguyên văn, không chạy lệnh; bỏ tin của chính bot
    assert listener.e2ee == []


@pytest.mark.asyncio
async def test_fetch_attachment_uses_e2ee_or_regular_download() -> None:
    bridge = _FakeBridge(b"IMG")
    e2ee = {
        "type": "image",
        "directPath": "/v/t62",
        "mediaKey": "a2V5",
        "mediaSha256": "c2hh",
        "mediaEncSha256": "ZW5j",
        "mimeType": "image/jpeg",
        "fileSize": 3,
    }
    file = await fetch_attachment(bridge, e2ee)  # type: ignore[arg-type]
    assert file == {
        "name": "image.jpg",
        "content_type": "image/jpeg",
        "data": base64.b64encode(b"IMG").decode(),
    }
    assert (
        bridge.calls[0][0] == "downloadE2EEMedia"
        and bridge.calls[0][1]["mediaType"] == "image"
    )

    regular = {
        "type": "file",
        "url": "https://cdn.fb/x.pdf",
        "fileName": "x.pdf",
        "mimeType": "application/pdf",
    }
    assert (await fetch_attachment(bridge, regular))["name"] == "x.pdf"  # type: ignore[arg-type,index]
    assert bridge.calls[1] == ("downloadMedia", {"url": "https://cdn.fb/x.pdf"})

    assert await fetch_attachment(bridge, {"type": "sticker", "url": "https://x"}) is None  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        await fetch_attachment(bridge, {"type": "video", "url": "https://x", "fileSize": 999 << 20})  # type: ignore[arg-type]


def test_media_rpc_picks_method_by_chat_and_mime() -> None:
    img = {"name": "a", "content_type": "image/png", "data": "QQ=="}
    doc = {"name": "bao-gia.pdf", "content_type": "application/pdf", "data": "QQ=="}
    assert media_rpc("e2ee", "8@msgr", img)[0] == "sendE2EEImage"
    method, params = media_rpc("e2ee", "8@msgr", doc)
    assert (
        method == "sendE2EEDocument"
        and params["filename"] == "bao-gia.pdf"
        and params["chatJid"] == "8@msgr"
    )
    assert (
        media_rpc("e2ee", "8@msgr", {**img, "content_type": "video/mp4"})[0]
        == "sendE2EEVideo"
    )
    assert (
        media_rpc("e2ee", "8@msgr", {**img, "content_type": "audio/mpeg"})[0]
        == "sendE2EEAudio"
    )
    method, params = media_rpc("thread", "9", img)
    assert (
        method == "sendImage"
        and params["threadId"] == 9
        and params["filename"] == "a.png"
    )
    assert media_rpc("thread", "9", doc)[0] == "sendFile"


@pytest.mark.asyncio
async def test_send_reply_sends_text_then_files() -> None:
    link = _link(lambda r: httpx.Response(200))
    bot = _Bot()
    link.bots["acc1"] = bot  # type: ignore[assignment]
    payload = {
        "account": "acc1",
        "thread_id": "e2ee:8@msgr",
        "text": "gửi bạn file",
        "message_id": "r1",
        "attachments": [
            {"name": "a.pdf", "content_type": "application/pdf", "data": "UERG"}
        ],
    }
    assert await link.send_reply(payload) == (200, "ok")
    assert bot.listener.e2ee == [("8@msgr", "gửi bạn file")]
    assert [c[0] for c in bot.listener._bridge.calls] == ["sendE2EEDocument"]  # type: ignore[attr-defined]

    only_file = {
        "account": "acc1",
        "thread_id": "thread:9",
        "text": "",
        "message_id": "r2",
        "attachments": [
            {"name": "p.jpg", "content_type": "image/jpeg", "data": "SlBH"}
        ],
    }
    assert await link.send_reply(only_file) == (200, "ok")
    assert bot.listener.regular == []
    bad = {
        **only_file,
        "message_id": "r3",
        "attachments": [{"name": "x", "data": "@@@"}],
    }
    assert (await link.send_reply(bad))[0] == 400


@pytest.mark.asyncio
async def test_forward_includes_downloaded_attachments(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    sent: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        sent.append(json.loads(request.content))
        return httpx.Response(200, json={})

    async def fake_user_info(
        dataFB: dict[str, Any], user_id: str, **_: Any
    ) -> dict[str, Any]:
        return {"nameUser": "Khách"}

    monkeypatch.setattr(gateway_link._get_user_info, "func", fake_user_info)
    link = _link(handler)
    message = {
        "messageID": "m9",
        "userID": "7",
        "body": "",
        "chatJid": "7@msgr",
        "attachments": [
            {
                "type": "image",
                "directPath": "/p",
                "mediaKey": "aw==",
                "mimeType": "image/jpeg",
                "fileSize": 8,
            },
            {"type": "sticker", "url": "https://sticker"},
        ],
    }
    await link.forward("acc1", _Bot(), message)
    assert sent[0]["text"] == "[sticker] https://sticker"
    assert [a["name"] for a in sent[0]["attachments"]] == ["image.jpg"]
