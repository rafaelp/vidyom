"""Audio duration without decoding. Standard library first, ffprobe when available."""

from __future__ import annotations

import json
import shutil
import struct
import subprocess
import wave
from pathlib import Path

_OGG_CAPTURE = b"OggS"
_TAIL_BYTES = 256 * 1024


class DurationUnknown(Exception):
    """No available method could read this file's duration."""


def audio_duration_seconds(path: str) -> float:
    """Returns the duration of an audio file, or raises DurationUnknown."""
    for reader in (_wav_duration, _ogg_duration, _ffprobe_duration):
        try:
            duration = reader(path)
        except (OSError, ValueError, EOFError, wave.Error, struct.error, subprocess.SubprocessError):
            continue
        if duration is not None and duration > 0:
            return duration
    raise DurationUnknown(Path(path).suffix or "no extension")


def _wav_duration(path: str) -> float | None:
    with open(path, "rb") as probe:
        head = probe.read(12)
    if head[:4] != b"RIFF" or head[8:12] != b"WAVE":
        return None
    with wave.open(path, "rb") as wav:
        return wav.getnframes() / wav.getframerate()


def _ogg_duration(path: str) -> float | None:
    """Reads Ogg's last granule position; supports Opus and Vorbis streams."""
    with open(path, "rb") as ogg:
        head = ogg.read(512)
        if head[:4] != _OGG_CAPTURE:
            return None
        rate, pre_skip = _ogg_timing(head)
        if rate is None:
            return None
        ogg.seek(0, 2)
        size = ogg.tell()
        ogg.seek(max(0, size - _TAIL_BYTES))
        tail = ogg.read()
    last = tail.rfind(_OGG_CAPTURE)
    if last < 0 or len(tail) < last + 14:
        return None
    granule = struct.unpack_from("<q", tail, last + 6)[0]
    if granule <= 0:
        return None
    return (granule - pre_skip) / rate


def _ogg_timing(head: bytes) -> tuple[int | None, int]:
    opus = head.find(b"OpusHead")
    if opus >= 0 and len(head) >= opus + 12:
        # Opus granules always count 48 kHz samples; pre-skip is at offset 10.
        return 48000, struct.unpack_from("<H", head, opus + 10)[0]
    vorbis = head.find(b"\x01vorbis")
    if vorbis >= 0 and len(head) >= vorbis + 16:
        return struct.unpack_from("<I", head, vorbis + 12)[0], 0
    return None, 0


def _ffprobe_duration(path: str) -> float | None:
    binary = shutil.which("ffprobe")
    if binary is None:
        return None
    done = subprocess.run(
        [binary, "-v", "error", "-show_entries", "format=duration", "-of", "json", path],
        capture_output=True,
        timeout=60,
        check=True,
    )
    return float(json.loads(done.stdout)["format"]["duration"])
