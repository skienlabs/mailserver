# syntax=docker/dockerfile:1
# ---- build: a single static binary with the web app embedded
# Built on the machine's own platform and cross-compiled (pure Go), so multi-platform images
# (linux/amd64, linux/arm64) build quickly.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/skienmail ./cmd/skienmail

# ---- runtime: ~25 MB image, runs as an unprivileged user
FROM alpine:3.22
LABEL org.opencontainers.image.title="Skien Mails" \
      org.opencontainers.image.description="Self-hosted mail server, webmail and admin console in one container" \
      org.opencontainers.image.source="https://github.com/skienlabs/mailserver" \
      org.opencontainers.image.vendor="Skien Software Labs" \
      org.opencontainers.image.licenses="MIT"
RUN apk add --no-cache ca-certificates tzdata libcap \
 && adduser -D -H -u 10001 -s /sbin/nologin skienmail \
 && mkdir -p /data && chown skienmail:skienmail /data
COPY --from=build /out/skienmail /usr/local/bin/skienmail
# allow binding ports 25/465/587 (and 80/443 in ACME mode) without root
RUN setcap cap_net_bind_service=+ep /usr/local/bin/skienmail && apk del libcap
USER skienmail
ENV SM_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 25 143 465 587 993 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/skienmail"]
