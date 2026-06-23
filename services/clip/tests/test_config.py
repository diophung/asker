"""Config.from_env tests: defaults, CLIP_DIM/port parsing, validation."""

from __future__ import annotations

import pytest

from clip.config import (
    DEFAULT_DIM,
    DEFAULT_MODEL,
    DEFAULT_PRETRAINED,
    Config,
    ConfigError,
)


def test_defaults_when_env_empty():
    cfg = Config.from_env({})
    assert cfg.model == DEFAULT_MODEL
    assert cfg.pretrained == DEFAULT_PRETRAINED
    assert cfg.clip_dim == DEFAULT_DIM
    assert cfg.host == "0.0.0.0"
    assert cfg.port == 9800
    assert cfg.device == "auto"
    assert cfg.precision == "auto"


def test_device_and_precision_parse_and_lowercase():
    cfg = Config.from_env({"CLIP_DEVICE": "CUDA", "CLIP_PRECISION": "FP16"})
    assert cfg.device == "cuda"
    assert cfg.precision == "fp16"


def test_device_invalid_is_config_error():
    with pytest.raises(ConfigError, match="CLIP_DEVICE"):
        Config.from_env({"CLIP_DEVICE": "gpu"})
    with pytest.raises(ConfigError, match="CLIP_PRECISION"):
        Config.from_env({"CLIP_PRECISION": "bf16"})


def test_overrides_model_and_dim():
    cfg = Config.from_env(
        {"CLIP_MODEL": "ViT-L-14", "CLIP_PRETRAINED": "laion2b_s32b_b82k", "CLIP_DIM": "768"}
    )
    assert cfg.model == "ViT-L-14"
    assert cfg.pretrained == "laion2b_s32b_b82k"
    assert cfg.clip_dim == 768


def test_clip_port_host_and_port_forms():
    assert Config.from_env({"CLIP_PORT": ":9000"}).port == 9000
    cfg = Config.from_env({"CLIP_PORT": "127.0.0.1:9001"})
    assert cfg.host == "127.0.0.1" and cfg.port == 9001
    # A bare number is treated as a port.
    assert Config.from_env({"CLIP_PORT": "9002"}).port == 9002


def test_clip_dim_must_be_positive_integer():
    with pytest.raises(ConfigError, match="CLIP_DIM"):
        Config.from_env({"CLIP_DIM": "abc"})
    with pytest.raises(ConfigError, match="CLIP_DIM"):
        Config.from_env({"CLIP_DIM": "0"})
    with pytest.raises(ConfigError, match="CLIP_DIM"):
        Config.from_env({"CLIP_DIM": "-5"})


def test_clip_port_invalid_is_config_error():
    with pytest.raises(ConfigError, match="CLIP_PORT"):
        Config.from_env({"CLIP_PORT": ":notaport"})


def test_request_bound_overrides():
    cfg = Config.from_env({"CLIP_MAX_TEXTS": "10", "CLIP_MAX_IMAGES": "4"})
    assert cfg.max_texts == 10
    assert cfg.max_images == 4
