# Contributing to timich-agent

Thanks for helping improve Timich Agent. This repository is the public
distribution repository for the local agent runtime.

## Project Shape

Timich Agent source snapshots are exported from the Timich codebase. Keep
changes compatible with that export flow. Broad product documentation and
README changes should normally be made in the Timich source of truth and then
synced here, unless a change is specific to this standalone public repository.

Good standalone-repository changes include:

- OSS governance files
- release and distribution metadata
- issue, pull request, and security process documentation
- fixes that are also ready to be carried back into the Timich source tree

## Local Setup

Requirements:

- Go 1.26 or newer
- Docker, if you are testing container workflows
- Docker Compose, if you are testing the compose service

Common checks:

```bash
make test
make build
cd packages/contracts
go test ./...
```

For local runtime testing, start with:

```bash
make init
make run
```

Do not commit `.local`, release bundles, build output, credentials, generated
state, or private deployment files.

## Pull Requests

- Keep pull requests focused and explain the user-facing behavior change.
- Add or update tests for behavior changes and bug fixes.
- Update relevant docs or specs when interfaces, setup, or security
  expectations change.
- Prefer existing project patterns over new abstractions.
- Use the project logging path for diagnostics, and never log secrets or
  sensitive user data.
- Call out whether the change needs to be reflected in the Timich source tree.

## Security-Sensitive Changes

Changes touching authentication, pairing, token handling, datasource secrets,
remote browsing, media authorization, or local network exposure should include
extra verification notes in the pull request. Avoid putting real tokens,
private hostnames, IP addresses, or media metadata in tests and examples.

## License

By contributing to this repository, you agree that your contribution is
licensed under the MIT License unless you explicitly state otherwise in writing
before the contribution is accepted.
