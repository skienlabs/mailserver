# syntax=docker/dockerfile:1
# ---- build: a single static binary with the web app embedded
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/skienmail ./cmd/skienmail

# ---- runtime: ~25 MB image, runs as an unprivileged user
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata libcap \
 && adduser -D -H -u 10001 -s /sbin/nologin skienmail \
 && mkdir -p /data && chown skienmail:skienmail /data
COPY --from=build /out/skienmail /usr/local/bin/skienmail
# allow binding ports 25/465/587 (and 80/443 in ACME mode) without root
RUN setcap cap_net_bind_service=+ep /usr/local/bin/skienmail && apk del libcap
USER skienmail
ENV SM_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 25 465 587 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/skienmail"]
