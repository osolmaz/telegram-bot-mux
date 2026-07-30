# Security model

Telegram Bot Mux separates the physical Telegram credential from downstream bot processes.

## Credentials

The BotFather token and every local client token must come from separate regular files with no group or other permissions. The mux reads them during startup and keeps them in memory.

The mux must not write credentials to:

- SQLite
- logs or error messages
- diagnostics and health responses
- command arguments
- downstream response bodies

Local client tokens authenticate clients to the mux. They are not accepted by Telegram. Each client receives a different token so the mux can maintain independent queues and reject one client using another client's API path.

## Network boundary

The default listener is `127.0.0.1:8080`. Non-loopback listeners require `allow_remote: true`. That flag only permits the bind; it does not add TLS, rate limiting, or network identity. Deploy a separate authenticated TLS boundary before exposing the API outside one trusted host.

Plain HTTP upstream roots are accepted only on loopback unless `allow_insecure_upstream` is explicitly enabled. Production Telegram traffic should use `https://api.telegram.org`.

## Downstream authority

A downstream client can call Bot API methods as the shared bot. The mux prevents it from taking ownership of update delivery by rejecting `setWebhook` and handling `deleteWebhook` locally. It does not currently restrict ordinary send, edit, delete, moderation, or callback-answer methods per client.

This project solves update-stream sharing and physical-token isolation. It does not make mutually untrusted clients safe from every action available to the shared Telegram identity. Use separate Telegram bots when visible identity or method-level separation is required.

## Stored data

SQLite contains raw Telegram updates until all targeted clients acknowledge them and the safety window expires. Those updates may contain private messages, usernames, chat identifiers, and callback data. Protect the database and its backups as private application state.

The service uses WAL mode, foreign keys, a busy timeout, and full synchronous durability. Backups use SQLite `VACUUM INTO` and are written with mode `0600`.

## Reporting vulnerabilities

Report vulnerabilities through GitHub's private security advisory flow. Do not include real bot tokens, messages, callback data, or private chat identifiers in a public issue.
