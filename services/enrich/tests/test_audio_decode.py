"""Exercise the actual faster-whisper/PyAV boundary without model inference."""

import io
from pathlib import Path

import numpy as np
from faster_whisper.audio import decode_audio


def test_committed_video_fixture_decodes_without_loading_a_model():
    # The live M3 fixture failed here before transcription: PyAV 19 removed
    # metadata_errors, still passed by faster-whisper 1.2.1. Real decoding
    # verifies the installed dependency pair, rather than mocking av.open.
    fixture = Path(__file__).resolve().parents[3] / "tools/e2e/fixtures/media/speech.mp4"
    audio = decode_audio(io.BytesIO(fixture.read_bytes()), sampling_rate=16000)
    assert audio.dtype == np.float32
    assert audio.ndim == 1
    assert 16000 < audio.size < 16000 * 60
    assert np.isfinite(audio).all()
    assert np.max(np.abs(audio)) > 0.01
