"""THE FAKE ENGINE. It does not transcribe anything and understands nothing (I27).

Transcription returns canned text, or a fixture, whatever the audio says. Embeddings
are feature hashes that only find shared words. It exists so tests and the demo run
with no ML dependencies and no model downloads. It proves the path, never quality:
"no literal match required" (§15.2) stays a validation for the real models.

It is selected only by an explicit `--engine fake`, and it is labeled on everything
it returns (I202).
"""

from __future__ import annotations

import json
import math
import os
import unicodedata
from typing import Any

from vidyom.engines.audio import DurationUnknown, audio_duration_seconds
from vidyom.engines.base import Description, RequestContext
from vidyom.protocol import CODE_INVALID_PARAMS, CODE_UNAVAILABLE, RpcError

FAKE_NOTICE = (
    "FAKE ENGINE: transcripts are canned text unrelated to the audio and embeddings "
    "only match shared words. Not for real content."
)

CANNED_ENGINE = "fake/canned@1"
FIXTURE_ENGINE = "fake/fixture@1"
EMBEDDING_MODEL = "fake-hash-embed@1"
EMBEDDING_DIMENSIONS = 1024

_FNV_OFFSET = 2166136261
_FNV_PRIME = 16777619
_PROGRESS_STEPS = 10

# The spec's example stories (§3.2, §15.2), so the scenarios have something to find.
# Fictional text written for this engine.
CANNED_SENTENCES = [
    "[FAKE ENGINE] This transcript is canned text from the Vidyom fake engine, not a transcription of this audio.",
    "Welcome to this Hatha Yoga class, today we will work with the breath before we move the body.",
    "Please sit comfortably, let the shoulders fall and close your eyes.",
    "Let us begin with conscious breathing, simply noticing the air as it comes in and goes out.",
    "Conscious breathing means that you are present for each inhale and each exhale.",
    "Now I will explain Ujjayi breathing, which is sometimes called the victorious breath.",
    "In Ujjayi breathing you narrow the back of the throat, so the breath makes a soft sound like the sea.",
    "Keep the mouth closed and let the sound guide the rhythm of the whole practice.",
    "That is the explanation of Ujjayi breathing, and we will use it during the postures today.",
    "Before the postures I want to tell you the story of a ninety year old woman I met in my first years of teaching.",
    "This 90-year-old woman came to class every week, and one day a student asked her about her life.",
    "She said she had made many mistakes, had loved the wrong people and had moved across the world twice.",
    "We all expected some regret, but she looked at us and said she had no remorse at all.",
    "She had no remorse, she told us, because every choice had been the best one she knew how to make at the time.",
    "That is why I remember the 90-year-old woman who had no remorse whenever a student is too hard on herself.",
    "Now we move slowly into the first posture, lifting the arms as we inhale.",
    "Exhale and fold forward, letting the head hang heavy and the knees stay soft.",
    "Inhale halfway up with a long spine, and exhale to fold again.",
    "While you hold this posture, I want to share the case of a friend who never got a hug.",
    "I had a friend who grew up in a house where nobody ever touched anybody, and he never got a hug as a child.",
    "When he was forty years old, he came to a retreat and a stranger hugged him after the closing circle.",
    "He stood completely still and then he cried, because he realized it was the first hug he could remember.",
    "My friend who never got a hug now hugs everybody he meets, and that is how the story ended.",
    "Come back to your breath now, and release the posture when you are ready.",
    "Lie down on your back for the final relaxation and let the whole body become heavy.",
    "Stay here for a few breaths, with nothing to do and nowhere to go.",
    "Thank you for practicing today, and I will see you in the next class.",
]


def fake_hash_embed(text: str) -> list[float]:
    """fake-hash-embed@1, schemas/ai-worker-rpc.v1.json#/$defs/fakeHashEmbed (I27).

    Lowercases one code point at a time, as Go's strings.ToLower does, rather than
    with str.lower, which treats `İ` and word-final `Σ` differently (I203).
    """
    sums = [0.0] * EMBEDDING_DIMENSIONS
    token: list[str] = []

    def flush() -> None:
        if not token:
            return
        value = _FNV_OFFSET
        for byte in "".join(token).encode("utf-8"):
            value ^= byte
            value = (value * _FNV_PRIME) & 0xFFFFFFFF
        sums[value % EMBEDDING_DIMENSIONS] += -1.0 if value & (1 << 31) else 1.0
        token.clear()

    for char in text:
        lowered = char.lower()[0]
        if unicodedata.category(lowered)[0] in ("L", "N"):
            token.append(lowered)
        else:
            flush()
    flush()

    norm = math.sqrt(sum(value * value for value in sums))
    if norm == 0:
        return sums
    return [value / norm for value in sums]


