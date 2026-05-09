# timich-agent

`timich-agent` is the local Timich runtime for datasource configuration,
pairing, direct LAN media delivery, and Timich Reach relay responses.

The current implementation focuses on a local operator loop that actually runs:

- persistent local config
- persistent agent identity and session-signing secret
- LAN-facing authenticated admin HTTP API and web UI
- LAN-facing pairing, session, catalog, and media HTTP APIs
- first-datasource Immich catalog and media proxying
- file-backed demo catalog/media serving for repeatable local testing
- remote browsing checks and outbound relay connection handling
- graceful shutdown on `SIGINT` / `SIGTERM`

## Quick Start

From this repository root:

```bash
make init
make run
make docker-build
make docker-run
make compose-up
```

The defaults use repository-local development paths:

- config: `.local/agent.json`
- state: `.local/state/agent-state.json`
- admin API: `0.0.0.0:8081`
- media API: `0.0.0.0:8082`

Both ports are intended only for trusted local networks, such as a home LAN you
control, or for host-local access. They are not encrypted by default, so do not
use them on guest Wi-Fi, shared office networks, untrusted LANs, or the public
internet. Timich Reach is the remote-access path.

## Docker Quick Start

For foreground testing:

```bash
make docker-run
```

That command builds on the same repo-local `.local` directory the native flow
uses. If `.local/agent.json` does not exist yet, the container
entrypoint creates it automatically and then starts the agent.

For NAS-style daemon runs:

```bash
make compose-up
docker compose -f compose.yaml logs -f
```

The compose service uses `restart: unless-stopped`, mounts
`.local` into the container, and publishes the authenticated admin API on host
port `8081` plus the media API on host port `8082`.

Compose runs use `Timich Agent` as the default admin UI display name instead of
the container hostname. Set `TIMICH_AGENT_NAME` before starting compose to
choose a more specific name such as `Home Timich Agent`.

The default paired-device limit is 32. Set `TIMICH_AGENT_DEVICE_LIMIT` if you
want a lower or higher cap.

Remote Browsing is enabled by default for compose runs. The agent registers its
relay public key automatically after admin setup, datasource setup, and at least
one paired app device are complete. To keep the agent local-only, opt out before
starting compose:

```bash
export TIMICH_AGENT_NAME="Home Timich Agent"
export TIMICH_AGENT_DEVICE_LIMIT=32
export TIMICH_AGENT_REMOTE_BROWSING_ENABLED=false
make compose-up
```

On first run, open the Admin UI and create the admin token before leaving the
agent running unattended:

```bash
# Open in a browser:
# http://127.0.0.1:8081/
```

If no admin token is configured yet, the startup logs also show the admin UI
URL and the local state file path where the token will be stored after setup.

After setup, you can inspect the admin API from the host by exporting the saved
admin token:

```bash
export TIMICH_AGENT_ADMIN_TOKEN="$(jq -r .adminToken .local/state/agent-state.json)"
curl -s http://127.0.0.1:8081/status \
  -H "Authorization: Bearer $TIMICH_AGENT_ADMIN_TOKEN"
```

## Useful Endpoints

Admin API:

- `GET http://AGENT_LAN_HOST:8081/`
- `GET http://AGENT_LAN_HOST:8081/healthz`
- `GET http://AGENT_LAN_HOST:8081/readyz`
- `GET http://AGENT_LAN_HOST:8081/version`
- `GET http://AGENT_LAN_HOST:8081/status`
- `GET http://AGENT_LAN_HOST:8081/config`
- `POST http://AGENT_LAN_HOST:8081/setup-admin-token`
- `GET http://AGENT_LAN_HOST:8081/v1/datasource/primary`
- `PUT http://AGENT_LAN_HOST:8081/v1/datasource/primary`
- `POST http://AGENT_LAN_HOST:8081/v1/pairing-sessions`
- `POST http://AGENT_LAN_HOST:8081/v1/compatibility-check`
- `GET http://AGENT_LAN_HOST:8081/v1/update-check`
- `POST http://AGENT_LAN_HOST:8081/v1/restart`
- `GET http://AGENT_LAN_HOST:8081/v1/devices`
- `DELETE http://AGENT_LAN_HOST:8081/v1/devices/{deviceID}`

