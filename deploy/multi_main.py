"""Chạy nhiều tài khoản trong một tiến trình Python.

File thuộc phần deploy của fork: chỉ dùng API sẵn có của ``src/main.py`` (kế
thừa ``SimpleBot``, bọc ``main.log``) để không phải sửa file upstream và tránh
conflict khi đồng bộ. ``deploy/tests`` kiểm tra các điểm bám này.

Mỗi tài khoản là một thư mục con của ``FBCHAT_ACCOUNTS_DIR`` (mặc định
``deploy/accounts``) chứa ``config.json`` cùng schema với ``src/config.json``.
Bridge E2EE vẫn là một tiến trình Go riêng cho mỗi tài khoản (bridge chỉ hỗ
trợ một client), nhưng interpreter Python, event loop và ``httpx.AsyncClient``
được dùng chung. Nếu bật lưu trạng thái E2EE ra file, mỗi tài khoản dùng
``<accounts>/<tên>/e2ee_device.json`` riêng.

Tài khoản lỗi được thử lại với backoff và không làm dừng tài khoản khác.
"""

from __future__ import annotations

import asyncio
import contextlib
import contextvars
import gc
import json
import os
import re
import shutil
import tempfile
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx

import main as upstream_main
from _core._console import configure_utf8_console
from _core._permissions import set_private_file_permissions
from _core._session import dataGetHome
from _core._storage import FileSessionStorage
from _messaging._listening_e2ee import _format_exception_chain
from main import DEFAULT_HTTP_TIMEOUT, SimpleBot, is_valid_datafb

from gateway_link import META_EVENT_TYPES, GatewayLink, undecryptable_message

ACCOUNTS_DIR = Path(
    os.environ.get("FBCHAT_ACCOUNTS_DIR", Path(__file__).resolve().parent / "accounts")
)
ACCOUNT_NAME_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$")
MAX_ATTEMPTS = 5
RETRY_BASE_SECONDS = 30.0
RETRY_MAX_SECONDS = 600.0
STABLE_AFTER_SECONDS = 600.0
# File trạng thái cho Docker healthcheck; healthy khi mọi tài khoản là "running".
HEALTH_FILE = os.environ.get("FBCHAT_HEALTH_FILE")
BOT_HTTP_PORT = int(os.environ.get("BOT_HTTP_PORT", "7002"))
E2EE_PERSIST = os.environ.get("FBCHAT_E2EE_PERSIST", "1").lower() not in {
    "0",
    "false",
    "no",
}
# Có GATEWAY_URL: tin khách được chuyển tới omnichannel gateway và lệnh /ping...
# bị tắt (đặt BOT_COMMANDS=1 để bật lại).
COMMANDS_FLAG = os.environ.get("BOT_COMMANDS", "").lower() in {"1", "true", "yes"}
_gateway: GatewayLink | None = None

_account_states: dict[str, str] = {}
# tên tài khoản -> {"uid", "displayName", "avatar"}: hiển thị ở trang "Tài khoản kênh".
_account_profiles: dict[str, dict[str, str]] = {}

# Tên tài khoản gắn vào log; task con và asyncio.to_thread kế thừa context.
LOG_LABEL: contextvars.ContextVar[str] = contextvars.ContextVar(
    "fbchat_log_label", default=""
)
_upstream_log = upstream_main.log


def log(tag: str, message: str) -> None:
    label = LOG_LABEL.get()
    # main.log in "[giờ] [tag] nội dung"; nhét nhãn vào tag để có "[acc] [tag]".
    _upstream_log(f"{label}] [{tag}" if label else tag, message)


def install_labeled_log() -> None:
    """Cho SimpleBot dùng log có nhãn: hàm của nó tra ``log`` trong main lúc gọi."""
    upstream_main.log = log


def load_account_config(path: Path) -> dict[str, Any]:
    """Đọc config.json của một tài khoản; cùng quy tắc với main.load_config()."""
    set_private_file_permissions(path)
    with path.open("r", encoding="utf-8") as file_handle:
        config = json.load(file_handle)
    if not isinstance(config, dict):
        raise ValueError(f"{path.name} phải là một JSON object.")
    config.setdefault("prefix", "/")
    config.setdefault("admins", [])
    config.setdefault("log_message_content", False)
    config.setdefault("debug_errors", False)
    if not isinstance(config["prefix"], str) or not config["prefix"]:
        raise ValueError("config.prefix phải là chuỗi không rỗng.")
    if not isinstance(config["admins"], list):
        raise ValueError("config.admins phải là một danh sách ID.")
    for flag in ("log_message_content", "debug_errors"):
        if not isinstance(config[flag], bool):
            raise ValueError(f"config.{flag} phải là true hoặc false.")
    return config


