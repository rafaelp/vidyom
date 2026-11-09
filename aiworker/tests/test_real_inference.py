"""Real models, only where the extras AND the models are already available.

Skipped everywhere else, including this repository's CI: running it downloads nothing
because it passes offline mode, and it needs VIDYOM_AI_REAL_TEST=1 to run at all.
"""

import importlib.util
import os
import tempfile
import unittest
from pathlib import Path

from tests.helpers import write_wav
from vidyom.engines.base import RequestContext
from vidyom.engines.real import RealEngine

ENABLED = os.environ.get("VIDYOM_AI_REAL_TEST") == "1"


@unittest.skipUnless(ENABLED and importlib.util.find_spec("faster_whisper"), "needs vidyom[asr] and VIDYOM_AI_REAL_TEST=1")
class RealWhisperTest(unittest.TestCase):
    def test_transcribes_silence_without_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            audio = Path(tmp) / "silence.wav"
            write_wav(audio, 2, rate=16000)
            result = RealEngine(offline=True).transcribe(str(audio), "en", True, RequestContext())
        self.assertEqual(result["engine"], "faster-whisper/large-v3")
        self.assertAlmostEqual(result["duration_seconds"], 2, places=0)


@unittest.skipUnless(
    ENABLED and importlib.util.find_spec("sentence_transformers"), "needs vidyom[embeddings] and VIDYOM_AI_REAL_TEST=1"
)
class RealBgeM3Test(unittest.TestCase):
    def test_similar_texts_score_higher(self):
        result = RealEngine(offline=True).embed(
            ["conscious breathing in yoga", "attention to the breath during practice", "quarterly tax filing deadline"],
            "passage",
            RequestContext(),
        )
        vectors = result["vectors"]
        self.assertEqual(len(vectors[0]), 1024)
        dot = lambda a, b: sum(x * y for x, y in zip(a, b))
        self.assertGreater(dot(vectors[0], vectors[1]), dot(vectors[0], vectors[2]))


if __name__ == "__main__":
    unittest.main()
