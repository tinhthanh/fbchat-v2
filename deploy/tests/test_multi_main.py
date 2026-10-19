from __future__ import annotations

import asyncio
import contextvars
import inspect
import json
from pathlib import Path
from typing import Any

import pytest

import main as upstream_main
import multi_main
from _messaging._listening_e2ee import listeningE2EEEvent


class _FakeListener:
    startup_timeout = 1.0
    device_path: str | None = None

    def on_message(self, callback: Any) -> None:
        pass

    async def connect_mqtt(self) -> None:
        await asyncio.Event().wait()

    def wait_until_connected(self, timeout: float, *, require_e2ee: bool) -> bool:
        return True

    def stop(self) -> None:
        pass


@pytest.fixture
def fake_listener(monkeypatch: pytest.MonkeyPatch) -> _FakeListener:
    listener = _FakeListener()
    monkeypatch.setattr(
        upstream_main, "listeningE2EEEvent", lambda data, **options: listener
    )
    return listener


def _write_account(root: Path, name: str, **overrides: Any) -> Path:
    account_dir = root / name
    account_dir.mkdir(parents=True)
    config = {"cookies": "c_user=1; xs=x;", "prefix": "/", "admins": []}
    config.update(overrides)
    (account_dir / "config.json").write_text(json.dumps(config), encoding="utf-8")
    return account_dir


# --- Contract với upstream: báo sớm khi src/main.py hoặc listener đổi API ---


def test_upstream_contract_used_by_fork(mock_dataFB: dict[str, Any]) -> None:
    assert callable(upstream_main.log)
    assert callable(getattr(upstream_main.SimpleBot, "_queue_event", None))
    for name in (
        "prefix",
        "admins",
        "log_message_content",
        "debug_errors",
        "http_client",
    ):
        assert name in inspect.signature(upstream_main.SimpleBot).parameters

    listener = listeningE2EEEvent(mock_dataFB)
    assert hasattr(listener, "device_path")
    assert isinstance(listener.startup_timeout, float)
    wait_params = inspect.signature(listener.wait_until_connected).parameters
    assert "require_e2ee" in wait_params


def test_simple_bot_resolves_log_from_main_at_call_time(
    monkeypatch: pytest.MonkeyPatch,
    mock_dataFB: dict[str, Any],
    fake_listener: _FakeListener,
) -> None:
    lines: list[tuple[str, str]] = []
    monkeypatch.setattr(
        upstream_main, "log", lambda tag, message: lines.append((tag, message))
    )
    bot = upstream_main.SimpleBot(mock_dataFB)

    bot._queue_event({"type": "ready"})

    assert lines == [("e2ee", "ready")]


# --- Hành vi của runner ---


def test_discover_accounts_requires_config_and_safe_name(tmp_path: Path) -> None:
    _write_account(tmp_path, "beta")
    _write_account(tmp_path, "alpha")
    _write_account(tmp_path, "-bad")
    (tmp_path / "empty").mkdir()
    (tmp_path / "stray.json").write_text("{}", encoding="utf-8")

    names = [name for name, _ in multi_main.discover_accounts(tmp_path)]

    assert names == ["alpha", "beta"]


def test_discover_accounts_missing_root_returns_empty(tmp_path: Path) -> None:
    assert multi_main.discover_accounts(tmp_path / "missing") == []


def test_load_account_config_applies_defaults_and_validates(tmp_path: Path) -> None:
    account_dir = _write_account(tmp_path, "acc1")
    config = multi_main.load_account_config(account_dir / "config.json")
    assert config["log_message_content"] is False
    assert config["debug_errors"] is False

    bad_dir = _write_account(tmp_path, "acc2", admins="42")
    with pytest.raises(ValueError, match="admins"):
        multi_main.load_account_config(bad_dir / "config.json")


@pytest.mark.asyncio
async def test_run_account_builds_account_bot(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, mock_dataFB: dict[str, Any]
) -> None:
    account_dir = _write_account(tmp_path, "acc1", admins=["42"])
    created: list[dict[str, Any]] = []

    async def fake_data_get_home(**_: Any) -> dict[str, Any]:
        return mock_dataFB

    class _FakeBot:
        def __init__(self, dataFB: dict[str, Any], **options: Any) -> None:
            created.append(options)

        async def run(self) -> None:
            return None

    monkeypatch.setattr(multi_main, "dataGetHome", fake_data_get_home)
    monkeypatch.setattr(multi_main, "AccountBot", _FakeBot)

    await multi_main.run_account("acc1", account_dir, object())  # type: ignore[arg-type]

    assert created[0]["account"] == "acc1"
    assert created[0]["device_path"] == str(account_dir / "e2ee_device.json")
    assert created[0]["admins"] == ["42"]


