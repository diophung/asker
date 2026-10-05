"""Start a host-native resident model. Starting may download uncached weights."""
from __future__ import annotations

import logging
import signal
import threading

from .config import Config
from .encoder import Encoder
from .server import BoundedServer

log = logging.getLogger(__name__)


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    config = Config.from_env()
    encoder = Encoder(config)
    server = BoundedServer(config, encoder)

    def warmup() -> None:
        try:
            encoder.load()
            log.info("native encoder ready model=%s device=%s dimension=%d fingerprint=%s",
                         config.model, encoder.device, config.dimension, encoder.identity()["fingerprint"])
        except Exception:  # noqa: BLE001 - failure must keep readiness false without model bodies
            # No provider body or user evidence is logged; readiness stays false.
            log.error("native encoder failed to load; check model, dimension and accelerator")

    def stop(_signal: int, _frame: object) -> None:
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    threading.Thread(target=warmup, name="native-encoder-warmup", daemon=True).start()
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
