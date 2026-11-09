"""Engines the worker can run. The choice is explicit, with no fallback (I27)."""

from __future__ import annotations

from vidyom.engines.base import Engine


def create_engine(name: str, **options) -> Engine:
    """Builds the named engine. Unknown names fail; nothing falls through."""
    if name == "fake":
        from vidyom.engines.fake import FakeEngine

        return FakeEngine()
    if name == "real":
        from vidyom.engines.real import RealEngine

        return RealEngine(**options)
    raise ValueError(f"unknown engine {name!r}; expected 'fake' or 'real'")
