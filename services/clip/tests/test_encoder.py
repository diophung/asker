"""Encoder helper tests (no torch): L2 normalization + base64 decode.

The real OpenClipEncoder forward pass is exercised by the integration smoke
(README), not here — importing torch/open_clip in unit tests is too heavy.
"""

from __future__ import annotations

import base64
import math

import pytest

from clip.encoder import EncodeError, decode_images_b64, l2_normalize


def test_l2_normalize_returns_unit_vector():
    out = l2_normalize([3.0, 4.0])
    assert math.isclose(math.sqrt(sum(x * x for x in out)), 1.0, rel_tol=1e-9)
    assert math.isclose(out[0], 0.6) and math.isclose(out[1], 0.8)


def test_l2_normalize_zero_vector_unchanged():
    assert l2_normalize([0.0, 0.0, 0.0]) == [0.0, 0.0, 0.0]


def test_l2_normalize_already_unit_is_stable():
    out = l2_normalize([1.0, 0.0, 0.0])
    assert out == [1.0, 0.0, 0.0]


def test_decode_images_b64_roundtrip():
    raws = [b"hello", b"\x00\x01\x02binary"]
    encoded = [base64.b64encode(r).decode() for r in raws]
    assert decode_images_b64(encoded) == raws


def test_decode_images_b64_strips_data_uri():
    encoded = "data:image/jpeg;base64," + base64.b64encode(b"jpegbytes").decode()
    assert decode_images_b64([encoded]) == [b"jpegbytes"]


def test_decode_images_b64_rejects_bad_base64():
    with pytest.raises(EncodeError, match="base64"):
        decode_images_b64(["%%%not base64%%%"])


def test_decode_images_b64_rejects_non_string():
    with pytest.raises(EncodeError, match="not a string"):
        decode_images_b64([123])  # type: ignore[list-item]
