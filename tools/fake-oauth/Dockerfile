# fake-oauth: a standards-correct fake OAuth 2.0 authorization server for
# dev/CI. NEVER deploy this image to production — it auto-consents (no login)
# so any caller obtains a token for the configured subject.
#
# Build context is the REPO ROOT:
#   docker build -f tools/fake-oauth/Dockerfile .

FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY platform/ platform/
COPY tools/fake-oauth/ tools/fake-oauth/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-oauth ./tools/fake-oauth

# Distroless: no shell, no curl. The compose healthcheck runs the binary
# itself: ["CMD", "/fake-oauth", "-healthcheck"].
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/fake-oauth /fake-oauth
EXPOSE 9500
ENTRYPOINT ["/fake-oauth"]
