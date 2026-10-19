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
import contextvars
import gc
import json
import os
import re
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

_account_states: dict[str, str] = {}

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
        # Listener chỉ đọc device_path khi connect; chỉ có tác dụng nếu bật lưu
        # trạng thái E2EE ra file (e2ee_memory_only=False).
        self.listener.device_path = device_path

    def _queue_event(self, event: dict[str, Any]) -> None:
        # call_soon_threadsafe chạy callback trong bản sao context của listener
        # thread, nên phải gắn lại nhãn; không ảnh hưởng task khác.
        LOG_LABEL.set(self.account)
        super()._queue_event(event)


def set_account_state(name: str, state: str) -> None:
    """Ghi nhận starting/running/retrying/dropped và ghi file health nếu bật."""
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
    ready_task = asyncio.create_task(_notify_when_ready(bot, on_ready))
    try:
        await bot.run()
    finally:
        ready_task.cancel()


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


async def main() -> None:
    install_labeled_log()
    accounts = discover_accounts(ACCOUNTS_DIR)
    if not accounts:
        raise RuntimeError(
            f"Không có tài khoản nào trong {ACCOUNTS_DIR} (cần <tên>/config.json)."
        )
    log("multi", f"Chạy {len(accounts)} tài khoản: {', '.join(n for n, _ in accounts)}")
    async with httpx.AsyncClient(timeout=DEFAULT_HTTP_TIMEOUT) as http_client:
        results = await asyncio.gather(
            *(
                supervise_account(name, account_dir, http_client)
                for name, account_dir in accounts
            )
        )
        if not any(results):
            # Không thoát: với restart policy unless-stopped, thoát sẽ khiến
            # Docker restart và gọi lại Facebook bằng cookie hỏng mãi mãi.
            # Giữ tiến trình sống để healthcheck báo unhealthy.
            log(
                "multi",
                "❌ Mọi tài khoản đã bị bỏ; đăng nhập lại bằng dịch vụ login "
                "rồi restart bot.",
            )
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
