from __future__ import annotations

import pytest

from reranker.config import DEFAULT_MODEL, DEFAULT_PORT, Config, ConfigError


def test_defaults() -> None:
    cfg = Config.from_env({})
    assert cfg.model == DEFAULT_MODEL
    assert cfg.port == DEFAULT_PORT
    assert cfg.device == "auto"
    assert cfg.host == "0.0.0.0"


def test_env_overrides() -> None:
    cfg = Config.from_env(
        {
            "RERANKER_MODEL": "Qwen/Qwen3-Reranker-0.6B",
            "RERANKER_DEVICE": "mps",
            "RERANKER_PORT": "host:9910",
            "RERANKER_MAX_DOCUMENTS": "50",
        }
    )
    assert cfg.model == "Qwen/Qwen3-Reranker-0.6B"
    assert cfg.device == "mps"
    assert (cfg.host, cfg.port) == ("host", 9910)
    assert cfg.max_documents == 50


def test_bare_port() -> None:
    cfg = Config.from_env({"RERANKER_PORT": "9955"})
    assert (cfg.host, cfg.port) == ("0.0.0.0", 9955)


@pytest.mark.parametrize(
    "env",
    [
        {"RERANKER_DEVICE": "tpu"},
        {"RERANKER_PRECISION": "int4"},
        {"RERANKER_MAX_DOCUMENTS": "0"},
        {"RERANKER_MAX_DOCUMENTS": "-3"},
        {"RERANKER_MAX_DOCUMENTS": "notint"},
        {"RERANKER_PORT": "host:notaport"},
        {"RERANKER_PORT": ":-1"},
    ],
)
def test_bad_values_raise(env: dict[str, str]) -> None:
    with pytest.raises(ConfigError):
        Config.from_env(env)
