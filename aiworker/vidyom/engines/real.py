"""The real engine: faster-whisper large-v3 and BGE-M3 (D9).

Both libraries are optional extras (`vidyom[asr,embeddings]`), imported on first use
so the core stays stdlib-only (I37). When one is missing or its model cannot load,
the method fails with -32001 and health reports degraded. Nothing falls back to the
fake engine (I27).

No VAD runs: transcription covers the whole audio, so timestamps stay on the working
master's timeline with no map (I11, D21, D56).

Status: real inference has not been exercised on this project's target content.
Accuracy, word timestamps and CPU performance are open validations (§17.2, D9, D41).
"""

from __future__ import annotations

import importlib
import importlib.util
import logging
import threading
from typing import Any

from vidyom.engines.base import Description, RequestContext
from vidyom.protocol import CODE_INVALID_PARAMS, CODE_UNAVAILABLE, Cancelled, RpcError

_LOG = logging.getLogger("vidyom.aiworker")

DEFAULT_TRANSCRIPTION_MODEL = "large-v3"
DEFAULT_EMBEDDING_REPO = "BAAI/bge-m3"
EMBEDDING_MODEL_ID = "bge-m3@1"
EMBEDDING_DIMENSIONS = 1024
_EMBED_BATCH = 16


class RealEngine:
    """Loads each model once, and serializes inference per model (I200)."""

    def __init__(
        self,
        *,
        device: str = "auto",
        compute_type: str = "auto",
        transcription_model: str = DEFAULT_TRANSCRIPTION_MODEL,
        embedding_model_path: str | None = None,
        offline: bool = False,
    ) -> None:
        self._device = device
        self._compute_type = compute_type
        self._transcription_model = transcription_model
        self._embedding_source = embedding_model_path or DEFAULT_EMBEDDING_REPO
        self._offline = offline
        self._whisper: Any = None
        self._embedder: Any = None
        self._whisper_lock = threading.Lock()
        self._embedder_lock = threading.Lock()

    def describe(self) -> Description:
        return Description(
            engine="real",
            transcription_model=f"faster-whisper/{self._transcription_model}",
            embedding_model=EMBEDDING_MODEL_ID,
            embedding_dimensions=EMBEDDING_DIMENSIONS,
            device=self._device,
            capabilities=["transcribe", "embed", "progress", "cancel", "word_timestamps"],
        )

    def health(self) -> dict[str, Any]:
        missing = [
            name
            for name in ("faster_whisper", "sentence_transformers")
            if importlib.util.find_spec(name) is None
        ]
        loaded = []
        if self._whisper is not None:
            loaded.append(f"faster-whisper/{self._transcription_model}")
        if self._embedder is not None:
            loaded.append(EMBEDDING_MODEL_ID)
        result: dict[str, Any] = {"status": "degraded" if missing else "ok", "loaded_models": loaded}
        if missing:
            result["detail"] = "not installed: " + ", ".join(missing) + " (install vidyom[all])"
        return result

    def _load_whisper(self) -> Any:
        if self._whisper is None:
            try:
                module = importlib.import_module("faster_whisper")
                self._whisper = module.WhisperModel(
                    self._transcription_model,
                    device=self._device,
                    compute_type=self._compute_type,
                    local_files_only=self._offline,
                )
            except Exception as err:
                _LOG.exception("faster-whisper did not load")
                raise RpcError(
                    CODE_UNAVAILABLE,
                    f"faster-whisper {self._transcription_model} is not usable: {type(err).__name__}",
                ) from err
        return self._whisper

    def _load_embedder(self) -> Any:
        if self._embedder is None:
            try:
                module = importlib.import_module("sentence_transformers")
                device = None if self._device == "auto" else self._device
                self._embedder = module.SentenceTransformer(
                    self._embedding_source,
                    device=device,
                    local_files_only=self._offline,
                )
            except Exception as err:
                _LOG.exception("BGE-M3 did not load")
                raise RpcError(CODE_UNAVAILABLE, f"BGE-M3 is not usable: {type(err).__name__}") from err
        return self._embedder

    def transcribe(
        self,
        audio_path: str,
        language: str | None,
        word_timestamps: bool,
        ctx: RequestContext,
    ) -> dict[str, Any]:
        with self._whisper_lock:
            ctx.check_cancelled()
            model = self._load_whisper()
            try:
                # transcribe returns a lazy generator: the audio is decoded as it runs,
                # so cancellation is checked between segments.
                pieces, info = model.transcribe(
                    audio_path,
                    language=language or None,
                    word_timestamps=word_timestamps,
                    vad_filter=False,
                )
                duration = float(info.duration)
                segments = []
                for piece in pieces:
                    ctx.check_cancelled()
                    segments.append(_segment(piece, word_timestamps))
                    if duration > 0:
                        ctx.progress(100.0 * float(piece.end) / duration, "transcribing")
            except (RpcError, Cancelled):
                raise
            except Exception as err:
                # The client gets the type only; the cause stays in this worker's log.
                _LOG.exception("transcription failed")
                raise RpcError(CODE_UNAVAILABLE, f"transcription failed: {type(err).__name__}") from err
        ctx.progress(100.0, "transcribed")
        return {
            "language": str(info.language),
            "duration_seconds": duration,
            "engine": f"faster-whisper/{self._transcription_model}",
            "segments": segments,
        }

    def embed(self, texts: list[str], kind: str, ctx: RequestContext) -> dict[str, Any]:
        if kind not in ("passage", "query"):
            raise RpcError(CODE_INVALID_PARAMS, "kind must be passage or query")
        with self._embedder_lock:
            ctx.check_cancelled()
            model = self._load_embedder()
            try:
                # BGE-M3 takes no query or passage prefix, so kind does not change input.
                vectors = model.encode(
                    texts,
                    batch_size=_EMBED_BATCH,
                    normalize_embeddings=True,
                    show_progress_bar=False,
                )
            except Exception as err:
                _LOG.exception("embedding failed")
                raise RpcError(CODE_UNAVAILABLE, f"embedding failed: {type(err).__name__}") from err
        out = [[float(value) for value in vector] for vector in vectors]
        if out and len(out[0]) != EMBEDDING_DIMENSIONS:
            raise RpcError(CODE_UNAVAILABLE, f"embedding model returned {len(out[0])} dimensions")
        return {"model": EMBEDDING_MODEL_ID, "dimensions": EMBEDDING_DIMENSIONS, "vectors": out}


def _segment(piece: Any, word_timestamps: bool) -> dict[str, Any]:
    segment: dict[str, Any] = {
        "start": float(piece.start),
        "end": float(piece.end),
        "text": str(piece.text).strip(),
    }
    words = getattr(piece, "words", None)
    if word_timestamps and words:
        segment["words"] = [
            {"start": float(word.start), "end": float(word.end), "text": str(word.word).strip()} for word in words
        ]
    return segment
