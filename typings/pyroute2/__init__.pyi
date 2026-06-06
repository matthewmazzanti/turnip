# Partial local stub for pyroute2 0.9.6 — scoped to the symbols main.py uses.
# NOT a complete stub. pyroute2 ships no py.typed and no stubs of its own, and
# pyright's source inference gets the dynamic bits wrong (e.g. link_lookup is
# inferred as a Generator, but it returns a subscriptable list of ifindexes),
# so we pin just our surface here. Extend as the project touches more of the API.
from typing import Any, Self, TypedDict

from . import netns as netns


class Status(TypedDict):
    """The `.status` dict of an IPRoute/NetNS handle (mirrors the socket spec).

    `netns` is the bound namespace path (None on the host netns) -- the field
    we read to target a veth peer's net_ns_fd. The rest is here for fidelity.
    """

    target: str
    netns: str | None
    netns_path: list[str]
    event_loop: str
    closed: bool
    error: BaseException | None
    pid: int | None
    epid: int | None
    port: int | None
    groups: int
    sndbuf: int
    rcvbuf: int
    all_ns: bool
    ext_ack: bool
    strict_check: bool
    nlm_echo: bool
    nlm_generator: bool | None
    use_event_loop: Any
    use_socket: Any
    uname: Any
    eids: Any

# --- low-level netlink (IPRoute / NetNS) -----------------------------------

class IPRoute:
    # bound-namespace identifier lives here, e.g. status["netns"] -> the path
    status: Status
    def __init__(self, *args: Any, **kwargs: Any) -> None: ...
    def __enter__(self) -> Self: ...
    def __exit__(self, *exc: Any) -> None: ...
    def close(self) -> None: ...
    # link_lookup really returns a list of ifindexes (subscriptable)
    def link_lookup(self, **criteria: Any) -> list[int]: ...
    # string-dispatched verbs ("add" / "set" / "del" ...)
    def link(self, command: str, **kwargs: Any) -> list[Any]: ...
    def addr(self, command: str, **kwargs: Any) -> list[Any]: ...
    def route(self, command: str, **kwargs: Any) -> list[Any]: ...
    def get_links(self, *index: int, **kwargs: Any) -> list[Any]: ...
    def get_addr(self, **kwargs: Any) -> list[Any]: ...
    def get_routes(self, **kwargs: Any) -> list[Any]: ...

class NetNS(IPRoute):
    def __init__(self, path: str, *args: Any, **kwargs: Any) -> None: ...
