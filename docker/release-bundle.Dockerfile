FROM debian:bookworm-slim

# Bootstrap HTTPS apt without opening port 80 in the disposable build network.
COPY --from=alpine:3.22 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

RUN sed -i 's|http://|https://|g' /etc/apt/sources.list.d/debian.sources && \
	apt-get update && \
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

# The private build context can use mode 0700/0600 under umask 077.
# Published image inputs must also be executable/readable by a non-root user.
RUN chmod 0755 /usr/local/bin/timich-agent /usr/local/bin/timich-semantic-helper \
	/usr/local/bin/timich-media-helper /usr/local/bin/timich-agent-entrypoint && \
	chmod -R a+rX /usr/local/bin/semantic-runtime && \
	mkdir -p /var/lib/timich-agent

EXPOSE 8081 8082

ENTRYPOINT ["/usr/local/bin/timich-agent-entrypoint"]
CMD []
