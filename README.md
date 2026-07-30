# Telegram Bot Mux

Telegram Bot Mux is a durable Telegram Bot API multiplexer. It lets several independent bot processes share one physical Telegram bot without competing for Telegram's single `getUpdates` stream.

The mux polls Telegram once, stores each update in SQLite, and gives every configured client its own Telegram-compatible update queue. Other Bot API calls and file downloads pass through to Telegram.

## Install

Build the static Go binary:

```sh
go install github.com/osolmaz/telegram-bot-mux/cmd/telegram-bot-mux@latest
```

Or run the container image from a published [release](https://github.com/osolmaz/telegram-bot-mux/releases):

```sh
docker run --rm ghcr.io/osolmaz/telegram-bot-mux:<tag> version
```

## Configure

Create one protected file containing the BotFather token and one local token file for each client:

```sh
install -m 600 /dev/null /run/secrets/telegram-token
telegram-bot-mux generate-client-token --out /run/secrets/openclaw-token
telegram-bot-mux generate-client-token --out /run/secrets/unyolo-token
```

Put the BotFather token in `/run/secrets/telegram-token`, then create `config.json`:

```json
{
  "version": 1,
  "listen": "127.0.0.1:8080",
  "database": "/var/lib/telegram-bot-mux/state.db",
  "telegram": {
    "token_file": "/run/secrets/telegram-token",
    "allowed_updates": ["message", "callback_query"]
  },
  "clients": [
    {
      "id": "openclaw",
      "token_file": "/run/secrets/openclaw-token"
    },
    {
      "id": "unyolo",
      "token_file": "/run/secrets/unyolo-token"
    }
  ]
}
```

Validate it without contacting Telegram:

```sh
telegram-bot-mux doctor --config /etc/telegram-bot-mux/config.json --offline
```

See [configuration](docs/CONFIGURATION.md) for routing, retention, and validation rules.

## Run

```sh
telegram-bot-mux serve --config /etc/telegram-bot-mux/config.json
```

Each client uses its own API root and local token:

```text
http://127.0.0.1:8080/client/openclaw
http://127.0.0.1:8080/client/unyolo
```

A Telegram library appends `/bot<TOKEN>/<method>` to that root as usual. Configure OpenClaw with the first root and unYOLO with the second. See [OpenClaw and unYOLO integration](docs/INTEGRATIONS.md) for complete examples.

## Guarantees

- The upstream offset advances only after the complete update batch is durable.
- Clients acknowledge updates independently through ordinary Telegram offsets.
- A restart cannot lose an update already acknowledged to Telegram.
- Backlog overflow pauses upstream polling instead of dropping updates.
- The BotFather token never enters SQLite or downstream responses.
- Uploads, downloads, and Bot API responses stream through the proxy.

The default listener is loopback-only. Do not expose the client API publicly without a separate authenticated transport boundary.

## Operations

Check connectivity and database integrity:

```sh
telegram-bot-mux doctor --config /etc/telegram-bot-mux/config.json
```

Create a consistent SQLite backup:

```sh
telegram-bot-mux backup \
  --config /etc/telegram-bot-mux/config.json \
  --out /var/backups/telegram-bot-mux/state.db
```

See [operations](docs/OPERATIONS.md) for health checks, shutdown behavior, recovery, and backlog handling.

## License

[MIT](LICENSE)