def _timed_words(text: str, start: float, end: float) -> list[dict[str, Any]]:
    """Spreads words over a span in proportion to their length."""
    words = text.split()
    total = sum(len(word) for word in words) or 1
    span = end - start
    out = []
    position = start
    for word in words:
        step = span * len(word) / total
        out.append({"start": round(position, 3), "end": round(position + step, 3), "text": word})
        position += step
    if out:
        out[-1]["end"] = round(end, 3)
    return out


def scaled_canned_segments(duration: float, word_timestamps: bool) -> list[dict[str, Any]]:
    """The canned text spread over the audio's duration, by sentence length."""
    total = sum(len(sentence) for sentence in CANNED_SENTENCES)
    segments = []
    position = 0.0
    for index, sentence in enumerate(CANNED_SENTENCES):
        last = index == len(CANNED_SENTENCES) - 1
        end = duration if last else position + duration * len(sentence) / total
        segment: dict[str, Any] = {"start": round(position, 3), "end": round(end, 3), "text": sentence}
        if word_timestamps:
            segment["words"] = _timed_words(sentence, position, end)
        segments.append(segment)
        position = end
    return segments


class FakeEngine:
    """See the module docstring: this engine fakes everything, by design."""

    def describe(self) -> Description:
        return Description(
            engine="fake",
            transcription_model=CANNED_ENGINE,
            embedding_model=EMBEDDING_MODEL,
            embedding_dimensions=EMBEDDING_DIMENSIONS,
            device="cpu",
            capabilities=["transcribe", "embed", "progress", "cancel", "word_timestamps", "fake"],
        )

    def health(self) -> dict[str, Any]:
        return {"status": "ok", "loaded_models": [CANNED_ENGINE, EMBEDDING_MODEL], "detail": FAKE_NOTICE}

    def transcribe(
        self,
        audio_path: str,
        language: str | None,
        word_timestamps: bool,
        ctx: RequestContext,
    ) -> dict[str, Any]:
        fixture = os.environ.get("VIDYOM_AI_FAKE_TRANSCRIPT")
        if fixture:
            return self._from_fixture(fixture, audio_path, word_timestamps, ctx)
        try:
            duration = audio_duration_seconds(audio_path)
        except DurationUnknown as err:
            raise RpcError(
                CODE_UNAVAILABLE,
                f"fake engine cannot read the audio duration ({err}): install ffprobe or use WAV or Ogg",
            ) from err
        self._simulate(ctx)
        return {
            "language": "en",
            "duration_seconds": round(duration, 3),
            "engine": CANNED_ENGINE,
            "segments": scaled_canned_segments(duration, word_timestamps),
        }

    def _from_fixture(
        self, fixture: str, audio_path: str, word_timestamps: bool, ctx: RequestContext
    ) -> dict[str, Any]:
        try:
            with open(fixture, encoding="utf-8") as handle:
                data = json.load(handle)
            segments = data["segments"]
        except (OSError, ValueError, KeyError) as err:
            raise RpcError(CODE_UNAVAILABLE, "fake engine cannot read VIDYOM_AI_FAKE_TRANSCRIPT") from err
        self._simulate(ctx)
        out = []
        for segment in segments:
            item = {"start": segment["start"], "end": segment["end"], "text": segment["text"]}
            if word_timestamps:
                item["words"] = segment.get("words") or _timed_words(segment["text"], segment["start"], segment["end"])
            if segment.get("speaker"):
                item["speaker"] = segment["speaker"]
            out.append(item)
        try:
            duration = audio_duration_seconds(audio_path)
        except DurationUnknown:
            duration = max((segment["end"] for segment in out), default=0.0)
        return {
            "language": data.get("language", "en"),
            "duration_seconds": round(duration, 3),
            "engine": FIXTURE_ENGINE,
            "segments": out,
        }

    def _simulate(self, ctx: RequestContext) -> None:
        """Reports progress; VIDYOM_AI_FAKE_DELAY_SECONDS stretches it for cancel tests."""
        delay = float(os.environ.get("VIDYOM_AI_FAKE_DELAY_SECONDS", "0") or 0)
        for step in range(_PROGRESS_STEPS):
            ctx.check_cancelled()
            ctx.progress(100.0 * step / _PROGRESS_STEPS, "fake transcription")
            if delay > 0:
                ctx.wait(delay / _PROGRESS_STEPS)
        ctx.check_cancelled()
        ctx.progress(100.0, "fake transcription")

    def embed(self, texts: list[str], kind: str, ctx: RequestContext) -> dict[str, Any]:
        if kind not in ("passage", "query"):
            raise RpcError(CODE_INVALID_PARAMS, "kind must be passage or query")
        vectors = []
        for text in texts:
            ctx.check_cancelled()
            vectors.append(fake_hash_embed(text))
        return {"model": EMBEDDING_MODEL, "dimensions": EMBEDDING_DIMENSIONS, "vectors": vectors}
