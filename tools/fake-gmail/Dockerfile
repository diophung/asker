# fake-gmail: in-memory Gmail API fake for dev/CI (ADR-008). NEVER deploy
# this image to production — its /admin API is intentionally unauthenticated.
#
# Build context is the REPO ROOT:
#   docker build -f tools/fake-gmail/Dockerfile .

FROM golang:1.25 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY platform/ platform/
COPY tools/fake-gmail/ tools/fake-gmail/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-gmail ./tools/fake-gmail

# Distroless: no shell, no curl. The compose healthcheck runs the binary
# itself: ["CMD", "/fake-gmail", "-healthcheck"].
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/fake-gmail /fake-gmail
EXPOSE 9400
ENTRYPOINT ["/fake-gmail"]
