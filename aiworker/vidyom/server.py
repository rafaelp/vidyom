"""The JSON-RPC server: dispatch, cancellation, shutdown and the three transports (I6).

Requests run in a thread pool so a long transcription never blocks `health`,
`$/cancelRequest` or another client's query embedding. Each connection must start
with `initialize`. stdout belongs to the protocol on the stdio transport, so anything
for people goes to stderr.
"""

from __future__ import annotations

import hmac
import ipaddress
import json
import logging
import os
import socket
import stat
import threading
from concurrent.futures import ThreadPoolExecutor
from typing import Any, BinaryIO

from vidyom.engines.base import Engine, RequestContext
from vidyom.protocol import (
    MAX_MESSAGE_BYTES,
    CODE_INTERNAL_ERROR,
    CODE_INVALID_PARAMS,
    CODE_INVALID_REQUEST,
    CODE_METHOD_NOT_FOUND,
    CODE_PARSE_ERROR,
    CODE_REQUEST_CANCELLED,
    CODE_UNAUTHORIZED,
    CODE_UNAVAILABLE,
    PROTOCOL_VERSION,
    VERSION,
    Cancelled,
    MessageTooLarge,
    RpcError,
    encode,
    error_message,
    notification,
    read_message,
    result_message,
)

log = logging.getLogger("vidyom.aiworker")

MAX_EMBED_BATCH = 64
_MAX_WORKERS = 8


class Worker:
    """State shared by every connection: the engine, the pool and the drain flag."""

    def __init__(self, engine: Engine, token: str | None = None, token_required: bool = False) -> None:
        self.engine = engine
        self.token = token
        self.token_required = token_required
        self.pool = ThreadPoolExecutor(max_workers=_MAX_WORKERS, thread_name_prefix="vidyom-rpc")
        self.stopping = threading.Event()
        self.finished = threading.Event()
        self._in_flight = 0
        self._lock = threading.Lock()
        self._idle = threading.Condition(self._lock)

    def begin(self) -> bool:
        with self._lock:
            if self.stopping.is_set():
                return False
            self._in_flight += 1
            return True

    def end(self) -> None:
        with self._lock:
            self._in_flight -= 1
            self._idle.notify_all()

    def drain(self) -> None:
        """Refuses new work and waits for what is running."""
        with self._lock:
            self.stopping.set()
            while self._in_flight > 0:
                self._idle.wait()

    def close(self) -> None:
        self.pool.shutdown(wait=False, cancel_futures=True)
        self.finished.set()


