"""Fixture cho test của phần deploy; không phụ thuộc tests/conftest.py upstream."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

import pytest

_DEPLOY_DIR = Path(__file__).resolve().parents[1]
for _path in (_DEPLOY_DIR.parent / "src", _DEPLOY_DIR):
    if str(_path) not in sys.path:
        sys.path.insert(0, str(_path))


@pytest.fixture
def mock_dataFB() -> dict[str, Any]:
    return {
        "fb_dtsg": "mock_dtsg",
        "jazoest": "22421",
        "sessionID": "1234567890",
        "FacebookID": "10001234567890",
        "clientRevision": "123456",
        "cookieFacebook": "c_user=10001234567890; xs=mock_xs; fr=mock_fr; datr=mock_datr;",
    }
