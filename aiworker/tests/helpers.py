"""Shared test helpers: a blocking JSON-RPC client and audio fixtures."""

from __future__ import annotations

import json
import os
import struct
import subprocess
import sys
import threading
import wave
from pathlib import Path
from typing import Any, BinaryIO

AIWORKER_ROOT = Path(__file__).resolve().parent.parent
REPOSITORY_ROOT = AIWORKER_ROOT.parent
RPC_SCHEMA = REPOSITORY_ROOT / "schemas" / "ai-worker-rpc.v1.json"


def worker_environment(extra: dict[str, str] | None = None) -> dict[str, str]:
    env = dict(os.environ)
    env["PYTHONPATH"] = str(AIWORKER_ROOT)
    env.pop("VIDYOM_AI_FAKE_TRANSCRIPT", None)
    env.pop("VIDYOM_AI_FAKE_DELAY_SECONDS", None)
    if extra:
        env.update(extra)
    return env


def start_worker(*args: str, env: dict[str, str] | None = None) -> subprocess.Popen:
    return subprocess.Popen(
        [sys.executable, "-m", "vidyom", *args],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=worker_environment(env),
    )


class RpcClient:
    """A small client: notifications are collected, responses wait by ID."""

    def __init__(self, reader: BinaryIO, writer: BinaryIO) -> None:
        self.reader = reader
        self.writer = writer
        self.next_id = 0
        self.responses: dict[Any, dict[str, Any]] = {}
        self.notifications: list[dict[str, Any]] = []
        self.condition = threading.Condition()
        self.eof = False
        self.thread = threading.Thread(target=self._read, daemon=True)
        self.thread.start()

    def _read(self) -> None:
        for line in self.reader:
            message = json.loads(line)
            with self.condition:
                if "method" in message:
                    self.notifications.append(message)
                else:
                    self.responses[message.get("id")] = message
                self.condition.notify_all()
        with self.condition:
            self.eof = True
            self.condition.notify_all()

    def send_raw(self, data: bytes) -> None:
        self.writer.write(data + b"\n")
        self.writer.flush()

    def send(self, method: str, params: Any = None, request_id: Any = None) -> Any:
        if request_id is None:
            self.next_id += 1
            request_id = self.next_id
        self.send_raw(json.dumps({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params or {}}).encode())
        return request_id

    def notify(self, method: str, params: Any) -> None:
        self.send_raw(json.dumps({"jsonrpc": "2.0", "method": method, "params": params}).encode())

    def wait(self, request_id: Any, timeout: float = 15) -> dict[str, Any]:
        with self.condition:
            ok = self.condition.wait_for(lambda: request_id in self.responses or self.eof, timeout)
            if not ok:
                raise AssertionError(f"no response to {request_id}")
            if request_id not in self.responses:
                raise EOFError(f"stream ended before the response to {request_id}")
            return self.responses.pop(request_id)

    def call(self, method: str, params: Any = None, timeout: float = 15) -> dict[str, Any]:
        return self.wait(self.send(method, params), timeout)

    def initialize(self, **extra: Any) -> dict[str, Any]:
        response = self.call("initialize", {"protocol_version": 1, "client": "test", **extra})
        if "error" in response:
            raise AssertionError(f"initialize failed: {response['error']}")
        return response["result"]

    def wait_eof(self, timeout: float = 15) -> bool:
        with self.condition:
            return self.condition.wait_for(lambda: self.eof, timeout)


def write_wav(path: Path, seconds: float, rate: int = 8000) -> None:
    with wave.open(str(path), "wb") as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)
        wav.setframerate(rate)
        wav.writeframes(b"\x00\x00" * int(seconds * rate))


def _ogg_page(serial: int, sequence: int, granule: int, payload: bytes, flags: int = 0) -> bytes:
    segments = []
    remaining = len(payload)
    while remaining >= 255:
        segments.append(255)
        remaining -= 255
    segments.append(remaining)
    header = struct.pack("<4sBBqIIIB", b"OggS", 0, flags, granule, serial, sequence, 0, len(segments))
    return header + bytes(segments) + payload


def write_ogg_opus(path: Path, seconds: float, pre_skip: int = 312) -> None:
    """A minimal Ogg container with OpusHead and a last page at the right granule."""
    head = b"OpusHead" + bytes([1, 1]) + struct.pack("<HIh", pre_skip, 48000, 0) + b"\x00"
    with open(path, "wb") as ogg:
        ogg.write(_ogg_page(1, 0, 0, head, flags=2))
        ogg.write(_ogg_page(1, 1, 0, b"OpusTags\x00\x00\x00\x00\x00\x00\x00\x00"))
        ogg.write(_ogg_page(1, 2, int(seconds * 48000) + pre_skip, b"\x00" * 300, flags=4))
