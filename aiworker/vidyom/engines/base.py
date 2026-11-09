"""The engine interface and what a running request gives an engine."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from typing import Any, Callable, Protocol

from vidyom.protocol import Cancelled


class RequestContext:
    """Progress and cancellation of one request."""

    def __init__(
        self,
        cancel_event: threading.Event | None = None,
        progress: Callable[[float, str], None] | None = None,
    ) -> None:
        self._cancel = cancel_event or threading.Event()
        self._progress = progress

    def check_cancelled(self) -> None:
        if self._cancel.is_set():
            raise Cancelled()

    def cancelled(self) -> bool:
        return self._cancel.is_set()

    def wait(self, seconds: float) -> None:
        """Sleeps, waking at once on cancellation."""
        if self._cancel.wait(seconds):
            raise Cancelled()

    def progress(self, percent: float, message: str = "") -> None:
        if self._progress is not None:
            self._progress(max(0.0, min(100.0, percent)), message)


@dataclass
class Description:
    """What initialize reports."""

    engine: str
    transcription_model: str
    embedding_model: str
    embedding_dimensions: int
    device: str
    capabilities: list[str] = field(default_factory=list)


class Engine(Protocol):
    def describe(self) -> Description: ...

    def health(self) -> dict[str, Any]: ...

    def transcribe(
        self,
        audio_path: str,
        language: str | None,
        word_timestamps: bool,
        ctx: RequestContext,
    ) -> dict[str, Any]: ...

    def embed(self, texts: list[str], kind: str, ctx: RequestContext) -> dict[str, Any]: ...
