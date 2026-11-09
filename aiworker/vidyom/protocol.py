"""JSON-RPC 2.0 framing, one JSON object per line (I6).

This is the Python side of internal/contract/jsonrpc; the line limit and the error
codes are the same on both sides.
"""

from __future__ import annotations

import json
from typing import Any, BinaryIO

VERSION = "2.0"

# Same bound as jsonrpc.MaxMessageBytes in Go.
MAX_MESSAGE_BYTES = 64 << 20

PROTOCOL_VERSION = 1

# Standard codes, plus the implementation range of schemas/ai-worker-rpc.v1.json.
CODE_PARSE_ERROR = -32700
CODE_INVALID_REQUEST = -32600
CODE_METHOD_NOT_FOUND = -32601
CODE_INVALID_PARAMS = -32602
CODE_INTERNAL_ERROR = -32603
CODE_REQUEST_CANCELLED = -32800
CODE_UNAVAILABLE = -32001
CODE_UNAUTHORIZED = -32002


class RpcError(Exception):
    """An error answered to the client with an explicit code.

    Messages reach clients and logs. They must never carry secrets or transcript
    text.
    """

    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


class Cancelled(Exception):
    """The client sent $/cancelRequest for this request."""


class MessageTooLarge(Exception):
    """A line passed MAX_MESSAGE_BYTES; the connection cannot be resynchronized."""


def read_message(stream: BinaryIO) -> bytes | None:
    """Reads one line without its terminator. None means end of stream."""
    line = stream.readline(MAX_MESSAGE_BYTES + 1)
    if not line:
        return None
    if len(line) > MAX_MESSAGE_BYTES:
        raise MessageTooLarge()
    return line.rstrip(b"\r\n")


def encode(message: dict[str, Any]) -> bytes:
    """Encodes one message as a line. NaN and Infinity are not JSON and are refused."""
    return (
        json.dumps(message, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode("utf-8")
        + b"\n"
    )


def result_message(request_id: Any, result: Any) -> dict[str, Any]:
    return {"jsonrpc": VERSION, "id": request_id, "result": result}


def error_message(request_id: Any, code: int, message: str) -> dict[str, Any]:
    return {"jsonrpc": VERSION, "id": request_id, "error": {"code": code, "message": message}}


def notification(method: str, params: dict[str, Any]) -> dict[str, Any]:
    return {"jsonrpc": VERSION, "method": method, "params": params}
