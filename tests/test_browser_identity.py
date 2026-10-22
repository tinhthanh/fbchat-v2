"""Mọi request HTTP và bridge E2EE phải mang cùng một danh tính trình duyệt.

Facebook coi một phiên đăng nhập đổi User-Agent/hệ điều hành giữa các request
là dấu hiệu phiên bị đánh cắp, nên Python phải trùng hằng số của bridge.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

from _core import _utils
from _core._session import _build_home_request

USERAGENT_GO = (
    Path(__file__).resolve().parents[1]
    / "bridge-e2ee/meta/pkg/messagix/useragent/useragent.go"
)


def _go_const(name: str) -> str:
    match = re.search(rf'^const {name} = "([^"]*)"', USERAGENT_GO.read_text(), re.M)
    assert match, f"không thấy {name} trong useragent.go"
    return match.group(1)


@pytest.mark.skipif(
    not USERAGENT_GO.exists(),
    reason="cần submodule bridge-e2ee/meta; job Go kiểm tra cùng điều này (identity_test.go)",
)
def test_python_identity_matches_bridge() -> None:
    version = _go_const("ChromeVersion")
    assert _utils.BROWSER_CHROME_VERSION == version
    assert _utils.BROWSER_CHROME_VERSION_FULL.startswith(version + ".")
    full_suffix = re.search(
        r'ChromeVersionFull = ChromeVersion \+ "([^"]+)"', USERAGENT_GO.read_text()
    )
    assert full_suffix
    assert _utils.BROWSER_CHROME_VERSION_FULL == version + full_suffix.group(1)
    assert _utils.BROWSER_PLATFORM == f'"{_go_const("OSName")}"'
    assert "X11; Linux x86_64" in _utils.BROWSER_USER_AGENT
    assert f"Chrome/{version}.0.0.0" in _utils.BROWSER_USER_AGENT


def test_every_request_uses_the_same_identity() -> None:
    seen = {
        (h["User-Agent"], h["sec-ch-ua"], h["sec-ch-ua-platform"])
        for h in (_utils.Headers() for _ in range(50))
    }
    assert seen == {
        (
            _utils.BROWSER_USER_AGENT,
            _utils.BROWSER_SEC_CH_UA,
            _utils.BROWSER_PLATFORM,
        )
    }
    home = _build_home_request("c_user=1; xs=x;")["headers"]
    assert home["user-agent"] == _utils.BROWSER_USER_AGENT
    assert home["sec-ch-ua-platform"] == _utils.BROWSER_PLATFORM
    assert home["sec-ch-ua"] == _utils.BROWSER_SEC_CH_UA