Use `127.0.0.1` when running commands on the agent host itself. Use the
agent's LAN hostname or IP address when connecting from another trusted LAN
device.

Media API:

- `GET http://AGENT_LAN_HOST:8082/healthz`
- `GET http://AGENT_LAN_HOST:8082/version`
- `GET http://AGENT_LAN_HOST:8082/v1/info`
- `POST http://AGENT_LAN_HOST:8082/v1/pairing/redeem`
- `POST http://AGENT_LAN_HOST:8082/v1/session/refresh`
- `GET http://AGENT_LAN_HOST:8082/v1/catalog`
- `GET http://AGENT_LAN_HOST:8082/v1/assets/{assetID}/preview`
- `GET http://AGENT_LAN_HOST:8082/v1/assets/{assetID}/detail_preview`
- `GET http://AGENT_LAN_HOST:8082/v1/assets/{assetID}/original`

When the agent runs in Docker, both host ports are published. Keep those ports
on a trusted LAN or host-local network only, and do not add router port-forward
rules for them.

## Admin Authentication and Web Management

The admin API requires an admin bearer token for every route except `/healthz`,
`/readyz`, `/version`, and the first-run admin-token setup route. The same admin
surface also serves a small web management UI at `http://AGENT_LAN_HOST:8081/`.

On first run, the agent creates its local identity and signing key but leaves the
admin token empty. Visit the admin UI from the trusted LAN or agent host and
create an admin token with at least 16 characters before leaving the agent
running unattended. The token is then stored in the local state file next to the
agent identity and signing key. For the default development layout:

```bash
export TIMICH_AGENT_ADMIN_TOKEN="$(jq -r .adminToken .local/state/agent-state.json)"
```

Use that token with curl:

```bash
curl -s http://127.0.0.1:8081/status \
  -H "Authorization: Bearer $TIMICH_AGENT_ADMIN_TOKEN"
```

The web UI uses the same token for browser login and currently covers:

- agent status and remote browsing readiness
- first-run admin-token setup
- agent update checks
- primary Immich datasource editing
- pairing-code creation
- paired-device listing and revoke
- remote browsing checks
- agent restart

Datasource editing is intentionally limited to the first datasource because the
current media proxy uses only the first configured datasource. Static demo
datasources are configured through the JSON config file instead of the current
web form.

The restart action gracefully stops the running process. It is useful when the
agent is supervised by Docker Compose, a NAS service, launchd, or systemd. When
running in the foreground, the process exits and must be started again manually.

## Updating the Agent

The Admin UI uses the authenticated `/v1/update-check` endpoint to show whether
a newer agent release is available. Release bundles publish an
`agent-update-manifest.json` asset next to the downloadable archives and
checksums.

The current update UX intentionally guides the operator through a safe update
instead of replacing the running process in place. For Docker Compose installs:

```bash
# Keep .local. It contains settings, the admin token, and paired devices.
docker compose -f compose.yaml down

# Extract the new Timich Agent bundle in place, preserving .local.
docker compose -f compose.yaml up -d --build
```

After the service is back online, open the Admin UI and confirm the displayed
version. Set `TIMICH_AGENT_UPDATE_MANIFEST_URL` before starting the agent to use
a custom manifest location for local testing or custom release channels.

## Local Pairing Smoke Test

Create a pairing code from the authenticated admin API:

```bash
curl -s -X POST http://127.0.0.1:8081/v1/pairing-sessions \
  -H "Authorization: Bearer $TIMICH_AGENT_ADMIN_TOKEN"
```

Redeem that code from the LAN-facing media API:

```bash
curl -s -X POST http://127.0.0.1:8082/v1/pairing/redeem \
  -H 'Content-Type: application/json' \
  -d '{"pairingCode":"PAIRING_CODE","deviceName":"Test iPhone"}'
```

Use the returned `accessToken` against the first catalog page:

```bash
curl -s http://127.0.0.1:8082/v1/catalog \
  -H 'Authorization: Bearer ACCESS_TOKEN'
```

Refresh the session with the returned `refreshToken`:

```bash
curl -s -X POST http://127.0.0.1:8082/v1/session/refresh \
  -H 'Content-Type: application/json' \
  -d '{"refreshToken":"REFRESH_TOKEN"}'
```