class Connection:
    """One client. Reads on the calling thread and answers from the pool."""

    def __init__(self, worker: Worker, reader: BinaryIO, writer: BinaryIO) -> None:
        self.worker = worker
        self.reader = reader
        self.writer = writer
        self._write_lock = threading.Lock()
        self._initialized = False
        self._cancels: dict[str, threading.Event] = {}
        self._cancels_lock = threading.Lock()
        self._closed = threading.Event()

    def serve(self) -> None:
        """Runs until the stream ends or the worker stops. Disconnecting cancels the
        client's in-flight requests: nobody is left to receive their results."""
        try:
            while not self._closed.is_set():
                try:
                    line = read_message(self.reader)
                except MessageTooLarge:
                    self._write(error_message(None, CODE_INVALID_REQUEST, "message exceeds the size limit"))
                    return
                except (OSError, ValueError):
                    return
                if line is None:
                    return
                if line:
                    self._handle(line)
        finally:
            self._closed.set()
            with self._cancels_lock:
                for event in self._cancels.values():
                    event.set()

    def _write(self, message: dict[str, Any]) -> None:
        self._write_encoded(encode(message))

    def _write_encoded(self, data: bytes) -> None:
        with self._write_lock:
            if self._closed.is_set():
                return
            try:
                self.writer.write(data)
                self.writer.flush()
            except (OSError, ValueError):
                self._closed.set()

    def _handle(self, line: bytes) -> None:
        try:
            message = json.loads(line)
        except ValueError:
            self._write(error_message(None, CODE_PARSE_ERROR, "invalid JSON"))
            return
        if not isinstance(message, dict) or message.get("jsonrpc") != VERSION:
            self._write(error_message(None, CODE_INVALID_REQUEST, "expected a JSON-RPC 2.0 object"))
            return
        method = message.get("method")
        if not isinstance(method, str):
            return  # a stray response; this server sends no requests
        params = message.get("params")
        if params is None:
            params = {}
        request_id = message.get("id")
        is_request = "id" in message and request_id is not None

        if method == "$/cancelRequest":
            self._cancel(params)
            return
        if not is_request:
            return  # unknown notifications are ignored, as JSON-RPC requires
        if not isinstance(params, dict):
            self._write(error_message(request_id, CODE_INVALID_PARAMS, "params must be an object"))
            return

        if method == "initialize":
            self._initialize(request_id, params)
            return
        if not self._initialized:
            self._write(error_message(request_id, CODE_UNAUTHORIZED, "initialize required"))
            return
        if method == "health":
            self._write(result_message(request_id, self.worker.engine.health()))
            return
        if method == "shutdown":
            threading.Thread(target=self._shutdown, args=(request_id,), daemon=True).start()
            return
        if method not in ("transcribe", "embed"):
            self._write(error_message(request_id, CODE_METHOD_NOT_FOUND, "method not found"))
            return
        if not self.worker.begin():
            self._write(error_message(request_id, CODE_UNAVAILABLE, "the worker is shutting down"))
            return
        cancel = threading.Event()
        with self._cancels_lock:
            self._cancels[str(request_id)] = cancel
        try:
            self.worker.pool.submit(self._run, method, request_id, params, cancel)
        except RuntimeError:
            self._finish(request_id)
            self._write(error_message(request_id, CODE_UNAVAILABLE, "the worker is shutting down"))

    def _initialize(self, request_id: Any, params: dict[str, Any]) -> None:
        if params.get("protocol_version") != PROTOCOL_VERSION:
            self._write(error_message(request_id, CODE_INVALID_PARAMS, f"protocol_version must be {PROTOCOL_VERSION}"))
            return
        if not isinstance(params.get("client"), str):
            self._write(error_message(request_id, CODE_INVALID_PARAMS, "client is required"))
            return
        if self.worker.token_required and not self._token_matches(params.get("token")):
            self._write(error_message(request_id, CODE_UNAUTHORIZED, "wrong token"))
            self._closed.set()
            return
        self._initialized = True
        described = self.worker.engine.describe()
        self._write(
            result_message(
                request_id,
                {
                    "protocol_version": PROTOCOL_VERSION,
                    "engine": described.engine,
                    "transcription_model": described.transcription_model,
                    "embedding_model": described.embedding_model,
                    "embedding_dimensions": described.embedding_dimensions,
                    "device": described.device,
                    "capabilities": described.capabilities,
                },
            )
        )

    def _token_matches(self, offered: Any) -> bool:
        if not isinstance(offered, str) or not self.worker.token:
            return False
        return hmac.compare_digest(offered.encode("utf-8"), self.worker.token.encode("utf-8"))

    def _shutdown(self, request_id: Any) -> None:
        self.worker.drain()
        self._write(result_message(request_id, {}))
        self.worker.close()

    def _cancel(self, params: Any) -> None:
        target = params.get("id") if isinstance(params, dict) else None
        with self._cancels_lock:
            event = self._cancels.get(str(target))
        if event is not None:
            event.set()

    def _finish(self, request_id: Any) -> None:
        with self._cancels_lock:
            self._cancels.pop(str(request_id), None)
        self.worker.end()

    def _run(self, method: str, request_id: Any, params: dict[str, Any], cancel: threading.Event) -> None:
        try:
            token = params.get("progress_token")
            ctx = RequestContext(cancel, self._progress_sender(token) if isinstance(token, str) and token else None)
            if method == "transcribe":
                result = self._transcribe(params, ctx)
            else:
                result = self._embed(params, ctx)
            reply = result_message(request_id, result)
        except Cancelled:
            reply = error_message(request_id, CODE_REQUEST_CANCELLED, "request cancelled")
        except RpcError as err:
            reply = error_message(request_id, err.code, err.message)
        except Exception as err:  # never leak internals to a client, keep them in the log
            log.exception("%s failed", method)
            reply = error_message(request_id, CODE_INTERNAL_ERROR, f"internal error: {type(err).__name__}")
        data = self._encode_reply(request_id, reply)
        # Deregister before answering, so a client that reacts to the reply and asks
        # the worker to shut down never finds this request still counted in flight.
        self._finish(request_id)
        self._write_encoded(data)

    def _encode_reply(self, request_id: Any, reply: dict[str, Any]) -> bytes:
        """A reply that cannot go on the wire must still answer, or the client waits forever."""
        try:
            data = encode(reply)
        except ValueError:
            log.error("a result could not be encoded")
            return encode(error_message(request_id, CODE_INTERNAL_ERROR, "the result could not be encoded"))
        if len(data) > MAX_MESSAGE_BYTES:
            return encode(error_message(request_id, CODE_INTERNAL_ERROR, "the result exceeds the message size limit"))
        return data

    def _progress_sender(self, token: str):
        def send(percent: float, message: str) -> None:
            params: dict[str, Any] = {"token": token, "percent": percent}
            if message:
                params["message"] = message
            self._write(notification("$/progress", params))

        return send

    def _transcribe(self, params: dict[str, Any], ctx: RequestContext) -> dict[str, Any]:
        audio_path = params.get("audio_path")
        if not isinstance(audio_path, str) or not audio_path:
            raise RpcError(CODE_INVALID_PARAMS, "audio_path is required")
        word_timestamps = params.get("word_timestamps")
        if not isinstance(word_timestamps, bool):
            raise RpcError(CODE_INVALID_PARAMS, "word_timestamps must be a boolean")
        language = params.get("language")
        if language is not None and not isinstance(language, str):
            raise RpcError(CODE_INVALID_PARAMS, "language must be a string")
        if not os.path.isfile(audio_path):
            raise RpcError(CODE_INVALID_PARAMS, "audio_path is not a readable file")
        return self.worker.engine.transcribe(audio_path, language or None, word_timestamps, ctx)

    def _embed(self, params: dict[str, Any], ctx: RequestContext) -> dict[str, Any]:
        texts = params.get("texts")
        if not isinstance(texts, list) or not texts or not all(isinstance(text, str) for text in texts):
            raise RpcError(CODE_INVALID_PARAMS, "texts must be a non-empty array of strings")
        if len(texts) > MAX_EMBED_BATCH:
            raise RpcError(CODE_INVALID_PARAMS, f"at most {MAX_EMBED_BATCH} texts per call")
        kind = params.get("kind")
        if kind not in ("passage", "query"):
            raise RpcError(CODE_INVALID_PARAMS, "kind must be passage or query")
        return self.worker.engine.embed(texts, kind, ctx)


