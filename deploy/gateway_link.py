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
import time
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
NAME_RETRY_SECONDS = 3600.0
MEDIA_RPC_TIMEOUT = 180.0
# Loại attachment của bridge có nội dung tải được; sticker/location/link chỉ mô tả.
DOWNLOADABLE_TYPES = frozenset(
    {"image", "video", "audio", "voice", "file", "gif", "sticker"}
)
# Event phụ (SimpleBot upstream lọc bỏ) cần thành ghi chú nội bộ.
META_EVENT_TYPES = frozenset(
    {"e2eeReaction", "reaction", "messageEdit", "messageUnsend"}
)
ECHO_WINDOW_SECONDS = 60.0
# Bridge gửi tin E2EE không có chữ/file cho loại nó chưa đọc được (danh thiếp,
# bình chọn, xem một lần, vị trí trực tiếp...): báo thay vì bỏ qua im lặng.
UNSUPPORTED_E2EE_TEXT = "❓ Loại tin chưa hỗ trợ: xem trên Messenger"
# Bridge chỉ phát lỗi chung cho tin E2EE không giải mã được; sự kiện raw của
# whatsmeow (UndecryptableMessage) vẫn có chat, người gửi và ID tin.
UNDECRYPTABLE_TEXT = (
    "⚠️ Tin nhắn mã hóa không giải mã được: xem trên Messenger"
    " (tin có thể tự đến sau khi giải mã lại)"
)
OWNER = "Chủ tài khoản"
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


def _rows(table: dict[str, Any], name: str) -> list[dict[str, Any]]:
    rows = table.get(name)
    return [r for r in rows if isinstance(r, dict)] if isinstance(rows, list) else []


def _jid_user(jid: Any) -> str:
    """ "100012.0:3@msgr" -> "100012" (phần user, bỏ agent/thiết bị)."""
    if not isinstance(jid, str):
        return ""
    return jid.split("@", 1)[0].split(":", 1)[0].split(".", 1)[0]


def undecryptable_message(event: dict[str, Any]) -> dict[str, Any] | None:
    """Tin giả cho ``forward`` từ raw UndecryptableMessage; None nếu không cần báo."""
    data = event.get("data")
    if not isinstance(data, dict) or data.get("type") != "UndecryptableMessage":
        return None
    payload = data.get("data")
    info = payload.get("Info") if isinstance(payload, dict) else None
    if not isinstance(info, dict) or info.get("IsFromMe"):
        return None
    # "hide": loại tin client cũng không hiện (reaction, sửa tin... hỏng).
    if payload.get("DecryptFailMode") == "hide":
        return None
    chat, message_id = info.get("Chat"), info.get("ID")
    if not isinstance(chat, str) or "@" not in chat or not message_id:
        return None
    return {
        # ID riêng: nếu sau đó giải mã lại được, tin thật không bị coi là trùng.
        "messageID": f"undecryptable-{message_id}",
        "userID": _jid_user(info.get("Sender")),
        "body": UNDECRYPTABLE_TEXT,
        "chatJid": chat,
        "type": "e2ee",
        "attachments": [],
    }


def thread_ref(message: dict[str, Any]) -> tuple[str, bool] | None:
    """Trả về (thread_id cho gateway, is_group) hoặc None nếu thiếu thông tin."""
    chat_jid = message.get("chatJid")
    if chat_jid:
        # Nhóm E2EE có JID dạng <id>@g.us; chat 1-1 là <id>@msgr.
        return f"e2ee:{chat_jid}", str(chat_jid).endswith("@g.us")
    raw = _raw(message)
    thread_id = raw.get("threadId") or message.get("replyToID")
    if not thread_id:
        return None
    thread_type = raw.get("threadType") or 0
    is_group = bool(thread_type) and thread_type not in ONE_TO_ONE_THREAD_TYPES
    return f"thread:{thread_id}", is_group


def describe_attachment(attachment: dict[str, Any]) -> str:
    """Chữ hiển thị cho attachment không tải được hoặc không có file (vị trí, link)."""
    kind = attachment.get("type") or "attachment"
    lat, lng = attachment.get("latitude"), attachment.get("longitude")
    if kind == "location" and lat is not None and lng is not None:
        label = attachment.get("description") or attachment.get("fileName") or ""
        return "\n".join(
            p
            for p in (
                f"📍 Vị trí{': ' + label if label else ''}",
                f"https://maps.google.com/?q={lat},{lng}",
            )
            if p
        )
    if kind == "link":
        lines = [
            f"🔗 {attachment.get('description') or attachment.get('url') or 'Liên kết'}"
        ]
        lines += [
            str(v) for v in (attachment.get("url"), attachment.get("sourceText")) if v
        ]
        return "\n".join(lines)
    if kind == "sticker":
        return "[Sticker]"
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


