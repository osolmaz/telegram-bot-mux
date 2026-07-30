# AGENTS.md

## Required checks

Before finishing a change, run:

```sh
gofmt -w .
go vet ./...
go test -race ./...
./scripts/check-coverage.sh
golangci-lint run
govulncheck ./...
slophammer-go dry .
slophammer-go crap .
slophammer-go mutate . --scan
slophammer-go check . --execute
git diff --check
```

Do not run full mutation testing during ordinary implementation work.

## Go

- Keep interfaces next to their consumers and package APIs small.
- Validate unknown network, JSON, file, and database data at its boundary.
- Do not add reflection, unchecked type assertions, or generic dynamic maps in domain code.
- Keep the Telegram token out of logs, errors, SQLite, command arguments, and downstream responses.
- Preserve streaming for proxied uploads and downloads.
- Add focused tests for every durability, authentication, routing, offset, and lifecycle change.

## Architecture

- `internal/config` owns configuration and secret-file loading.
- `internal/store` is the only SQLite authority.
- `internal/routing` decides update recipients without performing IO.
- `internal/telegram` owns upstream Telegram transport.
- `internal/server` owns the downstream Bot API surface.
- The command package wires dependencies and process lifecycle only.

See the Slophammer agent entrypoint at https://github.com/osolmaz/slophammer/blob/main/docs/AGENT_ENTRYPOINT.md.
