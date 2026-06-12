# Build context is the REPO ROOT:
#   docker build -f services/connector-hub/Dockerfile .

FROM golang:1.25 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY platform/ platform/
COPY connectors/ connectors/
COPY services/connector-hub/ services/connector-hub/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/connector-hub ./services/connector-hub

# Distroless: no shell, no curl. The compose healthcheck runs the binary
# itself: ["CMD", "/connector-hub", "-healthcheck"].
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/connector-hub /connector-hub
EXPOSE 9300 9301
ENTRYPOINT ["/connector-hub"]
