# Timich Agent

Timich Agent is the local runtime for connecting Timich to a self-hosted photo
library and enabling browsing from trusted devices.

Release bundles are published from this repository's GitHub Releases page. A
bundle includes the agent binary, the CLI helper, Docker Compose setup files,
and checksum metadata.

## Install

Download the release bundle for your server platform, extract it, then follow
the included `README.md` in the bundle.

For the Docker Compose setup, copy `.env.example` to `.env`, set the required
values, and start the agent:

```sh
cp .env.example .env
docker compose -f compose.yaml up -d --build
```

On first run, open the Admin UI URL shown in the logs from a trusted LAN and
create the admin token in the browser.

## Security

The Admin UI and local media API are intended for trusted local networks only.
Do not expose the local agent ports directly to the public internet.
