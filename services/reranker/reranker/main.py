"""Entrypoint: wiring, JSON logging, lazy model warmup, graceful shutdown.

The HTTP listener comes up immediately; the (heavy) model loads in a background
thread so /health reports 503 "loading" until the weights are ready and 200
afterward — matching how compose waits on a generous health start_period.
Mirrors services/clip/clip/main.py.
"""

from __future__ import annotations

import argparse
import json
import logging
import signal
import sys
import threading
import time
import urllib.error
import urllib.request

from .config import Config, ConfigError
from .scorer import CrossEncoderScorer
from .server import make_server

log = logging.getLogger("reranker.main")

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


def run(config: Config) -> None:
    scorer = CrossEncoderScorer(
        config.model,
        device=config.device,
        precision=config.precision,
        max_length=config.max_length,
        batch_size=config.batch_size,
    )

    def warmup() -> None:
        try:
            scorer.load()
        except Exception:  # the listener stays up reporting 503; restart loads again
            log.exception("reranker model load failed")

    threading.Thread(target=warmup, name="reranker-warmup", daemon=True).start()

    server = make_server(config, scorer, ready=lambda: scorer.loaded)
    log.info(
        "reranker service listening",
        extra={
            "host": config.host,
            "port": config.port,
            "model": config.model,
            "device": config.device,
            "precision": config.precision,
        },
    )

    def shutdown(_signum: int, _frame: object) -> None:
        log.info("reranker service stopping")
        threading.Thread(target=server.shutdown, name="reranker-shutdown", daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    try:
        server.serve_forever()
    finally:
        server.server_close()


def healthcheck() -> int:
    """Self-probe /health for container healthchecks (-healthcheck flag)."""
    try:
        config = Config.from_env()
    except ConfigError:
        return 1
    port = config.port
    try:
        with urllib.request.urlopen(  # fixed loopback URL, not user input
            f"http://127.0.0.1:{port}/health", timeout=3
        ) as resp:
            return 0 if resp.status == 200 else 1
    except (urllib.error.URLError, OSError, TimeoutError):
        return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="reranker", description="Asker cross-encoder reranker service"
    )
    parser.add_argument(
        "-healthcheck",
        "--healthcheck",
        action="store_true",
        help="probe the local /health endpoint and exit (for container healthchecks)",
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
        run(config)
    except KeyboardInterrupt:
        pass
    except Exception:  # log the crash, exit non-zero, let the platform restart us
        log.exception("reranker service crashed")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
