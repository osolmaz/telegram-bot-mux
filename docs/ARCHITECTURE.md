# Architecture

Telegram permits one active update reader for a bot token. Telegram Bot Mux owns that reader and exposes independent local Bot API views to downstream clients.

```text
Telegram Bot API
       │ one getUpdates stream
       ▼
Telegram Bot Mux
  ├── SQLite update log
  ├── OpenClaw delivery queue
  └── unYOLO delivery queue
       │
       ├── /client/openclaw/bot<local-token>/...
       └── /client/unyolo/bot<local-token>/...
```

## Ingest transaction

For every upstream batch, the mux performs one SQLite transaction:

1. Insert each new Telegram `update_id` and its exact JSON payload.
2. Compute the targeted clients and insert one delivery row per target.
3. Check every client backlog against its configured limit.
4. Store the next upstream offset.
5. Commit.

The next Telegram request starts only after this transaction commits. A crash before commit leaves the old upstream offset, so Telegram redelivers the batch. A crash after commit leaves both the update and next offset durable. The unique `update_id` constraint makes redelivery idempotent.

## Downstream offsets

Each client calls ordinary `getUpdates` with its local token. A positive offset acknowledges that client's deliveries below the offset. It does not affect any other client.

The mux returns Telegram's original update payload and `update_id`. Positive, zero, and negative offsets follow Telegram's queue semantics. Limits are capped at 100 and long polls at 50 seconds.

Updates with no remaining delivery rows are pruned outside the acknowledged safety window. A client that stops advancing eventually reaches `max_pending_per_client`; the mux then stops advancing the upstream stream rather than losing data.

## Routing

Broadcast mode targets every client.

Exclusive mode evaluates rules in order. The first matching rule owns the update. Rules may match the Telegram update type and a `callback_query.data` prefix. Unmatched updates go to the configured fallback clients.

Routing is deterministic and local. It does not execute code, call an LLM, inspect message text, or fetch remote policy.

## Bot API proxy

`getUpdates`, `setWebhook`, `deleteWebhook`, and `getWebhookInfo` are handled locally. Other Bot API methods stream to Telegram with the physical token substituted in the upstream URL. File requests under `/file/bot<TOKEN>/...` stream through the same boundary.

The mux does not retry outbound Bot API methods because many methods have side effects. Clients remain responsible for method-level retry and idempotency decisions.

## Process boundaries

- `internal/config` loads strict configuration and protected secret files.
- `internal/store` owns SQLite schema, transactions, retention, integrity checks, and backups.
- `internal/routing` parses update envelopes and chooses recipients without IO.
- `internal/telegram` owns upstream HTTP and polling.
- `internal/server` owns downstream authentication and Bot API compatibility.
- `cmd/telegram-bot-mux` wires lifecycle and signals.