class AccountBot(SimpleBot):
    """SimpleBot cho một tài khoản trong tiến trình nhiều tài khoản."""

    def __init__(
        self, dataFB: dict[str, Any], *, account: str, device_path: str, **options: Any
    ) -> None:
        super().__init__(dataFB, **options)
        self.account = account
        # Listener chỉ đọc hai thuộc tính này khi connect. Upstream mặc định giữ
        # trạng thái E2EE trong RAM: mỗi lần restart là một thiết bị E2EE mới và
        # tin đến hay báo "could not be decrypted". Lưu ra file (0600) theo tài khoản.
        self.listener.device_path = device_path
        self.listener.e2ee_memory_only = not E2EE_PERSIST

    async def _dispatch(self, message: dict[str, Any]) -> None:
        if _gateway is not None:
            sender_id = str(message.get("userID") or "")
            if message.get("messageID"):
                if sender_id == str(self.dataFB.get("FacebookID")):
                    # Chủ tài khoản gửi từ thiết bị khác: ghi chú, không phải tin khách.
                    await _gateway.forward_owner(self.account, self, message)
                else:
                    await _gateway.forward(self.account, self, message)
            if not COMMANDS_FLAG:
                return
        await super()._dispatch(message)

    def _forward_listener_event(
        self, loop: asyncio.AbstractEventLoop, event: dict[str, Any]
    ) -> None:
        # SimpleBot chỉ chuyển tin nhắn/điều khiển; reaction, sửa, thu hồi thì
        # chuyển thẳng cho gateway (chạy trên event loop, gọi từ listener thread).
        if (
            _gateway is not None
            and isinstance(event, dict)
            and event.get("type") == "raw"
        ):
            # Bảng LightSpeed mang tên/ảnh/thành viên nhóm (tin E2EE thì không).
            changes = _gateway.observe_raw(self.account, event)
            lost = undecryptable_message(event)
            try:
                if changes:
                    asyncio.run_coroutine_threadsafe(
                        _gateway.post_group_changes(self.account, self, changes), loop
                    )
                if lost is not None:
                    asyncio.run_coroutine_threadsafe(
                        _gateway.forward(self.account, self, lost), loop
                    )
            except RuntimeError:
                pass  # loop đã đóng khi đang tắt
        if (
            _gateway is not None
            and isinstance(event, dict)
            and event.get("type") in META_EVENT_TYPES
        ):
            try:
                asyncio.run_coroutine_threadsafe(
                    _gateway.handle_meta(self.account, self, event), loop
                )
            except RuntimeError:
                pass  # loop đã đóng khi đang tắt
            return
        super()._forward_listener_event(loop, event)

    def _queue_event(self, event: dict[str, Any]) -> None:
        # call_soon_threadsafe chạy callback trong bản sao context của listener
        # thread, nên phải gắn lại nhãn; không ảnh hưởng task khác.
        LOG_LABEL.set(self.account)
        super()._queue_event(event)


def set_account_state(name: str, state: str | None) -> None:
    """Ghi nhận starting/running/retrying/dropped (None: xoá) và ghi file health."""
    if state is None:
        _account_states.pop(name, None)
    else:
        _account_states[name] = state
    if not HEALTH_FILE:
        return
    target = Path(HEALTH_FILE)
    try:
        fd, tmp_name = tempfile.mkstemp(dir=target.parent, suffix=".tmp")
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(_account_states, handle)
        os.replace(tmp_name, target)
    except OSError as error:
        log("multi", f"Không ghi được file health: {error}")


def discover_accounts(root: Path) -> list[tuple[str, Path]]:
    """Trả về (tên, thư mục) của mọi tài khoản có config.json, sắp theo tên."""
    if not root.is_dir():
        return []
    accounts: list[tuple[str, Path]] = []
    for entry in sorted(root.iterdir()):
        if not entry.is_dir() or not (entry / "config.json").is_file():
            continue
        if not ACCOUNT_NAME_PATTERN.match(entry.name):
            log(
                "multi", f"Bỏ qua thư mục tài khoản có tên không hợp lệ: {entry.name!r}"
            )
            continue
        accounts.append((entry.name, entry))
    return accounts


