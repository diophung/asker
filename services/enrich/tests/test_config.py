"""Config parsing: EMBEDDING_DIM is required, defaults match the topology."""

import pytest

from enrich.config import Config, ConfigError, health_addr_from_env

# EMBEDDING_DIM and CLIP_DIM are both required (ADR-005/013); the media envs
# have container-network defaults like TEI_URL does.
_MIN_ENV = {"EMBEDDING_DIM": "384", "CLIP_DIM": "512"}


def test_defaults_with_required_dims():
    config = Config.from_env(dict(_MIN_ENV))
    assert config.kafka_brokers == ["redpanda:9092"]
    assert config.tei_url == "http://tei:80"
    assert config.embedding_dim == 384
    assert config.health_port == 9601
    # Media defaults (ADR-013).
    assert config.clip_dim == 512
    assert config.clip_url == "http://clip:9800"
    assert config.hub_media_url == "http://connector-hub:9300"
    assert config.whisper_model == "tiny"
    assert config.whisper_compute_type == "int8"
    assert config.max_keyframes == 20
    assert config.thumbnail_max_px == 512


def test_embedding_dim_missing_fails_fast():
    with pytest.raises(ConfigError, match="EMBEDDING_DIM"):
        Config.from_env({"CLIP_DIM": "512"})


def test_clip_dim_missing_fails_fast():
    with pytest.raises(ConfigError, match="CLIP_DIM"):
        Config.from_env({"EMBEDDING_DIM": "384"})


@pytest.mark.parametrize("raw", ["", "  ", "abc", "12.5", "0", "-1"])
def test_clip_dim_invalid_fails_fast(raw):
    with pytest.raises(ConfigError, match="CLIP_DIM"):
        Config.from_env({"EMBEDDING_DIM": "384", "CLIP_DIM": raw})


def test_media_urls_and_knob_overrides():
    config = Config.from_env(
        {
            **_MIN_ENV,
            "CLIP_URL": "http://clip.local:9800/",
            "HUB_MEDIA_URL": "http://hub.local:9300/",
            "WHISPER_MODEL": "base",
            "WHISPER_COMPUTE_TYPE": "float32",
            "MAX_KEYFRAMES": "5",
            "THUMBNAIL_MAX_PX": "256",
        }
    )
    assert config.clip_url == "http://clip.local:9800"  # trailing slash stripped
    assert config.hub_media_url == "http://hub.local:9300"
    assert config.whisper_model == "base"
    assert config.whisper_compute_type == "float32"
    assert config.max_keyframes == 5
    assert config.thumbnail_max_px == 256


@pytest.mark.parametrize("name", ["MAX_KEYFRAMES", "THUMBNAIL_MAX_PX"])
@pytest.mark.parametrize("raw", ["abc", "0", "-3"])
def test_optional_int_knobs_reject_bad_values(name, raw):
    with pytest.raises(ConfigError, match=name):
        Config.from_env({**_MIN_ENV, name: raw})


@pytest.mark.parametrize("raw", ["", "  ", "abc", "12.5", "1024px"])
def test_embedding_dim_non_int_fails_fast(raw):
    with pytest.raises(ConfigError, match="EMBEDDING_DIM"):
        Config.from_env({"EMBEDDING_DIM": raw})


@pytest.mark.parametrize("raw", ["0", "-1"])
def test_embedding_dim_non_positive_fails_fast(raw):
    with pytest.raises(ConfigError, match="EMBEDDING_DIM"):
        Config.from_env({"EMBEDDING_DIM": raw})


def test_env_overrides():
    config = Config.from_env(
        {
            "EMBEDDING_DIM": "1024",
            "CLIP_DIM": "512",
            "KAFKA_BROKERS": "a:9092, b:9092",
            "TEI_URL": "http://tei.local:8080/",
            "ENRICH_HEALTH_ADDR": "127.0.0.1:9999",
        }
    )
    assert config.kafka_brokers == ["a:9092", "b:9092"]
    assert config.tei_url == "http://tei.local:8080"  # trailing slash stripped
    assert config.embedding_dim == 1024
    assert config.health_host == "127.0.0.1"
    assert config.health_port == 9999


def test_empty_brokers_rejected():
    with pytest.raises(ConfigError, match="KAFKA_BROKERS"):
        Config.from_env({**_MIN_ENV, "KAFKA_BROKERS": " , "})


def test_health_addr_default_and_port_only():
    assert health_addr_from_env({}) == ("0.0.0.0", 9601)  # noqa: S104
    assert health_addr_from_env({"ENRICH_HEALTH_ADDR": ":9700"}) == ("0.0.0.0", 9700)  # noqa: S104


def test_health_addr_invalid():
    with pytest.raises(ConfigError):
        health_addr_from_env({"ENRICH_HEALTH_ADDR": "9601"})
    with pytest.raises(ConfigError):
        health_addr_from_env({"ENRICH_HEALTH_ADDR": ":not-a-port"})
