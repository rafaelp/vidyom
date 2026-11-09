"""Command line of the AI worker. The engine and the transport must be named (I27, I200)."""

from __future__ import annotations

import argparse
import logging
import os
import signal
import sys
import threading

from vidyom import __version__
from vidyom.engines import create_engine
from vidyom.engines.fake import FAKE_NOTICE
from vidyom.server import Worker, serve_stdio, serve_tcp, serve_unix


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="vidyom-ai-worker",
        description="Vidyom AI worker: JSON-RPC 2.0 over a Unix socket, loopback TCP or stdio.",
    )
    parser.add_argument("--version", action="version", version=__version__)
    parser.add_argument(
        "--engine",
        choices=["fake", "real"],
        required=True,
        help="'real' runs faster-whisper and BGE-M3 (needs vidyom[all]). "
        "'fake' returns canned text for tests and the demo. There is no fallback between them.",
    )
    transport = parser.add_mutually_exclusive_group(required=True)
    transport.add_argument("--socket", metavar="PATH", help="serve on a Unix socket, created with mode 0600")
    transport.add_argument("--tcp", metavar="127.0.0.1:PORT", help="serve on loopback TCP; needs --token-file")
    transport.add_argument("--stdio", action="store_true", help="serve one client on stdin and stdout (tests)")
    parser.add_argument("--token-file", metavar="PATH", help="file holding the initialize token for --tcp")
    parser.add_argument("--device", default="auto", help="real engine: auto, cpu or cuda")
    parser.add_argument("--compute-type", default="auto", help="real engine: CTranslate2 compute type")
    parser.add_argument("--transcription-model", default="large-v3", help="real engine: faster-whisper model")
    parser.add_argument("--embedding-model-path", help="real engine: local path of BGE-M3 instead of the Hub")
    parser.add_argument("--offline", action="store_true", help="real engine: never download models")
    parser.add_argument("--log-level", default="INFO")
    return parser


def _read_token(path: str | None) -> str:
    if not path:
        raise SystemExit("--tcp requires --token-file")
    try:
        with open(path, encoding="utf-8") as handle:
            token = handle.read().strip()
    except OSError as err:
        raise SystemExit(f"cannot read the token file: {err.strerror}") from err
    if not token:
        raise SystemExit("the token file is empty")
    return token


def _parse_tcp(value: str) -> tuple[str, int]:
    host, _, port = value.rpartition(":")
    if not host or not port.isdigit():
        raise SystemExit("--tcp expects HOST:PORT, for example 127.0.0.1:7077")
    return host.strip("[]"), int(port)


def _stop_on_signals(worker: Worker) -> None:
    def stop(signum, _frame) -> None:
        threading.Thread(target=lambda: (worker.drain(), worker.close()), daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)


def _exit_process(code: int) -> None:
    """Ends the process without finalizing it. Connection threads are blocked reading
    their streams, and interpreter shutdown can abort on a stream a thread holds. All
    in-flight work has been drained or cancelled by now."""
    logging.shutdown()
    sys.stdout.flush()
    sys.stderr.flush()
    os._exit(code)


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    logging.basicConfig(stream=sys.stderr, level=args.log_level.upper(), format="%(levelname)s %(name)s %(message)s")

    options = {}
    if args.engine == "real":
        options = {
            "device": args.device,
            "compute_type": args.compute_type,
            "transcription_model": args.transcription_model,
            "embedding_model_path": args.embedding_model_path,
            "offline": args.offline,
        }
    engine = create_engine(args.engine, **options)
    if args.engine == "fake":
        print(f"vidyom-ai-worker: {FAKE_NOTICE}", file=sys.stderr, flush=True)

    described = engine.describe()
    logging.getLogger("vidyom.aiworker").info(
        "engine=%s transcription=%s embedding=%s device=%s",
        described.engine,
        described.transcription_model,
        described.embedding_model,
        described.device,
    )

    if args.stdio:
        serve_stdio(engine, sys.stdin.buffer, sys.stdout.buffer)
        _exit_process(0)
    try:
        if args.socket:
            serve_unix(engine, args.socket, on_listening=_stop_on_signals)
        else:
            host, port = _parse_tcp(args.tcp)
            token = _read_token(args.token_file)

            def listening(worker: Worker, bound_port: int) -> None:
                _stop_on_signals(worker)
                print(f"vidyom-ai-worker: listening on {host}:{bound_port}", file=sys.stderr, flush=True)

            serve_tcp(engine, host, port, token, on_listening=listening)
    except (OSError, ValueError) as err:
        print(f"vidyom-ai-worker: {err}", file=sys.stderr, flush=True)
        _exit_process(1)
    _exit_process(0)
    return 0


if __name__ == "__main__":
    sys.exit(main())
