## Keep the image portable across NAS Docker setups by avoiding BuildKit-only features.
FROM golang:1.26.0-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
COPY packages ./packages
COPY cmd ./cmd
COPY internal ./internal

RUN go build -o /out/timich-agent ./cmd/timich-agent

FROM alpine:3.22

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /out/timich-agent /usr/local/bin/timich-agent
COPY docker/entrypoint.sh /usr/local/bin/timich-agent-entrypoint

RUN chmod +x /usr/local/bin/timich-agent-entrypoint && \
	mkdir -p /var/lib/timich-agent

EXPOSE 8081 8082

ENTRYPOINT ["/usr/local/bin/timich-agent-entrypoint"]
CMD []
