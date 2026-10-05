"""Bind an operator's corpus/version prefix to verified runtime fingerprints."""
from __future__ import annotations

import re


def validate_reranker_identity(
    identity: dict, *, model: str, revision: str | None, device: str,
    precision: str, batch_size: int, max_length: int,
) -> None:
    """Reject profile changes that would silently reuse a different resident model."""
    expected = {"model": model, "batchSize": batch_size, "maxLength": max_length}
    if revision:
        expected["resolvedRevision"] = revision
    if device != "auto":
        expected["device"] = device
    if precision != "auto":
        expected["precision"] = precision
    mismatches = [key for key, value in expected.items() if identity.get(key) != value]
    if identity.get("status") != "ok" or identity.get("resident") is not True:
        mismatches.append("readiness")
    if identity.get("device") not in ("cpu", "cuda", "mps"):
        mismatches.append("actualDevice")
    if identity.get("precision") not in ("fp32", "fp16", "bf16"):
        mismatches.append("actualPrecision")
    if mismatches:
        # Name fields only; never echo model input, credentials or private prefixes.
        raise ValueError("Running reranker differs from this profile in " + ", ".join(mismatches)
                         + "; stop only the managed native process before switching")


def bind_namespace(prefix: str, embedding: dict, reranker: dict | None = None) -> str:
    if not isinstance(prefix, str) or not prefix:
        raise ValueError("A nonempty operator cache namespace is required")

    def fingerprint(identity: dict) -> str:
        value = identity.get("fingerprint")
        if not isinstance(value, str) or not re.fullmatch(r"[0-9a-fA-F]{64}", value):
            raise ValueError("Native identity fingerprint is invalid; cache namespace was not reused")
        if identity.get("status") != "ok" or identity.get("resident") is not True:
            raise ValueError("Native identity is not ready; cache namespace was not reused")
        return value.lower()

    return prefix + ":embed:" + fingerprint(embedding) + ":rerank:" + (
        fingerprint(reranker) if reranker is not None else "off"
    )
