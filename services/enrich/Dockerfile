# Build context is the REPO ROOT (the committed protobuf gencode is copied
# from platform/proto/gen/python):
#   docker build -f services/enrich/Dockerfile .

FROM python:3.12-slim

WORKDIR /app

# Media pipeline system binaries (ADR-013, M3): tesseract drives image OCR
# (pytesseract shells out to it); ffmpeg + ffprobe extract a video's audio
# track, scene keyframes, and metadata. CLIP/torch are NOT here — CLIP lives in
# the separate `clip` service, called over HTTP. faster-whisper is a Python dep
# (CTranslate2, no torch) installed below.
RUN apt-get update && \
    apt-get install --no-install-recommends -y \
        tesseract-ocr \
        ffmpeg && \
    rm -rf /var/lib/apt/lists/*

COPY services/enrich/requirements.txt ./requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

# Committed protobuf gencode (asker.v1.Document et al.) + the worker package.
COPY platform/proto/gen/python/ /app/proto/
COPY services/enrich/enrich/ /app/enrich/

ENV PYTHONPATH=/app:/app/proto \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    # faster-whisper downloads the model (default whisper-tiny, ~75 MB) on FIRST
    # use into this Hugging Face cache dir, NOT at build time. The integrator
    # SHOULD mount a persistent volume here (e.g. compose `whisper-cache:`) so the
    # weights survive restarts/CI and the first media doc isn't blocked re-
    # downloading. The dir is owned by nonroot below so the lazy download works.
    HF_HOME=/home/nonroot/.cache/huggingface \
    WHISPER_MODEL=tiny

# Non-root (uid matches distroless nonroot for consistency across services).
RUN groupadd --gid 65532 nonroot && \
    useradd --uid 65532 --gid 65532 --create-home --shell /usr/sbin/nologin nonroot && \
    mkdir -p /home/nonroot/.cache/huggingface && \
    chown -R 65532:65532 /home/nonroot/.cache
USER nonroot

EXPOSE 9601

# Compose healthcheck: ["CMD", "python", "-m", "enrich.main", "-healthcheck"]
ENTRYPOINT ["python", "-m", "enrich.main"]
