import json
import math
import os
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

from tests.helpers import RPC_SCHEMA, write_ogg_opus, write_wav
from vidyom.engines.audio import DurationUnknown, audio_duration_seconds
from vidyom.engines.base import RequestContext
from vidyom.engines.fake import (
    CANNED_ENGINE,
    CANNED_SENTENCES,
    EMBEDDING_DIMENSIONS,
    EMBEDDING_MODEL,
    FIXTURE_ENGINE,
    FakeEngine,
    fake_hash_embed,
    scaled_canned_segments,
)
from vidyom.protocol import CODE_UNAVAILABLE, Cancelled, RpcError


class FakeHashEmbedTest(unittest.TestCase):
    def test_schema_vectors(self):
        vectors = json.loads(RPC_SCHEMA.read_text(encoding="utf-8"))["$defs"]["fakeHashEmbed"]["testVectors"]
        self.assertGreaterEqual(len(vectors), 4)
        for vector in vectors:
            got = {str(i): value for i, value in enumerate(fake_hash_embed(vector["text"])) if value != 0}
            self.assertEqual(set(got), set(vector["nonZeroBuckets"]), vector["text"])
            for bucket, value in vector["nonZeroBuckets"].items():
                self.assertAlmostEqual(got[bucket], value, places=6, msg=vector["text"])

    def test_normalized(self):
        vector = fake_hash_embed("conscious breathing in a yoga class")
        self.assertEqual(len(vector), EMBEDDING_DIMENSIONS)
        self.assertAlmostEqual(math.sqrt(sum(value * value for value in vector)), 1.0, places=9)

    def test_no_tokens_is_all_zero(self):
        self.assertTrue(all(value == 0 for value in fake_hash_embed("!!! ... ---")))

    def test_case_and_punctuation_do_not_matter(self):
        self.assertEqual(fake_hash_embed("Conscious, BREATHING!"), fake_hash_embed("conscious breathing"))


class AudioDurationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)

    def test_wav(self):
        path = self.dir / "a.wav"
        write_wav(path, 2.5)
        self.assertAlmostEqual(audio_duration_seconds(str(path)), 2.5, places=3)

    def test_ogg_opus_subtracts_pre_skip(self):
        path = self.dir / "a.opus"
        write_ogg_opus(path, 12.0, pre_skip=312)
        self.assertAlmostEqual(audio_duration_seconds(str(path)), 12.0, places=3)

    def test_unknown_without_ffprobe(self):
        path = self.dir / "a.bin"
        path.write_bytes(b"not audio at all")
        with mock.patch("shutil.which", return_value=None):
            with self.assertRaises(DurationUnknown):
                audio_duration_seconds(str(path))


class CannedTranscriptTest(unittest.TestCase):
    def test_covers_the_whole_duration_in_order(self):
        for duration in (0.5, 7.0, 60.0, 10800.0):
            segments = scaled_canned_segments(duration, True)
            self.assertEqual(segments[0]["start"], 0)
            self.assertEqual(segments[-1]["end"], round(duration, 3))
            for previous, current in zip(segments, segments[1:]):
                self.assertLessEqual(previous["start"], previous["end"])
                self.assertAlmostEqual(previous["end"], current["start"], places=2)
            for segment in segments:
                self.assertTrue(segment["words"])
                self.assertAlmostEqual(segment["words"][-1]["end"], segment["end"], places=2)

    def test_contains_the_spec_stories(self):
        text = " ".join(CANNED_SENTENCES)
        for phrase in ("90-year-old woman who had no remorse", "friend who never got a hug", "Ujjayi breathing", "conscious breathing"):
            self.assertIn(phrase, text)

    def test_declares_itself_fake(self):
        self.assertIn("FAKE ENGINE", CANNED_SENTENCES[0])


class FakeEngineTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.audio = self.dir / "a.wav"
        write_wav(self.audio, 30)
        patched = mock.patch.dict(os.environ)
        patched.start()
        self.addCleanup(patched.stop)
        os.environ.pop("VIDYOM_AI_FAKE_TRANSCRIPT", None)
        os.environ.pop("VIDYOM_AI_FAKE_DELAY_SECONDS", None)

    def test_labels_everything(self):
        engine = FakeEngine()
        described = engine.describe()
        self.assertEqual(described.engine, "fake")
        self.assertIn("fake", described.capabilities)
        self.assertIn("FAKE ENGINE", engine.health()["detail"])
        result = engine.transcribe(str(self.audio), None, True, RequestContext())
        self.assertEqual(result["engine"], CANNED_ENGINE)
        self.assertEqual(engine.embed(["x"], "query", RequestContext())["model"], EMBEDDING_MODEL)

    def test_reports_the_texts_language_not_the_requested_one(self):
        result = FakeEngine().transcribe(str(self.audio), "pt-BR", False, RequestContext())
        self.assertEqual(result["language"], "en")
        self.assertNotIn("words", result["segments"][0])

    def test_deterministic(self):
        engine = FakeEngine()
        first = engine.transcribe(str(self.audio), None, True, RequestContext())
        second = engine.transcribe(str(self.audio), None, True, RequestContext())
        self.assertEqual(first, second)

    def test_unknown_duration_fails_instead_of_inventing_a_timeline(self):
        path = self.dir / "a.bin"
        path.write_bytes(b"not audio at all")
        with mock.patch("shutil.which", return_value=None):
            with self.assertRaises(RpcError) as caught:
                FakeEngine().transcribe(str(path), None, False, RequestContext())
        self.assertEqual(caught.exception.code, CODE_UNAVAILABLE)

    def test_fixture(self):
        fixture = self.dir / "fixture.json"
        fixture.write_text(
            json.dumps(
                {
                    "language": "pt",
                    "segments": [
                        {"start": 1.0, "end": 2.0, "text": "Olá mundo.", "speaker": "A"},
                        {"start": 2.0, "end": 3.0, "text": "Até logo.", "words": [{"start": 2.0, "end": 3.0, "text": "Até"}]},
                    ],
                }
            ),
            encoding="utf-8",
        )
        os.environ["VIDYOM_AI_FAKE_TRANSCRIPT"] = str(fixture)
        result = FakeEngine().transcribe(str(self.audio), None, True, RequestContext())
        self.assertEqual(result["engine"], FIXTURE_ENGINE)
        self.assertEqual(result["language"], "pt")
        self.assertEqual(result["duration_seconds"], 30)
        self.assertEqual(result["segments"][0]["speaker"], "A")
        self.assertEqual(result["segments"][0]["words"][-1]["text"], "mundo.")
        self.assertEqual(result["segments"][1]["words"], [{"start": 2.0, "end": 3.0, "text": "Até"}])

    def test_unreadable_fixture_is_unavailable(self):
        os.environ["VIDYOM_AI_FAKE_TRANSCRIPT"] = str(self.dir / "missing.json")
        with self.assertRaises(RpcError) as caught:
            FakeEngine().transcribe(str(self.audio), None, False, RequestContext())
        self.assertEqual(caught.exception.code, CODE_UNAVAILABLE)

    def test_progress_and_cancel(self):
        seen = []
        FakeEngine().transcribe(str(self.audio), None, False, RequestContext(progress=lambda p, m: seen.append(p)))
        self.assertEqual(seen[-1], 100.0)
        self.assertEqual(seen, sorted(seen))
        event = threading.Event()
        event.set()
        with self.assertRaises(Cancelled):
            FakeEngine().transcribe(str(self.audio), None, False, RequestContext(event))
