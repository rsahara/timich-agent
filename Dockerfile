## Keep the image portable across NAS Docker setups by avoiding BuildKit-only features.
FROM golang:1.26.0-alpine AS builder

WORKDIR /src

RUN apk add --no-cache cargo rust

COPY go.mod go.sum ./
COPY packages ./packages
COPY cmd ./cmd
COPY internal ./internal
COPY media-helper ./media-helper

RUN go build -o /out/timich-agent ./cmd/timich-agent && \
	go build -o /out/timich-semantic-helper ./cmd/timich-semantic-helper && \
	RUSTFLAGS="-C target-feature=+crt-static" CARGO_TARGET_DIR=/tmp/timich-media-helper-target cargo build --manifest-path media-helper/Cargo.toml --release && \
	cp /tmp/timich-media-helper-target/release/timich-media-helper /out/timich-media-helper

FROM debian:bookworm-slim

RUN apt-get update && \
	DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		ca-certificates \
		ffmpeg \
		libvips-tools \
		wget && \
	rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/timich-agent /usr/local/bin/timich-agent
COPY --from=builder /out/timich-semantic-helper /usr/local/bin/timich-semantic-helper
COPY --from=builder /out/timich-media-helper /usr/local/bin/timich-media-helper
COPY semantic-runtime /usr/local/bin/semantic-runtime
COPY docker/entrypoint.sh /usr/local/bin/timich-agent-entrypoint

RUN chmod +x /usr/local/bin/timich-agent-entrypoint && \
	mkdir -p /var/lib/timich-agent

EXPOSE 8081 8082

ENTRYPOINT ["/usr/local/bin/timich-agent-entrypoint"]
CMD []
