"""Nối bot Facebook với omnichannel gateway (Libredesk).

- Tin khách gửi tới: ``POST {GATEWAY_URL}/v1/inbound``.
- Tin nhân viên trả lời: gateway gọi ``POST /send`` trên server HTTP nội bộ này.

``thread_id`` gửi cho gateway mang tiền tố để biết đường trả lời:
``e2ee:<chatJid>`` (chat mã hóa) hoặc ``thread:<threadId>`` (chat thường).
File đính kèm đi kèm dạng base64 ở cả hai chiều; bot tải/gửi file qua RPC của
bridge (``downloadE2EEMedia``/``downloadMedia``, ``sendE2EE*``/``sendImage``/
``sendFile``). Chỉ bật khi có ``GATEWAY_URL``; token qua ``GATEWAY_TOKEN``.
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import hmac
import json
import os
from collections import OrderedDict
from collections.abc import Callable
from typing import Any, Protocol

import httpx

from _features._facebook import _get_user_info

CHANNEL = "facebook"
# Messenger ThreadType (mautrix-meta table.ThreadType) của chat 1-1.
ONE_TO_ONE_THREAD_TYPES = frozenset({1, 7, 10, 13, 14, 15})
MAX_REQUEST_BYTES = 96 << 20  # base64 của vài file 20 MiB
MAX_FILE_BYTES = int(os.environ.get("GATEWAY_MAX_FILE_MB", "20")) << 20
RECENT_MAX = 4096
MEDIA_RPC_TIMEOUT = 180.0
# Loại attachment của bridge có nội dung tải được; sticker/location/link chỉ mô tả.
DOWNLOADABLE_TYPES = frozenset({"image", "video", "audio", "voice", "file", "gif"})
# mediaType cho downloadE2EEMedia (bridge nhận image/video/audio/voice/file/sticker).
E2EE_MEDIA_TYPE = {"gif": "video"}
EXT_BY_MIME = {
    "image/jpeg": "jpg",
    "image/png": "png",
    "image/gif": "gif",
    "image/webp": "webp",
    "video/mp4": "mp4",
    "audio/mpeg": "mp3",
    "audio/mp4": "m4a",
    "audio/ogg": "ogg",
    "application/pdf": "pdf",
}


class _Bridge(Protocol):
    async def call(
        self, method: str, params: dict[str, Any] | None = None, timeout: float = 60.0
    ) -> dict[str, Any]: ...


class _Listener(Protocol):
    async def send_e2ee_message(
        self,
        chat_jid: str,
        text: str,
        reply_to_id: str = "",
        reply_to_sender_jid: str = "",
    ) -> dict[str, Any]: ...

    async def send_message(
        self, thread_id: int, text: str, reply_to_id: str = ""
    ) -> dict[str, Any]: ...


class _Bot(Protocol):
    @property
    def dataFB(self) -> dict[str, Any]: ...

    @property
    def listener(self) -> _Listener: ...


def _raw(message: dict[str, Any]) -> dict[str, Any]:
    raw = message.get("raw")
    return raw if isinstance(raw, dict) else {}


def thread_ref(message: dict[str, Any]) -> tuple[str, bool] | None:
    """Trả về (thread_id cho gateway, is_group) hoặc None nếu thiếu thông tin."""
    chat_jid = message.get("chatJid")
    if chat_jid:
        return f"e2ee:{chat_jid}", False
    raw = _raw(message)
    thread_id = raw.get("threadId") or message.get("replyToID")
    if not thread_id:
        return None
    thread_type = raw.get("threadType") or 0
    is_group = bool(thread_type) and thread_type not in ONE_TO_ONE_THREAD_TYPES
    return f"thread:{thread_id}", is_group


def describe_attachment(attachment: dict[str, Any]) -> str:
    kind = attachment.get("type") or "attachment"
    detail = attachment.get("fileName") or attachment.get("url") or ""
    return f"[{kind}] {detail}".strip()


def describe_message(message: dict[str, Any]) -> str:
    """Nội dung chữ, kèm mô tả ngắn cho file đính kèm."""
    parts = []
    body = message.get("body")
    if isinstance(body, str) and body.strip():
        parts.append(body)
    for attachment in message.get("attachments") or []:
        if isinstance(attachment, dict):
            parts.append(describe_attachment(attachment))
    return "\n".join(parts)


def _bridge_of(bot: Any) -> _Bridge | None:
    # SimpleBot upstream cũng dùng listener._bridge cho BridgeActions.
    return getattr(bot.listener, "_bridge", None)


def file_name(name: Any, mime: str, fallback: str) -> str:
    clean = str(name or "").replace("/", "_").replace("\\", "_").strip()[:120]
    if "." in clean:
        return clean
    return f"{clean or fallback}.{EXT_BY_MIME.get(mime.split(';')[0], 'bin')}"


async def fetch_attachment(
    bridge: _Bridge, attachment: dict[str, Any]
) -> dict[str, str] | None:
    """Tải nội dung một attachment qua bridge; None nếu loại này không tải được."""
    kind = str(attachment.get("type") or "")
    if kind not in DOWNLOADABLE_TYPES:
        return None
    if int(attachment.get("fileSize") or 0) > MAX_FILE_BYTES:
        raise ValueError("file quá lớn")
    mime = str(attachment.get("mimeType") or "application/octet-stream")
    if attachment.get("directPath") and attachment.get("mediaKey"):
        result = await bridge.call(
            "downloadE2EEMedia",
            {
                "directPath": attachment["directPath"],
                "mediaKey": attachment["mediaKey"],
                "mediaSha256": attachment.get("mediaSha256") or "",
                "mediaEncSha256": attachment.get("mediaEncSha256") or "",
                "mediaType": E2EE_MEDIA_TYPE.get(kind, kind),
                "mimeType": mime,
                "fileSize": int(attachment.get("fileSize") or 0),
            },
            MEDIA_RPC_TIMEOUT,
        )
        mime = str(result.get("mimeType") or mime)
    elif attachment.get("url"):
        result = await bridge.call(
            "downloadMedia", {"url": attachment["url"]}, MEDIA_RPC_TIMEOUT
        )
    else:
        return None
    data = str(result.get("data") or "")
    size = len(base64.b64decode(data, validate=True))
    if size == 0 or size > MAX_FILE_BYTES:
        raise ValueError("file rỗng hoặc quá lớn")
    return {
        "name": file_name(attachment.get("fileName"), mime, kind),
        "content_type": mime,
        "data": data,
    }


def media_rpc(
    kind: str, target: str, file: dict[str, Any]
) -> tuple[str, dict[str, Any]]:
    """Chọn RPC gửi file theo loại chat và MIME type; data giữ nguyên base64."""
    mime = str(file.get("content_type") or "application/octet-stream")
    name = file_name(file.get("name"), mime, "file")
    data = str(file.get("data") or "")
    if kind == "e2ee":
        common = {"chatJid": target, "data": data, "mimeType": mime}
        reply = {"replyToId": "", "replyToSenderJid": ""}
        if mime.startswith("image/"):
            return "sendE2EEImage", {
                **common,
                "caption": "",
                "width": 0,
                "height": 0,
                **reply,
            }
        if mime.startswith("video/"):
            return "sendE2EEVideo", {
                **common,
                "caption": "",
                "width": 0,
                "height": 0,
                "duration": 0,
                **reply,
            }
        if mime.startswith("audio/"):
            return "sendE2EEAudio", {**common, "duration": 0, "ptt": False, **reply}
        return "sendE2EEDocument", {**common, "filename": name, **reply}
    thread = {
        "threadId": int(target),
        "data": data,
        "filename": name,
        "caption": "",
        "replyToId": "",
    }
    if mime.startswith("image/"):
        return "sendImage", thread
    return "sendFile", {**thread, "mimeType": mime}


class GatewayLink:
    def __init__(
        self,
        url: str,
        token: str,
        http_client: httpx.AsyncClient,
        log: Callable[[str, str], None],
    ) -> None:
        self.url = url.rstrip("/")
        self.token = token
        self.http = http_client
        self.log = log
        self.bots: dict[str, _Bot] = {}
        self._names: dict[str, str] = {}
        self._recent_replies: OrderedDict[str, None] = OrderedDict()

    @classmethod
    def from_env(
        cls, http_client: httpx.AsyncClient, log: Callable[[str, str], None]
    ) -> GatewayLink | None:
        url = os.environ.get("GATEWAY_URL", "")
        if not url:
            return None
        token = os.environ.get("GATEWAY_TOKEN", "")
        if len(token) < 32:
            raise RuntimeError("GATEWAY_TOKEN phải dài ít nhất 32 ký tự.")
        return cls(url, token, http_client, log)

    async def _post(self, path: str, body: dict[str, Any]) -> None:
        # File 20 MiB: gateway còn upload tiếp vào Libredesk trước khi trả lời.
        timeout = 120 if body.get("attachments") else 30
        response = await self.http.post(
            self.url + path,
            json=body,
            headers={"Authorization": f"Bearer {self.token}"},
            timeout=timeout,
        )
        response.raise_for_status()

    async def register(self, account: str, bot: _Bot) -> None:
        self.bots[account] = bot
        try:
            await self._post("/v1/register", {"channel": CHANNEL, "account": account})
        except httpx.HTTPError as error:
            # Gateway tự tạo inbox ở tin nhắn đầu tiên nên không chặn bot.
            self.log("gateway", f"Đăng ký inbox thất bại: {error}")

    def unregister(self, account: str, bot: _Bot) -> None:
        if self.bots.get(account) is bot:
            del self.bots[account]

    async def _user_name(self, bot: _Bot, user_id: str) -> str:
        if not user_id:
            return ""
        if user_id not in self._names:
            info = await _get_user_info.func(bot.dataFB, user_id, client=self.http)
            name = info.get("nameUser") if isinstance(info, dict) else None
            self._names[user_id] = name if isinstance(name, str) else ""
        return self._names[user_id]

    async def _collect(
        self, bot: _Bot, message: dict[str, Any]
    ) -> tuple[str, list[dict[str, str]]]:
        """Chữ của tin + file tải được; file lỗi/không tải được thành dòng mô tả."""
        body = message.get("body")
        parts = [body] if isinstance(body, str) and body.strip() else []
        files: list[dict[str, str]] = []
        bridge = _bridge_of(bot)
        for attachment in message.get("attachments") or []:
            if not isinstance(attachment, dict):
                continue
            file = None
            if bridge is not None:
                try:
                    file = await fetch_attachment(bridge, attachment)
                except Exception as error:  # bridge/mạng/dữ liệu: vẫn báo bằng chữ
                    self.log("gateway", f"Không tải được file đính kèm: {error}")
            if file is not None:
                files.append(file)
            else:
                parts.append(describe_attachment(attachment))
        return "\n".join(parts), files

    async def forward(self, account: str, bot: _Bot, message: dict[str, Any]) -> None:
        """Chuyển một tin khách sang gateway; lỗi chỉ được ghi log."""
        ref = thread_ref(message)
        if ref is None:
            return
        text, files = await self._collect(bot, message)
        if not text and not files:
            return
        thread_id, is_group = ref
        sender_name = await self._user_name(bot, str(message.get("userID") or ""))
        raw = _raw(message)
        thread_name = raw.get("threadName") or ("" if is_group else sender_name)
        try:
            await self._post(
                "/v1/inbound",
                {
                    "channel": CHANNEL,
                    "account": account,
                    "thread_id": thread_id,
                    "is_group": is_group,
                    "thread_name": thread_name,
                    "sender_name": sender_name,
                    "text": text,
                    "message_id": str(message.get("messageID") or ""),
                    "attachments": files,
                },
            )
        except httpx.HTTPError as error:
            self.log("gateway", f"❌ Không chuyển được tin tới gateway: {error}")

    def _seen_reply(self, message_id: str) -> bool:
        if not message_id:
            return False
        if message_id in self._recent_replies:
            return True
        self._recent_replies[message_id] = None
        if len(self._recent_replies) > RECENT_MAX:
            self._recent_replies.popitem(last=False)
        return False

    async def send_reply(self, payload: dict[str, Any]) -> tuple[int, str]:
        """Gửi tin trả lời của nhân viên (chữ và/hoặc file); trả về (status, mô tả)."""
        bot = self.bots.get(str(payload.get("account") or ""))
        if bot is None:
            return 404, "unknown account"
        thread_id = payload.get("thread_id")
        raw_text = payload.get("text")
        text = raw_text if isinstance(raw_text, str) else ""
        files = [
            f
            for f in (payload.get("attachments") or [])
            if isinstance(f, dict) and isinstance(f.get("data"), str) and f["data"]
        ]
        if not isinstance(thread_id, str) or (not text.strip() and not files):
            return 400, "invalid payload"
        try:
            for f in files:
                base64.b64decode(f["data"], validate=True)
        except (binascii.Error, ValueError):
            return 400, "invalid attachment data"
        kind, _, target = thread_id.partition(":")
        if not ((kind == "e2ee" and target) or (kind == "thread" and target.isdigit())):
            return 400, "invalid thread_id"
        message_id = str(payload.get("message_id") or "")
        if self._seen_reply(message_id):
            return 200, "duplicate"
        bridge = _bridge_of(bot)
        if files and bridge is None:
            self._recent_replies.pop(message_id, None)
            return 502, "bridge not ready"
        result: dict[str, Any] = {}
        try:
            if text.strip() and kind == "e2ee":
                result = await bot.listener.send_e2ee_message(target, text)
            elif text.strip():
                result = await bot.listener.send_message(int(target), text)
            for f in files:
                method, params = media_rpc(kind, target, f)
                assert bridge is not None
                result = await bridge.call(method, params, MEDIA_RPC_TIMEOUT)
        except Exception as error:  # lỗi bridge/mạng: để gateway thử lại
            self._recent_replies.pop(message_id, None)
            self.log("gateway", f"❌ Gửi tin trả lời thất bại: {type(error).__name__}")
            return 502, "send failed"
        if not (result.get("messageId") or result.get("id")):
            # Có thể đã gửi nhưng không xác nhận (deliveryStatus UNKNOWN): không
            # trả lỗi để gateway không gửi lại, tránh khách nhận trùng.
            self.log("gateway", "Gửi tin trả lời chưa được xác nhận; không thử lại.")
        else:
            suffix = f" ({len(files)} tệp)" if files else ""
            self.log("gateway", f"Đã gửi tin trả lời của nhân viên{suffix}.")
        return 200, "ok"

    async def _handle_connection(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        status, text = 500, "error"
        try:
            request_line = await asyncio.wait_for(reader.readline(), timeout=10)
            headers: dict[str, str] = {}
            while True:
                line = await asyncio.wait_for(reader.readline(), timeout=10)
                if line in (b"\r\n", b"\n", b""):
                    break
                name, _, value = line.decode("latin-1").partition(":")
                headers[name.strip().lower()] = value.strip()
            method, path, *_ = request_line.decode("latin-1").split()
            length = int(headers.get("content-length") or 0)
            if method != "POST" or path != "/send":
                status, text = 404, "not found"
            elif not hmac.compare_digest(
                headers.get("authorization", ""), f"Bearer {self.token}"
            ):
                status, text = 401, "unauthorized"
            elif length > MAX_REQUEST_BYTES:
                status, text = 413, "too large"
            else:
                body = await asyncio.wait_for(reader.readexactly(length), timeout=10)
                try:
                    payload = json.loads(body)
                except ValueError:
                    status, text = 400, "invalid json"
                else:
                    if isinstance(payload, dict):
                        status, text = await self.send_reply(payload)
                    else:
                        status, text = 400, "invalid json"
        except (ValueError, asyncio.TimeoutError, asyncio.IncompleteReadError):
            status, text = 400, "bad request"
        finally:
            data = text.encode()
            writer.write(
                f"HTTP/1.1 {status} X\r\nContent-Type: text/plain; charset=utf-8\r\n"
                f"Content-Length: {len(data)}\r\nConnection: close\r\n\r\n".encode()
                + data
            )
            try:
                await writer.drain()
            finally:
                writer.close()

    async def serve(self, port: int) -> asyncio.Server:
        server = await asyncio.start_server(self._handle_connection, "0.0.0.0", port)
        self.log("gateway", f"Nhận tin trả lời tại :{port}/send")
        return server
