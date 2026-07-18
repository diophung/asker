from __future__ import annotations

import pytest

from reranker.scorer import Scorer, resolve_device, use_fp16
from tests.conftest import FakeScorer


@pytest.mark.parametrize(
    ("pref", "cuda", "mps", "want"),
    [
        ("auto", True, True, "cuda"),
        ("auto", False, True, "mps"),
        ("auto", False, False, "cpu"),
        ("cuda", True, False, "cuda"),
        ("cuda", False, True, "cpu"),  # cuda requested but absent -> cpu (load() fails on explicit)
        ("mps", False, True, "mps"),
        ("mps", False, False, "cpu"),
        ("cpu", True, True, "cpu"),
    ],
)
def test_resolve_device(pref: str, cuda: bool, mps: bool, want: str) -> None:
    assert resolve_device(pref, cuda, mps) == want


@pytest.mark.parametrize(
    ("precision", "device", "want"),
    [
        ("auto", "cuda", True),
        ("fp16", "cuda", True),
        ("fp32", "cuda", False),
        ("auto", "mps", False),  # half only on cuda
        ("fp16", "cpu", False),
    ],
)
def test_use_fp16(precision: str, device: str, want: bool) -> None:
    assert use_fp16(precision, device) is want


def test_fake_scorer_satisfies_protocol() -> None:
    fs = FakeScorer()
    assert isinstance(fs, Scorer)
    assert fs.score("q", ["ab", "abcd"]) == [2.0, 4.0]
    assert fs.score("q", []) == []
