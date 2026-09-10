# Build context is the REPO ROOT:
#   docker build -f services/query/Dockerfile .

FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY platform/ platform/
COPY services/query/ services/query/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/query ./services/query

# Distroless: no shell, no curl. The compose healthcheck runs the binary
# itself: ["CMD", "/query", "-healthcheck"].
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/query /query
EXPOSE 9200 9201
ENTRYPOINT ["/query"]
