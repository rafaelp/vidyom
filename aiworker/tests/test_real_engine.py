"""The real engine's adapter logic, with stub libraries. Nothing here loads a model.

Real inference runs in test_real_inference.py only where the extras are installed.
"""

import sys
import threading
import types
import unittest
from unittest import mock

from vidyom.engines import create_engine
from vidyom.engines.base import RequestContext
from vidyom.engines.real import EMBEDDING_DIMENSIONS, EMBEDDING_MODEL_ID, RealEngine
from vidyom.protocol import CODE_UNAVAILABLE, Cancelled, RpcError


class FakeWhisperModel:
    instances = 0
    calls = []

    def __init__(self, model, device, compute_type, local_files_only):
        FakeWhisperModel.instances += 1
        self.model = model
        self.local_files_only = local_files_only

    def transcribe(self, path, language, word_timestamps, vad_filter):
        FakeWhisperModel.calls.append({"path": path, "language": language, "vad_filter": vad_filter})
        words = [types.SimpleNamespace(start=0.0, end=0.5, word=" Hello"), types.SimpleNamespace(start=0.5, end=1.0, word=" world.")]

        def pieces():
            yield types.SimpleNamespace(start=0.0, end=1.0, text=" Hello world. ", words=words if word_timestamps else None)
            yield types.SimpleNamespace(start=1.0, end=2.0, text=" Again. ", words=None)

        return pieces(), types.SimpleNamespace(duration=2.0, language="en")


class FakeSentenceTransformer:
    instances = 0

    def __init__(self, source, device, local_files_only):
        FakeSentenceTransformer.instances += 1

    def encode(self, texts, batch_size, normalize_embeddings, show_progress_bar):
        assert normalize_embeddings
        return [[1.0] + [0.0] * (EMBEDDING_DIMENSIONS - 1) for _ in texts]


def stub_modules():
    return {
        "faster_whisper": types.SimpleNamespace(WhisperModel=FakeWhisperModel),
        "sentence_transformers": types.SimpleNamespace(SentenceTransformer=FakeSentenceTransformer),
    }


class RealEngineWithStubsTest(unittest.TestCase):
    def setUp(self):
        patched = mock.patch.dict(sys.modules, stub_modules())
        patched.start()
        self.addCleanup(patched.stop)
        FakeWhisperModel.instances = 0
        FakeWhisperModel.calls = []
        FakeSentenceTransformer.instances = 0

    def test_is_not_labeled_fake(self):
        described = RealEngine().describe()
        self.assertEqual(described.engine, "real")
        self.assertEqual(described.embedding_model, "bge-m3@1")
        self.assertEqual(described.embedding_dimensions, 1024)
        self.assertNotIn("fake", described.capabilities)

    def test_transcribe_maps_segments_and_words(self):
        result = RealEngine().transcribe("/data/a.opus", "pt", True, RequestContext())
        self.assertEqual(result["engine"], "faster-whisper/large-v3")
        self.assertEqual(result["language"], "en")
        self.assertEqual(result["duration_seconds"], 2.0)
        self.assertEqual(result["segments"][0]["text"], "Hello world.")
        self.assertEqual(result["segments"][0]["words"][1], {"start": 0.5, "end": 1.0, "text": "world."})
        self.assertNotIn("words", result["segments"][1])

    def test_no_vad_runs(self):
        # VAD would need a timestamp map (I11); the whole audio keeps the timeline trivial.
        RealEngine().transcribe("/data/a.opus", None, False, RequestContext())
        self.assertFalse(FakeWhisperModel.calls[0]["vad_filter"])
        self.assertIsNone(FakeWhisperModel.calls[0]["language"])

    def test_models_load_once(self):
        engine = RealEngine()
        for _ in range(3):
            engine.transcribe("/data/a.opus", None, False, RequestContext())
            engine.embed(["x"], "query", RequestContext())
        self.assertEqual(FakeWhisperModel.instances, 1)
        self.assertEqual(FakeSentenceTransformer.instances, 1)

    def test_cancel_between_segments(self):
        event = threading.Event()

        def stop_after_first(percent, message):
            event.set()

        with self.assertRaises(Cancelled):
            RealEngine().transcribe("/data/a.opus", None, False, RequestContext(event, stop_after_first))

    def test_embed(self):
        result = RealEngine().embed(["a", "b"], "passage", RequestContext())
        self.assertEqual(result["model"], EMBEDDING_MODEL_ID)
        self.assertEqual(len(result["vectors"]), 2)
        self.assertEqual(len(result["vectors"][0]), EMBEDDING_DIMENSIONS)

    def test_wrong_dimensions_fail(self):
        class Tiny(FakeSentenceTransformer):
            def encode(self, texts, batch_size, normalize_embeddings, show_progress_bar):
                return [[1.0, 0.0] for _ in texts]

        modules = stub_modules()
        modules["sentence_transformers"] = types.SimpleNamespace(SentenceTransformer=Tiny)
        with mock.patch.dict(sys.modules, modules):
            with self.assertRaises(RpcError) as caught:
                RealEngine().embed(["a"], "passage", RequestContext())
        self.assertEqual(caught.exception.code, CODE_UNAVAILABLE)


class RealEngineWithoutLibrariesTest(unittest.TestCase):
    """No fallback: a missing library is an error, never the fake engine (I27)."""

    def setUp(self):
        patched = mock.patch.dict(sys.modules, {"faster_whisper": None, "sentence_transformers": None})
        patched.start()
        self.addCleanup(patched.stop)

    def test_transcribe_is_unavailable(self):
        with self.assertRaises(RpcError) as caught:
            RealEngine().transcribe("/data/a.opus", None, False, RequestContext())
        self.assertEqual(caught.exception.code, CODE_UNAVAILABLE)

    def test_embed_is_unavailable(self):
        with self.assertRaises(RpcError) as caught:
            RealEngine().embed(["a"], "query", RequestContext())
        self.assertEqual(caught.exception.code, CODE_UNAVAILABLE)

    def test_health_is_degraded(self):
        with mock.patch("importlib.util.find_spec", return_value=None):
            health = RealEngine().health()
        self.assertEqual(health["status"], "degraded")
        self.assertIn("faster_whisper", health["detail"])


class CreateEngineTest(unittest.TestCase):
    def test_unknown_engine(self):
        with self.assertRaises(ValueError):
            create_engine("whisper-tiny")

    def test_names(self):
        self.assertEqual(create_engine("fake").describe().engine, "fake")
        self.assertEqual(create_engine("real").describe().engine, "real")


if __name__ == "__main__":
    unittest.main()