@pytest.mark.asyncio
async def test_run_account_rejects_invalid_session(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    account_dir = _write_account(tmp_path, "acc1")

    async def fake_data_get_home(**_: Any) -> None:
        return None

    monkeypatch.setattr(multi_main, "dataGetHome", fake_data_get_home)

    with pytest.raises(RuntimeError, match="dataFB"):
        await multi_main.run_account("acc1", account_dir, object())  # type: ignore[arg-type]


def test_account_bot_sets_device_path_on_listener(
    mock_dataFB: dict[str, Any], fake_listener: _FakeListener
) -> None:
    multi_main.AccountBot(mock_dataFB, account="acc1", device_path="/d/acc1.json")

    assert fake_listener.device_path == "/d/acc1.json"


def test_listener_callback_logs_with_account_label(
    monkeypatch: pytest.MonkeyPatch,
    mock_dataFB: dict[str, Any],
    fake_listener: _FakeListener,
    capsys: pytest.CaptureFixture[str],
) -> None:
    monkeypatch.setattr(upstream_main, "log", multi_main.log)
    bot = multi_main.AccountBot(mock_dataFB, account="acc1", device_path="/d.json")

    # Mô phỏng call_soon_threadsafe: callback chạy trong context không có nhãn.
    contextvars.Context().run(bot._queue_event, {"type": "ready"})

    assert "[acc1] [e2ee] ready" in capsys.readouterr().out
    assert multi_main.LOG_LABEL.get() == ""


@pytest.mark.asyncio
async def test_on_ready_fires_when_listener_is_ready(
    mock_dataFB: dict[str, Any], fake_listener: _FakeListener
) -> None:
    bot = multi_main.AccountBot(mock_dataFB, account="acc1", device_path="/d.json")
    ready = asyncio.Event()

    await multi_main._notify_when_ready(bot, ready.set)

    assert ready.is_set()


@pytest.mark.asyncio
async def test_failing_account_gives_up_without_stopping_others(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    calls: dict[str, int] = {"bad": 0, "good": 0}

    async def fake_run_account(name: str, *_: Any, **__: Any) -> None:
        calls[name] += 1
        if name == "bad":
            raise RuntimeError("cookie expired")

    monkeypatch.setattr(multi_main, "run_account", fake_run_account)

    results = await asyncio.gather(
        multi_main.supervise_account(
            "bad", tmp_path, object(), max_attempts=3, retry_base=0  # type: ignore[arg-type]
        ),
        multi_main.supervise_account(
            "good", tmp_path, object(), max_attempts=3, retry_base=0  # type: ignore[arg-type]
        ),
    )

    assert results == [False, True]
    assert calls == {"bad": 3, "good": 1}


@pytest.mark.asyncio
async def test_long_running_account_resets_retry_budget(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    failures = {"left": 6}

    async def flaky_run_account(*_: Any, **__: Any) -> None:
        if failures["left"]:
            failures["left"] -= 1
            raise RuntimeError("E2EE listener đã dừng ngoài dự kiến.")

    monkeypatch.setattr(multi_main, "run_account", flaky_run_account)

    # stable_after=0: mọi lần chạy được coi là ổn định nên không bao giờ bỏ cuộc.
    ok = await multi_main.supervise_account(
        "acc1",
        tmp_path,
        object(),  # type: ignore[arg-type]
        max_attempts=2,
        retry_base=0,
        stable_after=0,
    )

    assert ok is True
    assert failures["left"] == 0


@pytest.mark.asyncio
async def test_health_file_tracks_running_and_dropped_accounts(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    health = tmp_path / "health.json"
    monkeypatch.setattr(multi_main, "HEALTH_FILE", str(health))
    monkeypatch.setattr(multi_main, "_account_states", {})
    seen_while_running: list[dict[str, str]] = []

    async def fake_run_account(name: str, *_: Any, on_ready: Any = None) -> None:
        if name == "bad":
            raise RuntimeError("cookie expired")
        on_ready()
        seen_while_running.append(json.loads(health.read_text(encoding="utf-8")))

    monkeypatch.setattr(multi_main, "run_account", fake_run_account)

    await multi_main.supervise_account(
        "good", tmp_path, object(), max_attempts=2, retry_base=0  # type: ignore[arg-type]
    )
    await multi_main.supervise_account(
        "bad", tmp_path, object(), max_attempts=2, retry_base=0  # type: ignore[arg-type]
    )

    assert seen_while_running == [{"good": "running"}]
    assert json.loads(health.read_text(encoding="utf-8")) == {
        "good": "running",
        "bad": "dropped",
    }
