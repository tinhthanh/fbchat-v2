import pytest

from _messaging._bridge_actions import BridgeActions


class _Bridge:
    def __init__(self) -> None:
        self.calls: list[tuple[str, dict]] = []

    def call_blocking(self, method: str, params: dict) -> dict:
        self.calls.append((method, params))
        return {"id": params.get("userId"), "name": "Người Lạ"}

    async def call(self, method: str, params: dict) -> dict:
        return self.call_blocking(method, params)


@pytest.mark.asyncio
async def test_get_user_info_sends_numeric_id() -> None:
    bridge = _Bridge()
    actions = BridgeActions(bridge)  # type: ignore[arg-type]
    assert (await actions.get_user_info("42"))["name"] == "Người Lạ"
    assert actions.get_user_info_blocking(43)["id"] == 43
    assert bridge.calls == [
        ("getUserInfo", {"userId": 42}),
        ("getUserInfo", {"userId": 43}),
    ]


@pytest.mark.asyncio
@pytest.mark.parametrize("user_id", ["", "abc", "0", -1])
async def test_get_user_info_rejects_invalid_id(user_id: object) -> None:
    bridge = _Bridge()
    with pytest.raises(ValueError):
        await BridgeActions(bridge).get_user_info(user_id)  # type: ignore[arg-type]
    assert bridge.calls == []


@pytest.mark.asyncio
async def test_get_e2ee_group_info_requires_group_jid() -> None:
    bridge = _Bridge()
    actions = BridgeActions(bridge)  # type: ignore[arg-type]
    await actions.get_e2ee_group_info("385@g.us")
    actions.get_e2ee_group_info_blocking("386@g.us")
    assert bridge.calls == [
        ("getE2EEGroupInfo", {"chatJid": "385@g.us"}),
        ("getE2EEGroupInfo", {"chatJid": "386@g.us"}),
    ]
    with pytest.raises(ValueError):
        await actions.get_e2ee_group_info("42@msgr")
