# Build context is the REPO ROOT:
#   docker build -f services/ingest/Dockerfile .

FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY platform/ platform/
COPY services/ingest/ services/ingest/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ingest ./services/ingest

# Distroless: no shell, no curl. The compose healthcheck runs the binary
# itself: ["CMD", "/ingest", "-healthcheck"].
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/ingest /ingest
EXPOSE 9501
ENTRYPOINT ["/ingest"]
