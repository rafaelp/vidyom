import json
import os
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from io import BytesIO
from pathlib import Path
from unittest import mock

from tests.helpers import RpcClient, start_worker, worker_environment, write_wav
from vidyom import protocol
from vidyom.engines.fake import EMBEDDING_DIMENSIONS, FakeEngine
from vidyom.server import Connection, Worker, open_tcp_listener, open_unix_listener


def shutdown_quietly(sock):
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass  # the other side is already gone


def close_process(process):
    process.kill()
    process.wait()
    for stream in (process.stdin, process.stdout, process.stderr):
        if stream is not None:
            stream.close()


class ConnectionTestCase(unittest.TestCase):
    """Runs a Connection in this process over a socket pair."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.audio = self.dir / "audio.wav"
        write_wav(self.audio, 20)
        patched = mock.patch.dict(os.environ)
        patched.start()
        self.addCleanup(patched.stop)
        os.environ.pop("VIDYOM_AI_FAKE_TRANSCRIPT", None)
        os.environ.pop("VIDYOM_AI_FAKE_DELAY_SECONDS", None)
        self.client = self.connect()

    def connect(self, token=None, token_required=False):
        server_side, client_side = socket.socketpair()
        self.worker = Worker(FakeEngine(), token=token, token_required=token_required)
        reader, writer = server_side.makefile("rb"), server_side.makefile("wb")
        self.connection = Connection(self.worker, reader, writer)

        connection = self.connection

        def serve():
            connection.serve()
            shutdown_quietly(server_side)
            reader.close()
            writer.close()
            server_side.close()

        threading.Thread(target=serve, daemon=True).start()
        reader_side, writer_side = client_side.makefile("rb"), client_side.makefile("wb")
        client = RpcClient(reader_side, writer_side)
        self.client_side = client_side
        self.addCleanup(client_side.close)
        self.addCleanup(writer_side.close)
        self.addCleanup(reader_side.close)
        self.addCleanup(shutdown_quietly, client_side)
        return client

    def transcribe_params(self, **extra):
        return {"audio_path": str(self.audio), "word_timestamps": True, **extra}


class ProtocolTest(ConnectionTestCase):
    def test_initialize_must_come_first(self):
        for method in ("health", "transcribe", "embed", "shutdown", "unknown"):
            self.assertEqual(self.client.call(method)["error"]["code"], protocol.CODE_UNAUTHORIZED, method)

    def test_initialize_describes_the_fake_engine(self):
        result = self.client.initialize()
        self.assertEqual(result["protocol_version"], 1)
        self.assertEqual(result["engine"], "fake")
        self.assertEqual(result["embedding_model"], "fake-hash-embed@1")
        self.assertEqual(result["embedding_dimensions"], EMBEDDING_DIMENSIONS)
        self.assertIn("fake", result["capabilities"])

    def test_initialize_rejects_another_protocol_version(self):
        response = self.client.call("initialize", {"protocol_version": 2, "client": "x"})
        self.assertEqual(response["error"]["code"], protocol.CODE_INVALID_PARAMS)

    def test_initialize_needs_a_client_name(self):
        response = self.client.call("initialize", {"protocol_version": 1})
        self.assertEqual(response["error"]["code"], protocol.CODE_INVALID_PARAMS)

    def test_malformed_input(self):
        self.client.send_raw(b"{nope")
        self.assertEqual(self.client.wait(None)["error"]["code"], protocol.CODE_PARSE_ERROR)
        self.client.send_raw(b"[1,2]")
        self.assertEqual(self.client.wait(None)["error"]["code"], protocol.CODE_INVALID_REQUEST)
        self.client.send_raw(b'{"id":1,"method":"health"}')
        self.assertEqual(self.client.wait(None)["error"]["code"], protocol.CODE_INVALID_REQUEST)

    def test_unknown_method(self):
        self.client.initialize()
        self.assertEqual(self.client.call("nope")["error"]["code"], protocol.CODE_METHOD_NOT_FOUND)

    def test_string_ids_are_echoed(self):
        self.client.initialize()
        request_id = self.client.send("health", request_id="abc")
        self.assertIn("result", self.client.wait(request_id))

    def test_health(self):
        self.client.initialize()
        result = self.client.call("health")["result"]
        self.assertEqual(result["status"], "ok")
        self.assertIn("FAKE ENGINE", result["detail"])


class TranscribeTest(ConnectionTestCase):
    def test_transcribes_with_words_and_label(self):
        self.client.initialize()
        result = self.client.call("transcribe", self.transcribe_params())["result"]
        self.assertEqual(result["engine"], "fake/canned@1")
        self.assertEqual(result["duration_seconds"], 20)
        self.assertTrue(result["segments"][0]["words"])

    def test_param_validation(self):
        self.client.initialize()
        cases = [
            {"word_timestamps": True},
            {"audio_path": "", "word_timestamps": True},
            {"audio_path": str(self.audio)},
            {"audio_path": str(self.audio), "word_timestamps": "yes"},
            {"audio_path": str(self.dir / "missing.wav"), "word_timestamps": True},
            {"audio_path": str(self.dir), "word_timestamps": True},
            {"audio_path": str(self.audio), "word_timestamps": True, "language": 7},
        ]
        for params in cases:
            response = self.client.call("transcribe", params)
            self.assertEqual(response["error"]["code"], protocol.CODE_INVALID_PARAMS, params)

    def test_progress_is_sent_only_with_a_token(self):
        self.client.initialize()
        self.client.call("transcribe", self.transcribe_params())
        self.assertEqual(self.client.notifications, [])
        self.client.call("transcribe", self.transcribe_params(progress_token="tok-1"))
        progress = [n for n in self.client.notifications if n["method"] == "$/progress"]
        self.assertTrue(progress)
        self.assertTrue(all(n["params"]["token"] == "tok-1" for n in progress))
        self.assertEqual(progress[-1]["params"]["percent"], 100.0)

    def test_cancel(self):
        os.environ["VIDYOM_AI_FAKE_DELAY_SECONDS"] = "30"
        self.client.initialize()
        request_id = self.client.send("transcribe", self.transcribe_params())
        time.sleep(0.1)
        self.client.notify("$/cancelRequest", {"id": str(request_id)})
        started = time.monotonic()
        self.assertEqual(self.client.wait(request_id)["error"]["code"], protocol.CODE_REQUEST_CANCELLED)
        self.assertLess(time.monotonic() - started, 5)

    def test_cancel_with_unknown_id_is_ignored(self):
        self.client.initialize()
        self.client.notify("$/cancelRequest", {"id": "999"})
        self.client.notify("$/cancelRequest", "not an object")
        self.assertEqual(self.client.call("health")["result"]["status"], "ok")

    def test_health_and_embed_answer_during_a_long_transcription(self):
        os.environ["VIDYOM_AI_FAKE_DELAY_SECONDS"] = "30"
        self.client.initialize()
        long_running = self.client.send("transcribe", self.transcribe_params())
        started = time.monotonic()
        self.assertEqual(self.client.call("health")["result"]["status"], "ok")
        self.assertIn("vectors", self.client.call("embed", {"texts": ["hi"], "kind": "query"})["result"])
        self.assertLess(time.monotonic() - started, 5)
        self.client.notify("$/cancelRequest", {"id": str(long_running)})
        self.client.wait(long_running)

    def test_queued_transcriptions_do_not_hold_up_embeddings(self):
        os.environ["VIDYOM_AI_FAKE_DELAY_SECONDS"] = "30"
        self.client.initialize()
        running = [self.client.send("transcribe", self.transcribe_params()) for _ in range(12)]
        started = time.monotonic()
        self.assertIn("vectors", self.client.call("embed", {"texts": ["hi"], "kind": "query"})["result"])
        self.assertLess(time.monotonic() - started, 5)
        for request_id in running:
            self.client.notify("$/cancelRequest", {"id": str(request_id)})
        for request_id in running:
            self.assertEqual(self.client.wait(request_id)["error"]["code"], protocol.CODE_REQUEST_CANCELLED)

    def test_disconnect_cancels_in_flight_work(self):
        os.environ["VIDYOM_AI_FAKE_DELAY_SECONDS"] = "30"
        self.client.initialize()
        self.client.send("transcribe", self.transcribe_params())
        time.sleep(0.1)
        self.client_side.shutdown(socket.SHUT_WR)  # as if the client went away
        deadline = time.monotonic() + 5
        while self.worker._in_flight and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertEqual(self.worker._in_flight, 0)


class UnencodableResultTest(ConnectionTestCase):
    """A client must always get an answer: it would otherwise wait forever."""

    def test_nan_in_a_result_answers_with_an_error(self):
        self.client.initialize()
        with mock.patch.object(self.worker.engine, "embed", return_value={"vectors": [[float("nan")]]}):
            response = self.client.call("embed", {"texts": ["x"], "kind": "query"})
        self.assertEqual(response["error"]["code"], protocol.CODE_INTERNAL_ERROR)

    def test_oversized_result_answers_with_an_error(self):
        self.client.initialize()
        with mock.patch("vidyom.server.MAX_MESSAGE_BYTES", 64):
            response = self.client.call("embed", {"texts": ["x"], "kind": "query"})
        self.assertEqual(response["error"]["code"], protocol.CODE_INTERNAL_ERROR)

    def test_an_engine_crash_answers_without_internals(self):
        self.client.initialize()
        with self.assertLogs("vidyom.aiworker", level="ERROR"):
            with mock.patch.object(self.worker.engine, "embed", side_effect=RuntimeError("secret detail")):
                response = self.client.call("embed", {"texts": ["x"], "kind": "query"})
        self.assertEqual(response["error"]["code"], protocol.CODE_INTERNAL_ERROR)
        self.assertNotIn("secret detail", response["error"]["message"])


class EmbedTest(ConnectionTestCase):
    def test_embed(self):
        self.client.initialize()
        result = self.client.call("embed", {"texts": ["conscious breathing", "!!!"], "kind": "passage"})["result"]
        self.assertEqual(result["model"], "fake-hash-embed@1")
        self.assertEqual(result["dimensions"], EMBEDDING_DIMENSIONS)
        self.assertEqual(len(result["vectors"]), 2)
        self.assertEqual(len(result["vectors"][0]), EMBEDDING_DIMENSIONS)

    def test_param_validation(self):
        self.client.initialize()
        for params in (
            {"kind": "query"},
            {"texts": [], "kind": "query"},
            {"texts": "hi", "kind": "query"},
            {"texts": [1], "kind": "query"},
            {"texts": ["x"]},
            {"texts": ["x"], "kind": "document"},
            {"texts": ["x"] * 65, "kind": "query"},
        ):
            self.assertEqual(self.client.call("embed", params)["error"]["code"], protocol.CODE_INVALID_PARAMS)

    def test_the_batch_limit_is_inclusive(self):
        self.client.initialize()
        self.assertEqual(len(self.client.call("embed", {"texts": ["x"] * 64, "kind": "query"})["result"]["vectors"]), 64)


class TokenTest(ConnectionTestCase):
    def test_wrong_token_closes_the_connection(self):
        client = self.connect(token="right", token_required=True)
        response = client.call("initialize", {"protocol_version": 1, "client": "t", "token": "wrong"})
        self.assertEqual(response["error"]["code"], protocol.CODE_UNAUTHORIZED)

    def test_missing_token(self):
        client = self.connect(token="right", token_required=True)
        response = client.call("initialize", {"protocol_version": 1, "client": "t"})
        self.assertEqual(response["error"]["code"], protocol.CODE_UNAUTHORIZED)

    def test_right_token(self):
        client = self.connect(token="right", token_required=True)
        self.assertEqual(client.initialize(token="right")["engine"], "fake")


class FramingTest(unittest.TestCase):
    def test_read_message(self):
        stream = BytesIO(b'{"a":1}\r\n{"b":2}\n')
        self.assertEqual(protocol.read_message(stream), b'{"a":1}')
        self.assertEqual(protocol.read_message(stream), b'{"b":2}')
        self.assertIsNone(protocol.read_message(stream))

    def test_oversized_line(self):
        with mock.patch.object(protocol, "MAX_MESSAGE_BYTES", 8):
            with self.assertRaises(protocol.MessageTooLarge):
                protocol.read_message(BytesIO(b"x" * 32 + b"\n"))

    def test_limit_matches_go(self):
        self.assertEqual(protocol.MAX_MESSAGE_BYTES, 64 << 20)

    def test_nan_is_refused(self):
        with self.assertRaises(ValueError):
            protocol.encode({"x": float("nan")})


class ListenerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = os.path.join(self.tmp.name, "ai.sock")

    def test_socket_mode_is_0600(self):
        listener = open_unix_listener(self.path)
        self.addCleanup(listener.close)
        self.assertEqual(stat.S_IMODE(os.stat(self.path).st_mode), 0o600)

    def test_refuses_to_replace_a_regular_file(self):
        Path(self.path).write_text("keep me")
        with self.assertRaises(OSError):
            open_unix_listener(self.path)
        self.assertEqual(Path(self.path).read_text(), "keep me")

    def test_replaces_a_stale_socket(self):
        stale = socket.socket(socket.AF_UNIX)
        stale.bind(self.path)
        stale.close()
        listener = open_unix_listener(self.path)
        self.addCleanup(listener.close)

    def test_refuses_a_live_socket(self):
        listener = open_unix_listener(self.path)
        self.addCleanup(listener.close)
        with self.assertRaises(OSError):
            open_unix_listener(self.path)

    def test_tcp_is_loopback_only(self):
        for host in ("0.0.0.0", "192.0.2.1", "::", "media.example.internal"):
            with self.assertRaises(ValueError, msg=host):
                open_tcp_listener(host, 0)
        listener = open_tcp_listener("127.0.0.1", 0)
        self.addCleanup(listener.close)


class ProcessTest(unittest.TestCase):
    """The real command line, as a separate process."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.audio = self.dir / "audio.wav"
        write_wav(self.audio, 5)

    def run_cli(self, *args):
        return subprocess.run(
            [sys.executable, "-m", "vidyom", *args],
            capture_output=True,
            env=worker_environment(),
            timeout=30,
            stdin=subprocess.DEVNULL,
        )

    def test_engine_is_required_and_has_no_default(self):
        done = self.run_cli("--stdio")
        self.assertEqual(done.returncode, 2)
        self.assertIn(b"--engine", done.stderr)

    def test_unknown_engine_is_refused(self):
        self.assertEqual(self.run_cli("--engine", "tiny", "--stdio").returncode, 2)

    def test_transport_is_required(self):
        self.assertEqual(self.run_cli("--engine", "fake").returncode, 2)

    def test_tcp_requires_a_token_file(self):
        done = self.run_cli("--engine", "fake", "--tcp", "127.0.0.1:0")
        self.assertNotEqual(done.returncode, 0)
        self.assertIn(b"--token-file", done.stderr)

    def test_tcp_refuses_an_empty_token_file(self):
        token = self.dir / "token"
        token.write_text("\n")
        done = self.run_cli("--engine", "fake", "--tcp", "127.0.0.1:0", "--token-file", str(token))
        self.assertNotEqual(done.returncode, 0)

    def test_fake_banner_goes_to_stderr(self):
        process = start_worker("--engine", "fake", "--stdio")
        stdout, stderr = process.communicate(timeout=10)
        self.assertEqual(stdout, b"")
        self.assertIn(b"FAKE ENGINE", stderr)

    def test_shutdown_finishes_in_flight_work_then_exits(self):
        process = start_worker("--engine", "fake", "--stdio", env={"VIDYOM_AI_FAKE_DELAY_SECONDS": "1"})
        self.addCleanup(close_process, process)
        client = RpcClient(process.stdout, process.stdin)
        client.initialize()
        running = client.send("transcribe", {"audio_path": str(self.audio), "word_timestamps": False})
        time.sleep(0.1)
        stopping = client.send("shutdown")
        late = client.send("embed", {"texts": ["x"], "kind": "query"})
        self.assertEqual(client.wait(late)["error"]["code"], protocol.CODE_UNAVAILABLE)
        self.assertEqual(client.wait(running)["result"]["engine"], "fake/canned@1")
        self.assertEqual(client.wait(stopping)["result"], {})
        self.assertEqual(process.wait(timeout=10), 0)

    def test_a_taken_socket_path_fails_with_a_message(self):
        path = self.dir / "ai.sock"
        path.write_text("not a socket")
        done = self.run_cli("--engine", "fake", "--socket", str(path))
        self.assertEqual(done.returncode, 1)
        self.assertIn(b"is not a socket", done.stderr)
        self.assertNotIn(b"Traceback", done.stderr)

    def test_unix_socket_end_to_end_and_sigterm(self):
        path = str(self.dir / "ai.sock")
        process = subprocess.Popen(
            [sys.executable, "-m", "vidyom", "--engine", "fake", "--socket", path],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            env=worker_environment(),
        )
        self.addCleanup(close_process, process)
        deadline = time.monotonic() + 15
        while not os.path.exists(path) and time.monotonic() < deadline:
            time.sleep(0.02)
        self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
        connection = socket.socket(socket.AF_UNIX)
        connection.connect(path)
        self.addCleanup(connection.close)
        client = RpcClient(connection.makefile("rb"), connection.makefile("wb"))
        client.initialize()
        result = client.call("transcribe", {"audio_path": str(self.audio), "word_timestamps": False})["result"]
        self.assertEqual(result["duration_seconds"], 5)
        process.terminate()
        self.assertEqual(process.wait(timeout=10), 0)
        self.assertFalse(os.path.exists(path))

    def test_tcp_end_to_end(self):
        token = self.dir / "token"
        token.write_text("fictional-test-token\n")
        process = subprocess.Popen(
            [sys.executable, "-m", "vidyom", "--engine", "fake", "--tcp", "127.0.0.1:0", "--token-file", str(token)],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            env=worker_environment(),
        )
        self.addCleanup(close_process, process)
        port = None
        for line in process.stderr:
            if b"listening on" in line:
                port = int(line.rsplit(b":", 1)[1])
                break
        self.assertIsNotNone(port)
        connection = socket.create_connection(("127.0.0.1", port), timeout=10)
        self.addCleanup(connection.close)
        client = RpcClient(connection.makefile("rb"), connection.makefile("wb"))
        response = client.call("initialize", {"protocol_version": 1, "client": "t"})
        self.assertEqual(response["error"]["code"], protocol.CODE_UNAUTHORIZED)
        self.assertTrue(client.wait_eof())

        connection = socket.create_connection(("127.0.0.1", port), timeout=10)
        self.addCleanup(connection.close)
        client = RpcClient(connection.makefile("rb"), connection.makefile("wb"))
        self.assertEqual(client.initialize(token="fictional-test-token")["engine"], "fake")
        process.terminate()
        self.assertEqual(process.wait(timeout=10), 0)


if __name__ == "__main__":
    unittest.main()
