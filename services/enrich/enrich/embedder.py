"""TEI embedding client: token-budget batching, retries, dimension validation.

ADR-005: the embedding dimension is deploy-time configuration (EMBEDDING_DIM)
and every vector TEI returns is validated against it — feeding a wrong-size
vector to Vespa is strictly worse than failing loudly.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable, Sequence

import httpx

log = logging.getLogger("enrich.embedder")

# The TEI server runs with max-batch-tokens 4096; staying at <=3500 estimated
# tokens per call (chars/4) leaves headroom for the estimate being rough.
MAX_BATCH_TOKENS = 3500
MAX_BATCH_TEXTS = 16

_HTTP_ATTEMPTS = 3
_HTTP_BACKOFF_BASE = 0.5  # 0.5s, 1s between attempts (exponential, capped)
_HTTP_BACKOFF_CAP = 2.0
_HTTP_TIMEOUT = 30.0


class EmbeddingError(Exception):
    """TEI could not produce embeddings (after retries) or returned junk."""


class DimensionMismatchError(EmbeddingError):
    """TEI returned a vector whose length differs from EMBEDDING_DIM.

    This is operator misconfiguration (EMBEDDING_DIM disagreeing with the
    model TEI serves, ADR-005) — retrying cannot fix it.
    """


def estimate_tokens(text: str) -> int:
    """Estimate the token count of text as chars/4, minimum 1."""
    return max(1, (len(text) + 3) // 4)


def plan_batches(
    texts: Sequence[str],
    max_tokens: int = MAX_BATCH_TOKENS,
    max_texts: int = MAX_BATCH_TEXTS,
) -> list[list[int]]:
    """Group texts into TEI-call batches, returned as lists of indices.

    Greedy in input order: a batch holds at most max_texts texts and at most
    max_tokens estimated tokens. A single text whose estimate alone exceeds
    the budget still ships (alone) — it cannot be split here, and TEI will
    truncate or reject it explicitly.
    """
    batches: list[list[int]] = []
    current: list[int] = []
    current_tokens = 0
    for i, text in enumerate(texts):
        tokens = estimate_tokens(text)
        if current and (current_tokens + tokens > max_tokens or len(current) >= max_texts):
            batches.append(current)
            current, current_tokens = [], 0
        current.append(i)
        current_tokens += tokens
    if current:
        batches.append(current)
    return batches


class Embedder:
    """Async client for TEI's POST /embed and GET /health endpoints."""

    def __init__(
        self,
        tei_url: str,
        embedding_dim: int,
        client: httpx.AsyncClient | None = None,
        attempts: int = _HTTP_ATTEMPTS,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        if embedding_dim <= 0:
            raise ValueError(f"embedding_dim must be positive, got {embedding_dim}")
        self._url = tei_url.rstrip("/")
        self._dim = embedding_dim
        self._owns_client = client is None
        self._client = client or httpx.AsyncClient(timeout=httpx.Timeout(_HTTP_TIMEOUT))
        self._attempts = attempts
        self._sleep = sleep

    @property
    def embedding_dim(self) -> int:
        return self._dim

    async def embed(self, texts: Sequence[str]) -> list[list[float]]:
        """Embed texts (order-preserving), batching by token budget and count.

        Every returned vector is validated to have exactly EMBEDDING_DIM
        elements; a mismatch raises DimensionMismatchError.
        """
        vectors: list[list[float] | None] = [None] * len(texts)
        for batch in plan_batches(texts):
            inputs = [texts[i] for i in batch]
            data = await self._post_with_retry(inputs)
            if not isinstance(data, list) or len(data) != len(inputs):
                raise EmbeddingError(
                    f"TEI /embed returned {self._shape(data)} for {len(inputs)} inputs"
                )
            for idx, vec in zip(batch, data, strict=True):
                if not isinstance(vec, list) or len(vec) != self._dim:
                    raise DimensionMismatchError(
                        f"TEI returned a vector of length {self._shape(vec)}, want "
                        f"EMBEDDING_DIM={self._dim}; EMBEDDING_DIM must match the model "
                        f"TEI serves (ADR-005)"
                    )
                vectors[idx] = [float(x) for x in vec]
        return [v for v in vectors if v is not None]

    async def healthy(self) -> tuple[bool, str]:
        """Probe TEI's /health endpoint (used by the worker's /readyz)."""
        try:
            resp = await self._client.get(f"{self._url}/health")
        except httpx.HTTPError as exc:
            return False, f"tei unreachable: {exc}"
        if resp.status_code == 200:
            return True, "ok"
        return False, f"tei /health returned status {resp.status_code}"

    async def aclose(self) -> None:
        if self._owns_client:
            await self._client.aclose()

    async def _post_with_retry(self, inputs: list[str]) -> object:
        last_exc: Exception | None = None
        for attempt in range(1, self._attempts + 1):
            if attempt > 1:
                delay = min(_HTTP_BACKOFF_BASE * 2 ** (attempt - 2), _HTTP_BACKOFF_CAP)
                await self._sleep(delay)
            try:
                resp = await self._client.post(f"{self._url}/embed", json={"inputs": inputs})
                resp.raise_for_status()
                return resp.json()
            except (httpx.HTTPError, ValueError) as exc:
                last_exc = exc
                log.warning(
                    "TEI /embed call failed",
                    extra={
                        "attempt": attempt,
                        "max_attempts": self._attempts,
                        "inputs": len(inputs),
                        "error": str(exc),
                    },
                )
        raise EmbeddingError(
            f"TEI /embed failed after {self._attempts} attempts: {last_exc}"
        ) from last_exc

    @staticmethod
    def _shape(value: object) -> str:
        if isinstance(value, list):
            return f"list of {len(value)}"
        return type(value).__name__
