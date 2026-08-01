FROM debian:bookworm-slim

RUN apt-get update && \
	DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		ca-certificates \
		ffmpeg \
		libvips-tools \
		wget && \
	rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY timich-agent /usr/local/bin/timich-agent
COPY timich-semantic-helper /usr/local/bin/timich-semantic-helper
COPY timich-media-helper /usr/local/bin/timich-media-helper
COPY semantic-runtime /usr/local/bin/semantic-runtime
COPY docker/entrypoint.sh /usr/local/bin/timich-agent-entrypoint

RUN chmod +x /usr/local/bin/timich-agent-entrypoint && \
	mkdir -p /var/lib/timich-agent

EXPOSE 8081 8082

ENTRYPOINT ["/usr/local/bin/timich-agent-entrypoint"]
CMD []
