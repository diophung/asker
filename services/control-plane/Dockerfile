# Build context is the REPO ROOT:
#   docker build -f services/control-plane/Dockerfile .

FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY platform/ platform/
COPY services/control-plane/ services/control-plane/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control-plane ./services/control-plane
# Empty dir whose nonroot ownership seeds the /keys named volume on first
# mount (Docker copies ownership from the image for empty volumes).
RUN mkdir -p /out/keys

# Distroless: no shell, no curl. The compose healthcheck runs the binary
# itself: ["CMD", "/control-plane", "-healthcheck"].
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/control-plane /control-plane
COPY --from=builder --chown=nonroot:nonroot /out/keys /keys
EXPOSE 9100 9101
ENTRYPOINT ["/control-plane"]
