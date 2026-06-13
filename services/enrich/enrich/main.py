"""Entrypoint: wiring, JSON logging, health endpoints, graceful shutdown."""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import json
import logging
import signal
import sys
import time
import urllib.error
import urllib.request
from collections.abc import Awaitable, Callable

from . import config as cfg
from .config import Config, ConfigError, health_addr_from_env
from .embedder import Embedder
from .kafka_worker import Worker, ensure_topics

log = logging.getLogger("enrich.main")

ReadyCheck = Callable[[], Awaitable[tuple[bool, str]]]

_LOG_RECORD_BUILTINS = frozenset(logging.LogRecord("", 0, "", 0, "", (), None).__dict__) | {
    "message",
    "asctime",
    "taskName",
}


class _JSONFormatter(logging.Formatter):
    """Minimal structured-log formatter (slog parity for the Python service)."""

    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, object] = {
            "time": time.strftime("%Y-%m-%dT%H:%M:%S%z", time.localtime(record.created)),
            "level": record.levelname,
            "logger": record.name,
            "msg": record.getMessage(),
        }
        for key, value in record.__dict__.items():
            if key not in _LOG_RECORD_BUILTINS:
                payload[key] = value
        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)
        return json.dumps(payload, default=str)


def setup_logging() -> None:
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(_JSONFormatter())
    logging.basicConfig(level=logging.INFO, handlers=[handler])


async def _handle_health(
    reader: asyncio.StreamReader,
    writer: asyncio.StreamWriter,
    ready_check: ReadyCheck,
) -> None:
    """Serve one HTTP request on the health listener (no extra deps)."""
    status, body = 404, b"not found\n"
    try:
        request_line = await asyncio.wait_for(reader.readline(), timeout=5)
        while True:  # drain request headers
            line = await asyncio.wait_for(reader.readline(), timeout=5)
            if line in (b"\r\n", b"\n", b""):
                break
        parts = request_line.decode("latin-1").split()
        path = parts[1].split("?", 1)[0] if len(parts) >= 2 else ""
        if path == "/healthz":
            status, body = 200, b"ok\n"
        elif path == "/readyz":
            ok, msg = await ready_check()
            status, body = (200, b"ok\n") if ok else (503, f"{msg}\n".encode())
    except (TimeoutError, ConnectionError, UnicodeDecodeError):
        status, body = 400, b"bad request\n"
    except Exception:  # the health listener must never crash the worker
        log.exception("health request failed")
        status, body = 500, b"internal error\n"
    try:
        reason = {200: "OK", 400: "Bad Request", 404: "Not Found", 503: "Service Unavailable"}
        writer.write(
            f"HTTP/1.1 {status} {reason.get(status, 'Error')}\r\n"
            f"Content-Type: text/plain; charset=utf-8\r\n"
            f"Content-Length: {len(body)}\r\n"
            f"Connection: close\r\n\r\n".encode("latin-1")
            + body
        )
        await writer.drain()
    except ConnectionError:
        pass
    finally:
        writer.close()
        with contextlib.suppress(ConnectionError):
            await writer.wait_closed()


async def serve_health(host: str, port: int, ready_check: ReadyCheck) -> asyncio.Server:
    server = await asyncio.start_server(
        lambda r, w: _handle_health(r, w, ready_check), host=host, port=port
    )
    log.info("health server listening", extra={"host": host, "port": port})
    return server


async def run(config: Config) -> None:
    # Imported here so unit tests of the worker/embedder never need the
    # aiokafka client machinery spun up.
    from aiokafka import AIOKafkaConsumer, AIOKafkaProducer

    embedder = Embedder(config.tei_url, config.embedding_dim)
    consumer_started = False

    async def ready_check() -> tuple[bool, str]:
        if not consumer_started:
            return False, "kafka consumer not started"
        return await embedder.healthy()

    health_server = await serve_health(config.health_host, config.health_port, ready_check)

    consumer = AIOKafkaConsumer(
        cfg.TOPIC_DOCS_CHUNKED,
        bootstrap_servers=config.kafka_brokers,
        group_id=cfg.CONSUMER_GROUP,
        enable_auto_commit=False,  # commit-after-success is the whole contract
        auto_offset_reset="earliest",
        max_poll_records=config.max_poll_records,
    )
    # acks="all" + idempotence matches franz-go's durable defaults on the Go
    # producers (all-ISR ack, no silent loss on leader failover), so the
    # docs.enriched stage has the same durability as docs.raw/docs.chunked
    # (ADR-004 parity).
    producer = AIOKafkaProducer(
        bootstrap_servers=config.kafka_brokers,
        acks="all",
        enable_idempotence=True,
    )
    worker = Worker(consumer, producer, embedder, max_poll_records=config.max_poll_records)

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, worker.request_stop)

    try:
        await ensure_topics(config.kafka_brokers)
        await producer.start()
        await consumer.start()
        consumer_started = True
        log.info(
            "enrich worker started",
            extra={
                "brokers": ",".join(config.kafka_brokers),
                "tei_url": config.tei_url,
                "embedding_dim": config.embedding_dim,
                "group": cfg.CONSUMER_GROUP,
                "topic": cfg.TOPIC_DOCS_CHUNKED,
            },
        )
        await worker.run()
        log.info("enrich worker stopping")
    finally:
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.remove_signal_handler(sig)
        # Graceful shutdown: per-record offsets were committed as we went;
        # stop the consumer (leaves the group), flush+stop the producer.
        with contextlib.suppress(Exception):
            await consumer.stop()
        with contextlib.suppress(Exception):
            await producer.stop()
        await embedder.aclose()
        health_server.close()
        await health_server.wait_closed()


def healthcheck() -> int:
    """Self-probe /healthz for container healthchecks (-healthcheck flag)."""
    _, port = health_addr_from_env()
    try:
        with urllib.request.urlopen(  # fixed loopback URL, not user input
            f"http://127.0.0.1:{port}/healthz", timeout=3
        ) as resp:
            return 0 if resp.status == 200 else 1
    except (urllib.error.URLError, OSError, TimeoutError):
        return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="enrich", description="Asker enrich worker (ADR-007)")
    parser.add_argument(
        "-healthcheck",
        "--healthcheck",
        action="store_true",
        help="probe the local health endpoint and exit (for container healthchecks)",
    )
    args = parser.parse_args(argv)
    if args.healthcheck:
        return healthcheck()

    setup_logging()
    try:
        config = Config.from_env()
    except ConfigError as exc:
        log.critical("invalid configuration", extra={"error": str(exc)})
        return 2

    try:
        asyncio.run(run(config))
    except KeyboardInterrupt:
        pass
    except Exception:  # log the crash, exit non-zero, let the platform restart us
        log.exception("enrich worker crashed")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
