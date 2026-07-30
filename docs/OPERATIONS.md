# Operations

## Health checks

`GET /healthz` and `GET /readyz` return HTTP 200 after the HTTP process starts. A fatal Telegram authentication error terminates the process, allowing the supervisor to restart or stop the deployment.

Use `doctor` for a stronger check:

```sh
telegram-bot-mux doctor --config /etc/telegram-bot-mux/config.json
```

It validates configuration and secret files, opens and checks SQLite, and calls Telegram `getMe`. Add `--offline` to skip the network check.

## Backlog pressure

Every client has a `max_pending_per_client` limit. If ingesting a batch would cross that limit, the whole transaction rolls back and upstream polling pauses with bounded retries. No update from that batch is acknowledged to Telegram.

Restore the stalled client and let it advance its offset. Do not delete the database or raise the limit without first understanding why the client stopped consuming.

## Shutdown

`SIGINT` and `SIGTERM` cancel the active Telegram long poll, stop accepting HTTP requests, allow in-flight HTTP work up to ten seconds, and close SQLite. Run the binary under the deployment's existing supervisor. The project does not install a service.

## Backup

Create a backup while the service is running or stopped:

```sh
telegram-bot-mux backup \
  --config /etc/telegram-bot-mux/config.json \
  --out /var/backups/telegram-bot-mux/state-$(date +%s).db
```

The destination must not already exist. The command runs an integrity check first and writes a consistent standalone SQLite file.

## Restore

1. Stop the mux.
2. Preserve the current database and its WAL/SHM files for incident analysis.
3. Copy the selected backup to the configured database path with mode `0600`.
4. Run `doctor --offline`.
5. Start the mux and confirm Telegram polling resumes from the stored upstream offset.

## Client changes

Adding a client begins delivery with updates received after the next start. Removing a client deletes its outstanding delivery rows. It does not rewrite other client cursors.

Client IDs are durable routing identities. Rename a client only when intentionally creating a new queue.