## Remote Browsing Check

Timich Reach provides remote browsing through a relay server when the app is
away from the trusted LAN. The local admin API can run a lightweight remote
browsing check:

```bash
curl -s -X POST http://127.0.0.1:8081/v1/compatibility-check \
  -H "Authorization: Bearer $TIMICH_AGENT_ADMIN_TOKEN" | jq
```

The current check covers:

- remote browsing and relay connection config presence
- relay signing-key state and registration status
- datasource metadata reachability
- relay server `/version` reachability
- relay connection hello/ack round trip

The intended operator entry point is the Admin UI.

## Pairing Security Notes

- Pairing codes are one-time 128-bit random values encoded as 32 hex characters.
- The agent keeps only one active pairing session at a time; creating a new code
  replaces any earlier unredeemed code.
- After five failed redemption attempts against the active pairing session, the
  agent invalidates that session and requires the operator to create a fresh
  pairing code.
- Access tokens are short-lived, device-scoped, and audience-scoped for the
  local `timich-agent` media API.
- Refresh tokens are stored at rest as salted hashes in the paired-device
  registry instead of being persisted verbatim.
- Immich datasource API keys are stored in the local agent config file with
  owner-only file permissions. This is an accepted risk for the current
  Docker/package milestone because the agent does not yet have a separate local
  secret store. The admin UI and API only show whether a key is configured, not
  the key value.
- The session-signing key and relay private key remain plaintext in the local
  state file because the single-process agent must read them directly to mint
  app-session tokens and sign relay connection tokens, and does not yet have a
  separate local secret store.
- The relay private key is generated automatically by the agent. The agent
  registers only the matching public key with the Timich Reach server; users do
  not need to install client certificates or server-issued bootstrap secrets.
  Before first registration, unauthenticated LAN info does not expose the agent
  ID; setup and pairing still happen on a trusted LAN. The relay server
  rate-limits new first-write registrations by client address, and the agent
  retries registration if the server asks it to slow down.
- Config, identity state, and device-registry files are replaced with atomic
  temp-file writes so an interrupted write does not leave a truncated JSON file
  behind.

## Local Config Shape

`make init` writes a starter JSON config like this:

```json
{
  "agentName": "your-hostname",
  "adminListenAddress": "0.0.0.0:8081",
  "mediaListenAddress": "0.0.0.0:8082",
  "dataDir": "state",
  "deviceLimit": 32,
  "relayConnectionAddress": "https://timich.runo.jp",
  "remoteBrowsing": {
    "enabled": false,
    "serverURL": "https://timich.runo.jp"
  },
  "datasources": []
}
```

Set `adminListenAddress` to `127.0.0.1:8081` if you want to opt out of LAN
admin access and use only the local browser, authenticated admin API, or SSH
tunnel workflows.

The first datasource-backed local flow proxies the first configured Immich
datasource for catalog pages plus `preview`, `detail_preview`, and `original`
media delivery. A `static_demo` datasource can also serve a generated sample
bundle from local disk for repeatable local testing.

Example static demo config:

```json
{
  "agentName": "Timich Demo Agent",
  "adminListenAddress": "127.0.0.1:8081",
  "mediaListenAddress": "127.0.0.1:8082",
  "dataDir": "state",
  "remoteBrowsing": {
    "enabled": true,
    "serverURL": "https://timich.runo.jp"
  },
  "datasources": [
    {
      "id": "static-demo",
      "kind": "static_demo",
      "url": "/opt/timich/static-demo-bundle",
      "name": "Static Demo",
      "enabled": true
    }
  ]
}
```

The static demo bundle is generated from original photos/videos and contains a
`manifest.json` plus per-asset preview, detail-preview, and original files. From
this source directory, generate a 120-item bundle with:

```bash
go run ./cmd/timich-static-demo-bundle \
  --input /path/to/original-samples \
  --output /path/to/static-demo-bundle \
  --count 120
```

The generator places image assets before video assets so a first-page
Remote Browsing activation can show a full page of photos while the second page
still includes video fixtures. Generated image originals are capped to a
reasonable test size and are re-encoded as JPEG; video originals are
copied through with generated poster previews.
