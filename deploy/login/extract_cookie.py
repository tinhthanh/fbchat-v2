"""Mở Chromium qua noVNC để người dùng đăng nhập Facebook, rồi ghi cookie.

Luồng:
  1. Nếu /data/accounts/$ACCOUNT/config.json đã có cookie c_user/xs và FORCE_LOGIN không bật,
     thoát ngay (exit 0) để bot khởi động.
  2. Khởi động Xvfb, x11vnc, websockify/noVNC và Chromium (không bật
     remote debugging, không có automation flag).
  3. Đọc định kỳ bản sao file SQLite "Cookies" trong profile và giải mã
     (password-store=basic -> khóa v10 mặc định của Chromium trên Linux).
  4. Khi có c_user và xs: tắt Chromium nhẹ nhàng để nó flush cookie xuống
     đĩa, đọc lại, ghi config.json atomically với quyền 0600 và exit 0.

Không bao giờ in giá trị cookie ra log.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any

from cryptography.hazmat.primitives import padding
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes

ACCOUNT = os.environ.get("ACCOUNT", "default")
if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}", ACCOUNT):
    sys.exit(f"[login] Tên tài khoản không hợp lệ: {ACCOUNT!r}")
CONFIG_PATH = Path(
    os.environ.get("CONFIG_PATH", f"/data/accounts/{ACCOUNT}/config.json")
)
LOGIN_URL = os.environ.get("LOGIN_URL", "https://www.facebook.com/login")
LOGIN_TIMEOUT = int(os.environ.get("LOGIN_TIMEOUT", "900"))
SCREEN = os.environ.get("SCREEN_SIZE", "1280x800")
VNC_PASSWORD = os.environ.get("VNC_PASSWORD", "")
FORCE_LOGIN = os.environ.get("FORCE_LOGIN", "").lower() in {"1", "true", "yes"}
PROFILE_DIR = Path(os.environ.get("CHROMIUM_PROFILE", "/tmp/chromium-profile"))
REQUIRED_COOKIES = ("c_user", "xs")
POLL_SECONDS = 3

# Chromium Linux với --password-store=basic: PBKDF2("peanuts", "saltysalt", 1).
_V10_KEY = hashlib.pbkdf2_hmac("sha1", b"peanuts", b"saltysalt", 1, 16)
_IV = b" " * 16

_procs: list[subprocess.Popen[bytes]] = []
_browser: subprocess.Popen[bytes] | None = None


def log(message: str) -> None:
    print(f"[login] {message}", flush=True)


def _cookie_names(cookie_string: str) -> set[str]:
    names = set()
    for part in cookie_string.split(";"):
        name, sep, value = part.strip().partition("=")
        if sep and value:
            names.add(name)
    return names


def _load_config() -> dict[str, Any]:
    try:
        data = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {}
    return data if isinstance(data, dict) else {}


def _write_config(cookie_string: str) -> None:
    config = _load_config()
    config.setdefault("botName", "fbchat-v2 docker bot")
    config.setdefault("prefix", os.environ.get("BOT_PREFIX", "/"))
    admins = [
        a.strip() for a in os.environ.get("BOT_ADMINS", "").split(",") if a.strip()
    ]
    if admins:
        config["admins"] = admins
    config.setdefault("admins", [])
    config.setdefault("log_message_content", False)
    config.setdefault("debug_errors", False)
    config["cookies"] = cookie_string

    CONFIG_PATH.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(
        dir=CONFIG_PATH.parent, prefix=f".{CONFIG_PATH.name}.", suffix=".tmp"
    )
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(config, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(tmp_name, 0o600)
        os.replace(tmp_name, CONFIG_PATH)
    except BaseException:
        Path(tmp_name).unlink(missing_ok=True)
        raise


def _spawn(*args: str) -> subprocess.Popen[bytes]:
    proc = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    _procs.append(proc)
    return proc


def _stop(proc: subprocess.Popen[bytes], timeout: float) -> None:
    if proc.poll() is None:
        proc.terminate()
    try:
        proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()


def _shutdown(*_: object) -> None:
    deadline = time.monotonic() + 10
    for proc in reversed(_procs):
        _stop(proc, max(0.1, deadline - time.monotonic()))


def _start_display() -> None:
    global _browser
    width, height = SCREEN.split("x")
    _spawn("Xvfb", ":99", "-screen", "0", f"{width}x{height}x24", "-nolisten", "tcp")
    time.sleep(1)
    # Không có window manager thì click không chuyển focus bàn phím giữa các cửa sổ.
    _spawn("openbox")

    vnc_args = [
        "x11vnc",
        "-display",
        ":99",
        "-forever",
        "-shared",
        "-localhost",
        "-rfbport",
        "5900",
        "-quiet",
    ]
    if VNC_PASSWORD:
        passwd_file = Path(tempfile.mkdtemp()) / "vncpass"
        subprocess.run(
            ["x11vnc", "-storepasswd", VNC_PASSWORD, str(passwd_file)],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        vnc_args += ["-rfbauth", str(passwd_file)]
    else:
        vnc_args.append("-nopw")
    _spawn(*vnc_args)
    _spawn("websockify", "--web", "/usr/share/novnc", "6080", "localhost:5900")

    _browser = _spawn(
        "chromium-browser",
        # Docker seccomp mặc định chặn namespace sandbox của Chromium.
        "--no-sandbox",
        # Ẩn infobar cảnh báo --no-sandbox; trang web không nhìn thấy flag này.
        "--test-type",
        "--disable-dev-shm-usage",
        # Không có GPU thật: dùng SwiftShader để WebGL vẫn hoạt động như máy thường.
        "--enable-unsafe-swiftshader",
        "--no-first-run",
        "--no-default-browser-check",
        "--password-store=basic",
        f"--user-data-dir={PROFILE_DIR}",
        "--start-maximized",
        f"--app={LOGIN_URL}",
    )


def _cookie_db() -> Path | None:
    for candidate in (
        PROFILE_DIR / "Default" / "Network" / "Cookies",
        PROFILE_DIR / "Default" / "Cookies",
    ):
        if candidate.is_file():
            return candidate
    return None


def _decrypt(encrypted: bytes, host_key: str, db_version: int) -> str:
    if encrypted[:3] != b"v10":
        raise ValueError(
            "Định dạng cookie không hỗ trợ (cần v10/password-store=basic)."
        )
    decryptor = Cipher(algorithms.AES(_V10_KEY), modes.CBC(_IV)).decryptor()
    padded = decryptor.update(encrypted[3:]) + decryptor.finalize()
    unpadder = padding.PKCS7(128).unpadder()
    plain = unpadder.update(padded) + unpadder.finalize()
    # Từ DB version 24, plaintext có tiền tố SHA-256(host_key) dài 32 byte.
    if db_version >= 24 and plain[:32] == hashlib.sha256(host_key.encode()).digest():
        plain = plain[32:]
    return plain.decode("utf-8")


def _read_facebook_cookies() -> dict[str, str]:
    """Đọc từ bản sao DB để không tranh khóa với Chromium đang chạy."""
    source = _cookie_db()
    if source is None:
        return {}
    with tempfile.TemporaryDirectory() as tmp:
        copy = Path(tmp) / "Cookies"
        shutil.copy2(source, copy)
        for suffix in ("-wal", "-journal"):
            sidecar = source.with_name(source.name + suffix)
            if sidecar.is_file():
                shutil.copy2(sidecar, copy.with_name(copy.name + suffix))
        conn = sqlite3.connect(copy)
        try:
            row = conn.execute(
                "SELECT value FROM meta WHERE key = 'version'"
            ).fetchone()
            db_version = int(row[0]) if row else 0
            rows = conn.execute(
                "SELECT host_key, name, value, encrypted_value FROM cookies "
                "WHERE host_key = 'facebook.com' OR host_key LIKE '%.facebook.com' "
                "ORDER BY creation_utc"
            ).fetchall()
        finally:
            conn.close()

    cookies: dict[str, str] = {}
    for host_key, name, value, encrypted in rows:
        if encrypted:
            value = _decrypt(encrypted, host_key, db_version)
        if value:
            cookies[name] = value
    return cookies


def _has_login(cookies: dict[str, str]) -> bool:
    return all(name in cookies for name in REQUIRED_COOKIES)


def main() -> int:
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))

    existing = str(_load_config().get("cookies") or "")
    if not FORCE_LOGIN and all(n in _cookie_names(existing) for n in REQUIRED_COOKIES):
        log(
            f"Tài khoản {ACCOUNT!r} đã có cookie; bỏ qua đăng nhập "
            "(đặt FORCE_LOGIN=1 để đăng nhập lại)."
        )
        return 0

    _start_display()
    log(f"Đăng nhập cho tài khoản {ACCOUNT!r}.")
    log("Mở http://localhost:6080/vnc.html?autoconnect=1&resize=scale để đăng nhập.")
    log("Sau khi đăng nhập, có thể mất tới ~30 giây để Chromium ghi cookie xuống đĩa.")

    deadline = time.monotonic() + LOGIN_TIMEOUT
    while time.monotonic() < deadline:
        time.sleep(POLL_SECONDS)
        try:
            cookies = _read_facebook_cookies()
        except (OSError, sqlite3.Error):
            # DB đang được ghi dở; thử lại ở vòng sau.
            continue
        if not _has_login(cookies):
            if _browser is not None and _browser.poll() is not None:
                log("Chromium đã bị đóng trước khi đăng nhập xong.")
                return 1
            continue

        log(
            "Đã phát hiện đăng nhập, đóng Chromium để lưu đủ cookie "
            "(màn hình 'Aw, Snap!' trong noVNC lúc này là bình thường)..."
        )
        if _browser is not None:
            _stop(_browser, timeout=15)
        final = _read_facebook_cookies()
        cookies = final if _has_login(final) else cookies
        cookie_string = "; ".join(f"{k}={v}" for k, v in cookies.items()) + ";"
        _write_config(cookie_string)
        log(f"Đã lưu cookie ({len(cookies)} mục) vào {CONFIG_PATH}.")
        return 0

    log(f"Hết thời gian chờ đăng nhập ({LOGIN_TIMEOUT}s).")
    return 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    finally:
        _shutdown()