def summarize(message: dict[str, Any]) -> str:
    text = " ".join(describe_message(message).split())
    return text if len(text) <= 200 else text[:199] + "…"


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
        # user id -> (tên, ảnh đại diện, lúc tra)
        self._names: dict[str, tuple[str, str, float]] = {}
        self._recent_replies: OrderedDict[str, None] = OrderedDict()
        # messageId -> {summary, sender, thread_id, is_group}: cho trích dẫn E2EE
        # (chỉ có messageId), sửa tin (threadId=0), thu hồi và reaction.
        self._recent: OrderedDict[str, dict[str, Any]] = OrderedDict()
        self._bot_sent: OrderedDict[str, None] = OrderedDict()
        self._pending: list[dict[str, Any]] = []
        # "tài khoản:threadKey" -> {"name", "picture"}: tên/ảnh nhóm đọc từ bảng
        # LightSpeed trong sự kiện raw (tin E2EE không mang tên nhóm).
        self._threads: OrderedDict[str, dict[str, str]] = OrderedDict()
        # "tài khoản:threadKey" -> {contactId: isAdmin}: thành viên nhóm đã thấy, để
        # chỉ báo thay đổi thật chứ không báo cả danh sách mỗi lần đồng bộ.
        self._members: OrderedDict[str, dict[str, bool]] = OrderedDict()

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

    def observe_raw(
        self, account: str, event: dict[str, Any]
    ) -> list[tuple[str, str, str]]:
        """Nhớ tên/ảnh/thành viên nhóm từ bảng LightSpeed (gọi từ listener thread).

        Trả về các thay đổi nhóm ``(threadKey, loại, giá trị)`` để ghi chú. Bảng
        LightSpeed vừa là sự kiện vừa là dữ liệu đồng bộ lúc kết nối, nên chỉ báo
        khi khác trạng thái đã biết: thà bỏ sót còn hơn báo sai cả loạt.
        """
        data = event.get("data")
        if not isinstance(data, dict) or data.get("type") != "LSTable":
            return []
        table = data.get("data")
        if not isinstance(table, dict):
            return []
        changes: list[tuple[str, str, str]] = []
        snapshots = set()
        for row in _rows(table, "LSDeleteThenInsertThread"):
            snapshots.add(str(row.get("ThreadKey") or ""))
            self._remember_thread(
                account,
                row.get("ThreadKey"),
                row.get("ThreadName"),
                row.get("ThreadPictureUrl"),
            )
        for row in _rows(table, "LSSyncUpdateThreadName"):
            key = str(row.get("ThreadKey") or "")
            name = row.get("ThreadName")
            known = self._threads.get(f"{account}:{key}", {}).get("name")
            self._remember_thread(account, key, name, None)
            if known and isinstance(name, str) and name.strip() not in ("", known):
                changes.append((key, "rename", name.strip()))
        for row in _rows(table, "LSSetThreadImageURL"):
            self._remember_thread(
                account, row.get("ThreadKey"), None, row.get("ImageURL")
            )
        added: dict[str, list[dict[str, Any]]] = {}
        for row in _rows(table, "LSAddParticipantIdToGroupThread"):
            added.setdefault(str(row.get("ThreadKey") or ""), []).append(row)
        for key, rows in added.items():
            members = self._thread_members(account, key)
            # Ảnh chụp cả hội thoại hoặc lần đầu thấy nhóm: chỉ ghi nhận.
            report = members is not None and key not in snapshots and len(rows) <= 3
            members = members if members is not None else {}
            for row in rows:
                contact = str(row.get("ContactId") or "")
                if not contact:
                    continue
                if report and contact not in members:
                    changes.append((key, "add", contact))
                members[contact] = bool(row.get("IsAdmin"))
            self._set_thread_members(account, key, members)
        for row in _rows(table, "LSUpdateThreadParticipantAdminStatus"):
            key, contact = str(row.get("ThreadKey") or ""), str(
                row.get("ContactId") or ""
            )
            members = self._thread_members(account, key)
            if members is None or contact not in members:
                continue
            is_admin = bool(row.get("IsAdmin"))
            if members[contact] != is_admin:
                changes.append((key, "admin" if is_admin else "unadmin", contact))
            members[contact] = is_admin
        for row in _rows(table, "LSRemoveParticipantFromThread"):
            key = str(row.get("ThreadKey") or "")
            contact = str(row.get("ParticipantId") or "")
            if not key or not contact:
                continue
            members = self._thread_members(account, key)
            if members is not None:
                members.pop(contact, None)
            changes.append((key, "remove", contact))
        return [c for c in changes if c[0]]

    def _thread_members(self, account: str, key: str) -> dict[str, bool] | None:
        return self._members.get(f"{account}:{key}")

    def _set_thread_members(
        self, account: str, key: str, members: dict[str, bool]
    ) -> None:
        cache_key = f"{account}:{key}"
        self._members[cache_key] = members
        self._members.move_to_end(cache_key)
        while len(self._members) > RECENT_MAX:
            self._members.popitem(last=False)

    async def post_group_changes(
        self, account: str, bot: _Bot, changes: list[tuple[str, str, str]]
    ) -> None:
        """Thay đổi nhóm Facebook thành ghi chú ``group_event``.

        Không biết nhóm là E2EE hay chat thường, nên gửi cho cả hai thread id:
        gateway bỏ qua (202) thread chưa có hội thoại.
        """
        for key, change, value in changes:
            if change == "rename":
                text = f"Nhóm đổi tên thành «{value}»"
            else:
                name = await self._user_name(bot, value) or f"Facebook {value}"
                text = {
                    "add": f"{name} đã vào nhóm",
                    "remove": f"{name} đã rời hoặc bị xoá khỏi nhóm",
                    "admin": f"{name} được làm quản trị viên",
                    "unadmin": f"{name} không còn là quản trị viên",
                }[change]
            minute = int(time.time() // 60)
            for thread_id in (f"e2ee:{key}@g.us", f"thread:{key}"):
                await self._post_event(
                    account,
                    thread_id,
                    True,
                    "group_event",
                    "",
                    text,
                    f"{change}-{key}-{value}-{minute}",
                )

    def _remember_thread(self, account: str, key: Any, name: Any, picture: Any) -> None:
        if not key:
            return
        cache_key = f"{account}:{key}"
        entry = dict(self._threads.get(cache_key) or {})
        if isinstance(name, str) and name.strip():
            if "name" not in entry:
                self.log("gateway", f"Đã biết tên nhóm {key} (từ bảng LightSpeed).")
            entry["name"] = name.strip()
        if isinstance(picture, str) and picture.startswith("https://"):
            entry["picture"] = picture
        if not entry:
            return
        self._threads[cache_key] = entry
        self._threads.move_to_end(cache_key)
        while len(self._threads) > RECENT_MAX:
            self._threads.popitem(last=False)

    def group_info(self, account: str, thread_id: str) -> dict[str, str]:
        """Tên/ảnh nhóm đã biết cho thread id của gateway (e2ee:<id>@g.us hoặc thread:<id>)."""
        key = thread_id.split(":", 1)[-1].split("@", 1)[0]
        return self._threads.get(f"{account}:{key}") or {}

    async def _user_profile(self, bot: _Bot, user_id: str) -> tuple[str, str]:
        """Tên và ảnh đại diện Facebook của user; tra lại sau 1 giờ nếu lần trước
        không có tên (tài khoản không phải bạn bè hoặc ẩn hồ sơ thường không trả tên).
        """
        if not user_id:
            return "", ""
        now = asyncio.get_running_loop().time()
        cached = self._names.get(user_id)
        if cached is None or (not cached[0] and now - cached[2] > NAME_RETRY_SECONDS):
            info = await _get_user_info.func(bot.dataFB, user_id, client=self.http)
            info = info if isinstance(info, dict) else {}
            name, avatar = info.get("nameUser"), info.get("thumbSrc")
            cached = (
                name if isinstance(name, str) else "",
                avatar if isinstance(avatar, str) else "",
                now,
            )
            self._names[user_id] = cached
        return cached[0], cached[1]

    async def _user_name(self, bot: _Bot, user_id: str) -> str:
        return (await self._user_profile(bot, user_id))[0]

    def _remember(self, account: str, message_id: str, entry: dict[str, Any]) -> None:
        # Khoá theo tài khoản: cùng một tin được bot của cả hai bên nhìn thấy.
        if not message_id:
            return
        key = f"{account}:{message_id}"
        self._recent[key] = entry
        self._recent.move_to_end(key)
        while len(self._recent) > RECENT_MAX:
            self._recent.popitem(last=False)

    def _mark_bot_sent(self, message_id: Any) -> None:
        if message_id:
            self._bot_sent[str(message_id)] = None
            while len(self._bot_sent) > RECENT_MAX:
                self._bot_sent.popitem(last=False)

    def is_bot_echo(
        self, account: str, thread_id: str, message: dict[str, Any]
    ) -> bool:
        """Tin của chính tài khoản do bot gửi (qua /send) thì không ghi chú lại."""
        if str(message.get("messageID") or "") in self._bot_sent:
            return True
        now = asyncio.get_running_loop().time()
        self._pending = [p for p in self._pending if p["until"] > now]
        body = message.get("body") if isinstance(message.get("body"), str) else ""
        for pending in self._pending:
            if (
                pending["account"] == account
                and pending["thread_id"] == thread_id
                and (
                    (body and body == pending["text"])
                    or (not body and pending["files"] > 0)
                )
            ):
                self._pending.remove(pending)
                return True
        return False

    def _quote(self, account: str, message: dict[str, Any]) -> str:
        reply = _raw(message).get("replyTo")
        if not isinstance(reply, dict):
            return ""
        cached = self._recent.get(f"{account}:{reply.get('messageId') or ''}")
        text = reply.get("text") or (cached or {}).get("summary") or ""
        sender = (cached or {}).get("sender") or ""
        if not text:
            return "(tin được trả lời không còn trong bộ nhớ của bot)"
        return f"{sender}: {text}" if sender else str(text)

    async def _post_event(
        self,
        account: str,
        thread_id: str,
        is_group: bool,
        kind: str,
        sender: str,
        text: str,
        event_id: str,
        files: list[dict[str, str]] | None = None,
    ) -> None:
        try:
            await self._post(
                "/v1/event",
                {
                    "channel": CHANNEL,
                    "account": account,
                    "thread_id": thread_id,
                    "is_group": is_group,
                    "sender_name": sender,
                    "kind": kind,
                    "text": text,
                    "message_id": f"{kind}-{event_id}" if event_id else "",
                    "attachments": files or [],
                },
            )
        except httpx.HTTPError as error:
            self.log("gateway", f"❌ Không ghi được sự kiện {kind}: {error}")

    async def forward_owner(
        self, account: str, bot: _Bot, message: dict[str, Any]
    ) -> None:
        """Tin chủ tài khoản gửi từ thiết bị khác thành ghi chú nội bộ."""
        ref = thread_ref(message)
        if ref is None:
            return
        thread_id, is_group = ref
        self._remember(
            account,
            str(message.get("messageID") or ""),
            {
                "summary": summarize(message),
                "sender": OWNER,
                "thread_id": thread_id,
                "is_group": is_group,
            },
        )
        if self.is_bot_echo(account, thread_id, message):
            return
        text, files = await self._collect(bot, message)
        if text or files:
            await self._post_event(
                account,
                thread_id,
                is_group,
                "owner_message",
                OWNER,
                text,
                str(message.get("messageID") or ""),
                files,
            )

    async def handle_meta(self, account: str, bot: _Bot, event: dict[str, Any]) -> None:
        """Reaction, sửa tin, thu hồi thành ghi chú nội bộ."""
        raw_data = event.get("data")
        data: dict[str, Any] = raw_data if isinstance(raw_data, dict) else {}
        kind = event.get("type")
        message_id = str(data.get("messageId") or "")
        cached = self._recent.get(f"{account}:{message_id}") or {}
        own_id = str(bot.dataFB.get("FacebookID"))
        if kind in ("e2eeReaction", "reaction"):
            if kind == "e2eeReaction":
                thread_id, is_group = f"e2ee:{data.get('chatJid')}", False
                actor = str(data.get("senderId") or "")
            else:
                thread_id = cached.get("thread_id") or f"thread:{data.get('threadId')}"
                is_group = bool(cached.get("is_group"))
                actor = str(data.get("actorId") or "")
            who = (
                OWNER
                if actor == own_id
                else await self._user_name(bot, actor) or f"Facebook {actor}"
            )
            target = f" vào tin «{cached['summary']}»" if cached.get("summary") else ""
            emoji = data.get("reaction") or "(bỏ cảm xúc)"
            await self._post_event(
                account,
                thread_id,
                is_group,
                "reaction",
                who,
                f"{emoji}{target}",
                f"{message_id}-{actor}-{emoji}",
            )
        elif kind == "messageEdit":
            if not cached.get("thread_id"):
                return  # sửa tin không rõ thread (bot chưa thấy tin gốc)
            new_text = str(data.get("newText") or "")
            text = f"«{cached.get('summary', '')}» → «{new_text}»"
            cached["summary"] = new_text
            await self._post_event(
                account,
                cached["thread_id"],
                bool(cached.get("is_group")),
                "edit",
                cached.get("sender", ""),
                text,
                f"{message_id}-{data.get('editCount', '')}",
            )
        elif kind == "messageUnsend":
            raw_thread = str(data.get("threadId") or "")
            thread_id = cached.get("thread_id") or (
                f"e2ee:{raw_thread}" if data.get("isE2EE") else f"thread:{raw_thread}"
            )
            text = (
                f"«{cached['summary']}»"
                if cached.get("summary")
                else "(nội dung tin không còn trong bộ nhớ của bot)"
            )
            await self._post_event(
                account,
                thread_id,
                bool(cached.get("is_group")),
                "recall",
                cached.get("sender", ""),
                text,
                message_id,
            )

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
        if not parts and not files and message.get("type") == "e2ee":
            parts.append(UNSUPPORTED_E2EE_TEXT)
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
        user_id = str(message.get("userID") or "")
        # Không tra được tên (ẩn hồ sơ / không phải bạn bè): dùng nhãn dễ đọc.
        name, avatar = await self._user_profile(bot, user_id)
        sender_name = name or (f"Facebook {user_id}" if user_id else "")
        self._remember(
            account,
            str(message.get("messageID") or ""),
            {
                "summary": summarize(message),
                "sender": sender_name,
                "thread_id": thread_id,
                "is_group": is_group,
            },
        )
        raw = _raw(message)
        group = self.group_info(account, thread_id) if is_group else {}
        # Nhóm chưa biết tên: để trống, gateway đặt tên chung (không lấy tên người gửi).
        thread_name = raw.get("threadName") or (
            group.get("name", "") if is_group else sender_name
        )
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
                    "quote": self._quote(account, message),
                    # Nhóm Facebook: chưa có API ảnh nhóm, Libredesk dùng chữ viết tắt.
                    "thread_avatar": group.get("picture", "") if is_group else avatar,
                    "sender_avatar": avatar,
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
        account = str(payload.get("account") or "")
        bot = self.bots.get(account)
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
        pending = {
            "account": str(payload.get("account")),
            "thread_id": thread_id,
            "text": text,
            "files": len(files),
            "until": asyncio.get_running_loop().time() + ECHO_WINDOW_SECONDS,
        }
        self._pending.append(pending)
        try:
            if text.strip() and kind == "e2ee":
                result = await bot.listener.send_e2ee_message(target, text)
            elif text.strip():
                result = await bot.listener.send_message(int(target), text)
            self._after_send(account, result, thread_id, text)
            for f in files:
                method, params = media_rpc(kind, target, f)
                assert bridge is not None
                result = await bridge.call(method, params, MEDIA_RPC_TIMEOUT)
                self._after_send(
                    account, result, thread_id, f"[Tệp] {f.get('name', '')}"
                )
        except Exception as error:  # lỗi bridge/mạng: để gateway thử lại
            self._recent_replies.pop(message_id, None)
            if pending in self._pending:
                self._pending.remove(pending)
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

    def _after_send(
        self, account: str, result: dict[str, Any], thread_id: str, summary: str
    ) -> None:
        sent_id = result.get("messageId") or result.get("id")
        self._mark_bot_sent(sent_id)
        if sent_id and summary.strip():
            self._remember(
                account,
                str(sent_id),
                {
                    "summary": summary[:200],
                    "sender": "Nhân viên",
                    "thread_id": thread_id,
                    "is_group": False,
                },
            )

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
