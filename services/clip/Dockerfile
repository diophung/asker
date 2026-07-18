# Build context is services/clip (this service has NO protobuf dependency):
#   docker build -f services/clip/Dockerfile services/clip
#
# The model is NOT baked into the image. It loads lazily on startup into a
# cache directory; mount a volume at /home/nonroot/.cache so a deployed/CI
# container keeps the weights across restarts and avoids re-downloading. First
# load (cold cache) downloads ViT-B-32/openai (~340 MB) and takes seconds-to-
# minutes depending on bandwidth/CPU; /health reports 503 "loading" until ready
# (give compose a generous healthcheck start_period).

FROM python:3.12-slim

WORKDIR /app

# Which PyTorch wheel index to install torch from. Default = the CPU index
# (the default PyPI build pulls CUDA on linux/amd64, which dev/CI/Apple-Silicon
# don't want). The two-host GPU deploy overrides this at build time to a CUDA
# index, e.g.:
#   --build-arg TORCH_INDEX_URL=https://download.pytorch.org/whl/cu126
# The CUDA wheels bundle the CUDA runtime libraries, so no CUDA base image is
# needed — only the host's NVIDIA driver + nvidia-container-toolkit at runtime
# (the compose `clip` service requests a gpu device). See docs/two-host-
# deploy.md. The torch VERSION pinned in requirements.txt must exist on the
# chosen index: torch==2.13.0 is published on cpu and cu126 (NOT cu124).
ARG TORCH_INDEX_URL=https://download.pytorch.org/whl/cpu

# Install torch from the selected index first, then the rest from PyPI.
COPY requirements.txt ./requirements.txt
RUN pip install --no-cache-dir --extra-index-url "${TORCH_INDEX_URL}" \
    -r requirements.txt

COPY clip/ /app/clip/

ENV PYTHONPATH=/app \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    # open_clip / huggingface_hub cache; mount a volume here to persist weights.
    HF_HOME=/home/nonroot/.cache/huggingface \
    CLIP_PORT=":9800"

# Non-root (uid matches distroless nonroot for consistency across services).
RUN groupadd --gid 65532 nonroot && \
    useradd --uid 65532 --gid 65532 --create-home --shell /usr/sbin/nologin nonroot && \
    mkdir -p /home/nonroot/.cache && chown -R nonroot:nonroot /home/nonroot/.cache
USER nonroot

# Bake the CLIP ViT-B-32/openai weights into the image at build time so a fresh
# deploy/CI container is ready in seconds instead of downloading on first use
# (the empty named cache volume mounted at /home/nonroot/.cache is populated from
# this image layer on first start). Matches the default CLIP_MODEL/CLIP_PRETRAINED.
# Retry with backoff in case the CDN/HF rate-limits parallel CI builds.
RUN for i in 1 2 3 4 5 6; do \
      python -c "import open_clip; open_clip.create_model_and_transforms('ViT-B-32', pretrained='openai')" && break; \
      if [ "$i" = 6 ]; then echo "clip prefetch failed after 6 attempts" >&2; exit 1; fi; \
      echo "clip prefetch attempt $i failed; retrying in $((i*20))s"; sleep $((i*20)); \
    done

EXPOSE 9800

# Compose healthcheck: ["CMD", "python", "-m", "clip.main", "-healthcheck"]
ENTRYPOINT ["python", "-m", "clip.main"]
