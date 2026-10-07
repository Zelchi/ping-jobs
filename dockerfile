FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/pingbot ./cmd/pingbot

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 app \
    && adduser -S -D -H -u 10001 -G app app \
    && mkdir -p /data \
    && chown app:app /data

WORKDIR /app
COPY --from=builder --chown=app:app /out/pingbot /usr/local/bin/pingbot

USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/pingbot"]
