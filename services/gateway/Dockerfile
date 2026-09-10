# Build context is the REPO ROOT:
#   docker build -f services/gateway/Dockerfile .

FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY platform/ platform/
COPY services/gateway/ services/gateway/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./services/gateway

# Distroless: no shell, no curl. The compose healthcheck runs the binary
# itself: ["CMD", "/gateway", "-healthcheck"].
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/gateway /gateway
EXPOSE 8080
ENTRYPOINT ["/gateway"]