async def run_account(
    name: str,
    account_dir: Path,
    http_client: httpx.AsyncClient,
    *,
    on_ready: Callable[[], None] | None = None,
) -> None:
    """Khởi tạo session và chạy bot cho một tài khoản tới khi lỗi hoặc bị hủy."""
    LOG_LABEL.set(name)
    config_path = account_dir / "config.json"
    config = load_account_config(config_path)
    log("boot", "Đang khởi tạo dataFB từ cookie...")
    dataFB = await dataGetHome(
        storage=FileSessionStorage(str(config_path), key="cookies")
    )
    # httpx.Response và stream của nó tham chiếu vòng nên HTML homepage (vài MB,
    # ~18 MiB sau khi decode) chỉ được giải phóng khi GC thế hệ cao nhất chạy;
    # bot rảnh có thể giữ nó rất lâu. Dọn ngay để không nhân lên theo số tài khoản.
    gc.collect()
    if dataFB is None or not is_valid_datafb(dataFB):
        raise RuntimeError(
            "Không lấy được dataFB hợp lệ; cookie đã hết hạn hoặc HTML token đã đổi."
        )
    bot = AccountBot(
        dataFB,
        account=name,
        device_path=str(account_dir / "e2ee_device.json"),
        prefix=config["prefix"],
        admins=config["admins"],
        log_message_content=config["log_message_content"],
        debug_errors=config["debug_errors"],
        http_client=http_client,
    )
    if _gateway is not None:
        await _gateway.register(name, bot)
    uid = str(dataFB.get("FacebookID") or "")
    _account_profiles[name] = {"uid": uid, "displayName": "", "avatar": ""}
    if _gateway is not None and uid:
        with contextlib.suppress(Exception):  # chỉ để hiển thị
            display_name, avatar = await _gateway.user_profile(bot, uid)
            _account_profiles[name].update(displayName=display_name, avatar=avatar)
    ready_task = asyncio.create_task(_notify_when_ready(bot, on_ready))
    try:
        await bot.run()
    finally:
        ready_task.cancel()
        if _gateway is not None:
            _gateway.unregister(name, bot)


async def _notify_when_ready(
    bot: SimpleBot, on_ready: Callable[[], None] | None
) -> None:
    """Chờ cùng điều kiện ready với SimpleBot.run() rồi báo cho supervisor."""
    if on_ready is None:
        return
    ready = await asyncio.to_thread(
        bot.listener.wait_until_connected,
        bot.listener.startup_timeout,
        require_e2ee=True,
    )
    if ready:
        on_ready()


async def supervise_account(
    name: str,
    account_dir: Path,
    http_client: httpx.AsyncClient,
    *,
    max_attempts: int = MAX_ATTEMPTS,
    retry_base: float = RETRY_BASE_SECONDS,
    stable_after: float = STABLE_AFTER_SECONDS,
) -> bool:
    """Chạy lại tài khoản khi lỗi; trả về False khi bỏ cuộc sau max_attempts.

    Bộ đếm reset nếu lần chạy trước kéo dài quá ``stable_after`` giây, để bot
    chạy lâu ngày không bị bỏ chỉ vì thỉnh thoảng mất kết nối.
    """
    LOG_LABEL.set(name)
    loop = asyncio.get_running_loop()
    attempt = 0
    while True:
        started = loop.time()
        set_account_state(name, "starting")
        try:
            await run_account(
                name,
                account_dir,
                http_client,
                on_ready=lambda: set_account_state(name, "running"),
            )
            return True
        except Exception as error:
            # Bắt rộng để lỗi của một tài khoản không làm dừng tài khoản khác;
            # CancelledError là BaseException nên vẫn được lan ra khi shutdown.
            attempt = 1 if loop.time() - started >= stable_after else attempt + 1
            if attempt >= max_attempts:
                set_account_state(name, "dropped")
                log(
                    "multi",
                    f"❌ Bỏ tài khoản sau {attempt} lần lỗi liên tiếp: "
                    f"{_format_exception_chain(error)}",
                )
                return False
            set_account_state(name, "retrying")
            delay = min(retry_base * 2 ** (attempt - 1), RETRY_MAX_SECONDS)
            log(
                "multi",
                f"❌ {_format_exception_chain(error)}; thử lại sau {delay:.0f}s "
                f"({attempt}/{max_attempts}).",
            )
            await asyncio.sleep(delay)