def serve_stdio(engine: Engine, reader: BinaryIO, writer: BinaryIO) -> None:
    """Serves one client on a pair of streams; used by tests. EOF ends the process."""
    worker = Worker(engine)
    connection = Connection(worker, reader, writer)
    thread = threading.Thread(target=connection.serve, daemon=True)
    thread.start()
    while thread.is_alive() and not worker.finished.is_set():
        thread.join(0.05)
    worker.close()


def _serve_listener(worker: Worker, listener: socket.socket) -> None:
    listener.settimeout(0.2)
    while not worker.stopping.is_set():
        try:
            client, _ = listener.accept()
        except TimeoutError:
            continue
        except OSError:
            break
        threading.Thread(target=_serve_client, args=(worker, client), daemon=True).start()
    worker.finished.wait()


def _serve_client(worker: Worker, client: socket.socket) -> None:
    with client:
        client.settimeout(None)
        with client.makefile("rb") as reader, client.makefile("wb") as writer:
            connection = Connection(worker, reader, writer)
            connection.serve()


def prepare_socket_path(path: str) -> None:
    """Removes a stale socket. Refuses to replace anything else, or a live socket."""
    try:
        mode = os.lstat(path).st_mode
    except FileNotFoundError:
        return
    if not stat.S_ISSOCK(mode):
        raise OSError(f"{path} exists and is not a socket")
    probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        probe.settimeout(1)
        probe.connect(path)
    except (ConnectionRefusedError, FileNotFoundError):
        os.unlink(path)
        return
    except OSError as err:
        raise OSError(f"cannot tell whether {path} is in use: {err}") from err
    finally:
        probe.close()
    raise OSError(f"another worker is listening on {path}")


def open_unix_listener(path: str) -> socket.socket:
    """Binds with mode 0600 from the moment the socket exists (I6)."""
    prepare_socket_path(path)
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    previous = os.umask(0o177)
    try:
        listener.bind(path)
    finally:
        os.umask(previous)
    os.chmod(path, 0o600)
    listener.listen(16)
    return listener


def open_tcp_listener(host: str, port: int) -> socket.socket:
    """Loopback only: this transport carries local paths and has no TLS (I6)."""
    try:
        address = ipaddress.ip_address(host)
    except ValueError as err:
        raise ValueError(f"tcp host must be a loopback IP address, got {host!r}") from err
    if not address.is_loopback:
        raise ValueError(f"tcp host must be a loopback address, got {host!r}")
    family = socket.AF_INET6 if address.version == 6 else socket.AF_INET
    listener = socket.socket(family, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind((host, port))
    listener.listen(16)
    return listener


def serve_unix(engine: Engine, path: str, on_listening=None) -> None:
    worker = Worker(engine)
    listener = open_unix_listener(path)
    try:
        if on_listening:
            on_listening(worker)
        _serve_listener(worker, listener)
    finally:
        listener.close()
        try:
            os.unlink(path)
        except OSError:
            pass


def serve_tcp(engine: Engine, host: str, port: int, token: str, on_listening=None) -> None:
    if not token:
        raise ValueError("the tcp transport requires a token")
    worker = Worker(engine, token=token, token_required=True)
    listener = open_tcp_listener(host, port)
    try:
        if on_listening:
            on_listening(worker, listener.getsockname()[1])
        _serve_listener(worker, listener)
    finally:
        listener.close()
