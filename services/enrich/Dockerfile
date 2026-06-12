# Build context is the REPO ROOT (the committed protobuf gencode is copied
# from platform/proto/gen/python):
#   docker build -f services/enrich/Dockerfile .

FROM python:3.12-slim

WORKDIR /app

COPY services/enrich/requirements.txt ./requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

# Committed protobuf gencode (asker.v1.Document et al.) + the worker package.
COPY platform/proto/gen/python/ /app/proto/
COPY services/enrich/enrich/ /app/enrich/

ENV PYTHONPATH=/app:/app/proto \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

# Non-root (uid matches distroless nonroot for consistency across services).
RUN groupadd --gid 65532 nonroot && \
    useradd --uid 65532 --gid 65532 --create-home --shell /usr/sbin/nologin nonroot
USER nonroot

EXPOSE 9601

# Compose healthcheck: ["CMD", "python", "-m", "enrich.main", "-healthcheck"]
ENTRYPOINT ["python", "-m", "enrich.main"]
