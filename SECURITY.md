# Security Policy

Timich Agent runs on user-controlled infrastructure and handles local admin
tokens, paired-device sessions, datasource credentials, and media access. Please
report security issues privately before sharing details in public.

## Supported Versions

| Track | Support |
| --- | --- |
| Latest GitHub release | Supported for security fixes. |
| `main` | Receives fixes before the next release when practical. |
| Older releases | Best effort only; users should upgrade to the latest release. |

## Reporting a Vulnerability

Use GitHub's private vulnerability reporting flow for this repository from the
Security tab's "Report a vulnerability" action.

Do not open a public issue, discussion, or pull request for a vulnerability
report. Do not include exploit details, secrets, tokens, logs, private network
information, or private media metadata in public project spaces.

Helpful reports include:

- affected version, commit, or release asset
- deployment mode, such as native binary, Docker, or Docker Compose
- impact and expected attacker position
- minimal reproduction steps
- sanitized logs or request examples, if relevant
- whether the issue is already public or under coordinated disclosure elsewhere

## Security Expectations

- The admin and media HTTP ports are intended for trusted local networks or
  host-local access only.
- Do not expose Timich Agent ports directly to the public internet.
- Treat admin tokens, datasource API keys, local state files, pairing codes,
  access tokens, and refresh tokens as secrets.
- Redact secrets and private media metadata from issues, pull requests, logs,
  screenshots, and test fixtures.

Security fixes may be released without full details until users have had a
reasonable opportunity to upgrade.