class AccountManager:
    """Chạy/dừng từng tài khoản khi bot đang chạy (trang "Tài khoản kênh")."""

    def __init__(self, root: Path, http_client: httpx.AsyncClient) -> None:
        self.root = root
        self.http_client = http_client
        self.tasks: dict[str, asyncio.Task[bool]] = {}

    def start(self, name: str) -> asyncio.Task[bool]:
        task = self.tasks.get(name)
        if task is not None and not task.done():
            return task
        task = asyncio.create_task(
            supervise_account(name, self.root / name, self.http_client),
            name=f"account-{name}",
        )
        self.tasks[name] = task
        return task

    async def stop(self, name: str) -> None:
        task = self.tasks.pop(name, None)
        if task is not None and not task.done():
            # Hủy task: SimpleBot.run() dừng bridge trong finally.
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await task
        set_account_state(name, None)
        _account_profiles.pop(name, None)

    async def restart(self, name: str) -> None:
        await self.stop(name)
        self.start(name)

    def list(self) -> list[dict[str, Any]]:
        names = {n for n, _ in discover_accounts(self.root)} | set(_account_states)
        return [
            {
                "name": n,
                "state": _account_states.get(n, "stopped"),
                "profile": _account_profiles.get(n),
            }
            for n in sorted(names)
        ]

    async def set_cookie(self, name: str, cookie: str) -> tuple[int, str]:
        """Kiểm tra cookie với Facebook rồi lưu và chạy lại tài khoản."""
        names = {p.partition("=")[0].strip() for p in cookie.split(";") if "=" in p}
        if not {"c_user", "xs"} <= names:
            return 400, "Cookie phải có c_user và xs."
        data_fb = await dataGetHome(cookie)
        gc.collect()
        if data_fb is None or not is_valid_datafb(data_fb):
            return 422, "Facebook không chấp nhận cookie này (hết hạn hoặc sai)."
        account_dir = self.root / name
        account_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        config_path = account_dir / "config.json"
        FileSessionStorage(str(config_path), key="cookies").save(cookie.strip())
        set_private_file_permissions(config_path)
        log("multi", f"Đã lưu cookie mới cho tài khoản {name}.")
        await self.restart(name)
        return 200, "ok"

    async def remove(self, name: str) -> None:
        await self.stop(name)
        shutil.rmtree(self.root / name, ignore_errors=True)
        log("multi", f"Đã gỡ tài khoản {name} và xoá dữ liệu đăng nhập.")

    async def handle(
        self, method: str, path: str, body: bytes
    ) -> tuple[int, Any] | None:
        """Route /accounts cho gateway; None nếu không phải route này."""
        parts = [p for p in path.split("?", 1)[0].split("/") if p]
        if not parts or parts[0] != "accounts":
            return None
        if len(parts) == 1 and method == "GET":
            return 200, self.list()
        name = parts[1] if len(parts) > 1 else ""
        if not ACCOUNT_NAME_PATTERN.match(name):
            return 400, {"error": "Tên tài khoản không hợp lệ."}
        action = parts[2] if len(parts) > 2 else ""
        if action == "cookie" and method == "POST":
            try:
                payload = json.loads(body or b"{}")
            except ValueError:
                return 400, {"error": "JSON không hợp lệ."}
            cookie = payload.get("cookie") if isinstance(payload, dict) else None
            if not isinstance(cookie, str) or not cookie.strip():
                return 400, {"error": "Thiếu cookie."}
            status, message = await self.set_cookie(name, cookie)
            return status, {"ok": status == 200, "message": message}
        if action == "restart" and method == "POST":
            if not (self.root / name / "config.json").is_file():
                return 404, {"error": "Chưa có tài khoản này."}
            await self.restart(name)
            return 202, {"ok": True}
        if action == "" and method == "DELETE":
            await self.remove(name)
            return 204, None
        return 404, {"error": "not found"}


async def main() -> None:
    install_labeled_log()
    accounts = discover_accounts(ACCOUNTS_DIR)
    log(
        "multi",
        f"Chạy {len(accounts)} tài khoản: "
        f"{', '.join(n for n, _ in accounts) or '(chưa có)'}",
    )
    global _gateway
    async with httpx.AsyncClient(timeout=DEFAULT_HTTP_TIMEOUT) as http_client:
        _gateway = GatewayLink.from_env(http_client, log)
        if _gateway is None and not accounts:
            raise RuntimeError(
                f"Không có tài khoản nào trong {ACCOUNTS_DIR} (cần <tên>/config.json)."
            )
        manager = AccountManager(ACCOUNTS_DIR, http_client)
        set_account_state("", None)  # ghi file health ngay cả khi chưa có tài khoản
        if _gateway is not None:
            _gateway.routes = manager.handle
            await _gateway.serve(BOT_HTTP_PORT)
        tasks = [manager.start(n) for n, _ in accounts]
        if _gateway is None:
            await asyncio.gather(*tasks)
            return
        # Không await các task: trang "Tài khoản kênh" có thể dừng/chạy lại
        # (hủy task) bất cứ lúc nào. Tài khoản bị bỏ vẫn giữ tiến trình sống để
        # healthcheck báo unhealthy thay vì Docker restart với cookie hỏng mãi mãi.
        await asyncio.Event().wait()


def main_blocking() -> None:
    configure_utf8_console()
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        log("multi", "Đã dừng theo yêu cầu người dùng.")
    except RuntimeError as error:
        log("multi", f"❌ {_format_exception_chain(error)}")
        raise SystemExit(1) from error


if __name__ == "__main__":
    main_blocking()
