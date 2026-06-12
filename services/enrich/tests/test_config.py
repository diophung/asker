"""Config parsing: EMBEDDING_DIM is required, defaults match the topology."""

import pytest

from enrich.config import Config, ConfigError, health_addr_from_env


def test_defaults_with_required_dim():
    config = Config.from_env({"EMBEDDING_DIM": "384"})
    assert config.kafka_brokers == ["redpanda:9092"]
    assert config.tei_url == "http://tei:80"
    assert config.embedding_dim == 384
    assert config.health_port == 9601


def test_embedding_dim_missing_fails_fast():
    with pytest.raises(ConfigError, match="EMBEDDING_DIM"):
        Config.from_env({})


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
        Config.from_env({"EMBEDDING_DIM": "384", "KAFKA_BROKERS": " , "})


def test_health_addr_default_and_port_only():
    assert health_addr_from_env({}) == ("0.0.0.0", 9601)  # noqa: S104
    assert health_addr_from_env({"ENRICH_HEALTH_ADDR": ":9700"}) == ("0.0.0.0", 9700)  # noqa: S104


def test_health_addr_invalid():
    with pytest.raises(ConfigError):
        health_addr_from_env({"ENRICH_HEALTH_ADDR": "9601"})
    with pytest.raises(ConfigError):
        health_addr_from_env({"ENRICH_HEALTH_ADDR": ":not-a-port"})
