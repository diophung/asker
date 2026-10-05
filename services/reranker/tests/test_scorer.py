from __future__ import annotations

import sys
from types import SimpleNamespace

import pytest

from reranker.scorer import CrossEncoderScorer, Scorer, resolve_device, use_fp16
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


def test_real_scorer_readiness_requires_warm_prediction_and_pins_revision(monkeypatch):
    observed = {}

    class FakeCrossEncoder:
        def __init__(self, name, **kwargs):
            observed.update(name=name, **kwargs)
            self.model = SimpleNamespace(
                config=SimpleNamespace(_commit_hash="a" * 40), device="mps:0",
                parameters=lambda: iter([SimpleNamespace(dtype="torch.float32")]),
            )

        def predict(self, pairs, **kwargs):
            observed["warmPairs"] = pairs
            return [0.5]

    fake_torch = SimpleNamespace(
        cuda=SimpleNamespace(is_available=lambda: False),
        backends=SimpleNamespace(mps=SimpleNamespace(is_available=lambda: True)),
    )
    monkeypatch.setitem(sys.modules, "torch", fake_torch)
    monkeypatch.setitem(
        sys.modules, "sentence_transformers", SimpleNamespace(CrossEncoder=FakeCrossEncoder)
    )
    scorer = CrossEncoderScorer("fake", device="mps", revision="a" * 40)
    assert scorer.identity()["device"] is None
    scorer.load()
    assert scorer.loaded
    assert observed["revision"] == "a" * 40
    assert len(observed["warmPairs"]) == 1
    assert scorer.identity()["device"] == "mps"
    assert scorer.identity()["precision"] == "fp32"
    assert scorer.identity()["resolvedRevision"] == "a" * 40


def test_mps_memory_is_sampled_unknown_when_unavailable_and_not_fingerprinted():
    scorer = CrossEncoderScorer("fake", device="mps", precision="fp32")
    scorer._loaded, scorer._device, scorer._actual_precision = True, "mps", "fp32"
    before = scorer.identity()
    assert before["memory"]["currentDriverAllocatedBytes"] is None
    scorer._torch = SimpleNamespace(mps=SimpleNamespace(
        current_allocated_memory=lambda: 123, driver_allocated_memory=lambda: 456,
    ))
    observed = scorer.identity()
    assert observed["memory"]["currentDriverAllocatedBytes"] == 456
    assert observed["memory"]["largestSampledDriverAllocatedBytes"] == 456
    scorer._torch.mps.driver_allocated_memory = lambda: 400
    final = scorer.identity()
    assert final["memory"]["currentDriverAllocatedBytes"] == 400
    assert final["memory"]["largestSampledDriverAllocatedBytes"] == 456
    assert before["fingerprint"] == observed["fingerprint"] == final["fingerprint"]
