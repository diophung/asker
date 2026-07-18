# Build context is services/reranker (this service has NO protobuf dependency):
#   docker build -f services/reranker/Dockerfile services/reranker
#
# The model is NOT baked into the image weights-first; it is prefetched into the
# HF cache at build time (below) and loads lazily on startup. Mount a volume at
# /home/nonroot/.cache so a deployed/CI container keeps the weights across
# restarts. /health reports 503 "loading" until ready (give compose a generous
# healthcheck start_period).

FROM python:3.12-slim

WORKDIR /app

# Which PyTorch wheel index to install torch from. Default = the CPU index. The
# two-host GPU deploy overrides this at build time to a CUDA index, e.g.:
#   --build-arg TORCH_INDEX_URL=https://download.pytorch.org/whl/cu126
# See docs/two-host-deploy.md. torch==2.12.0 is published on cpu and cu126.
ARG TORCH_INDEX_URL=https://download.pytorch.org/whl/cpu

COPY requirements.txt ./requirements.txt
RUN pip install --no-cache-dir --extra-index-url "${TORCH_INDEX_URL}" \
    -r requirements.txt

COPY reranker/ /app/reranker/

ENV PYTHONPATH=/app \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HF_HOME=/home/nonroot/.cache/huggingface \
    RERANKER_PORT=":9900"

RUN groupadd --gid 65532 nonroot && \
    useradd --uid 65532 --gid 65532 --create-home --shell /usr/sbin/nologin nonroot && \
    mkdir -p /home/nonroot/.cache && chown -R nonroot:nonroot /home/nonroot/.cache
USER nonroot

# Prefetch the bge-reranker-v2-m3 weights into the image so a fresh deploy/CI
# container is ready in seconds instead of downloading on first use. Matches the
# default RERANKER_MODEL. Retry with backoff in case the CDN/HF rate-limits.
RUN for i in 1 2 3 4 5 6; do \
      python -c "from sentence_transformers import CrossEncoder; CrossEncoder('BAAI/bge-reranker-v2-m3')" && break; \
      if [ "$i" = 6 ]; then echo "reranker prefetch failed after 6 attempts" >&2; exit 1; fi; \
      echo "reranker prefetch attempt $i failed; retrying in $((i*20))s"; sleep $((i*20)); \
    done

EXPOSE 9900

# Compose healthcheck: ["CMD", "python", "-m", "reranker.main", "-healthcheck"]
ENTRYPOINT ["python", "-m", "reranker.main"]
