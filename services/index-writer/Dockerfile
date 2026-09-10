# Build context is the REPO ROOT:
#   docker build -f services/index-writer/Dockerfile .

FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY platform/ platform/
COPY services/index-writer/ services/index-writer/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/index-writer ./services/index-writer

# Distroless: no shell, no curl. The compose healthcheck runs the binary
# itself: ["CMD", "/index-writer", "-healthcheck"].
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/index-writer /index-writer
EXPOSE 9701
ENTRYPOINT ["/index-writer"]
