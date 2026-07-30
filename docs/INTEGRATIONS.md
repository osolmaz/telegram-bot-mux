# OpenClaw and unYOLO

Both clients use the same physical Telegram bot through separate local mux endpoints.

Assume these files:

```text
/run/secrets/telegram-token   # BotFather token, read only by the mux
/run/secrets/openclaw-token   # Local mux client token
/run/secrets/unyolo-token     # Local mux client token
```

## Broadcast mode

Broadcast mode sends every update to both clients. Each client ignores update types it does not handle.

```json
{
  "version": 1,
  "listen": "127.0.0.1:8080",
  "database": "/var/lib/telegram-bot-mux/state.db",
  "telegram": {
    "token_file": "/run/secrets/telegram-token",
    "allowed_updates": [
      "message",
      "edited_message",
      "channel_post",
      "callback_query",
      "message_reaction"
    ]
  },
  "clients": [
    {"id": "openclaw", "token_file": "/run/secrets/openclaw-token"},
    {"id": "unyolo", "token_file": "/run/secrets/unyolo-token"}
  ]
}
```

Configure OpenClaw's Telegram account with:

```json
{
  "channels": {
    "telegram": {
      "botToken": "<contents of openclaw-token>",
      "apiRoot": "http://127.0.0.1:8080/client/openclaw"
    }
  }
}
```

Configure both the unYOLO notifier and `unyolo-telegram` ingress with the contents of `unyolo-token` and this Bot API base:

```text
http://127.0.0.1:8080/client/unyolo
```

The notifier and ingress must use the same unYOLO client endpoint. Sending Bot API methods does not consume updates; `unyolo-telegram` remains that client's update reader.

## Exclusive approval routing

To keep unYOLO approval callbacks out of OpenClaw, use exclusive routing:

```json
{
  "mode": "exclusive",
  "rules": [
    {
      "clients": ["unyolo"],
      "update_types": ["callback_query"],
      "callback_data_prefixes": ["bk:"]
    }
  ],
  "fallback_clients": ["openclaw"]
}
```

Place this object in the top-level `routing` field. Approval callbacks beginning with `bk:` go only to unYOLO. Ordinary messages and other callbacks go only to OpenClaw.

## Verification

1. Start the mux and confirm `doctor` passes.
2. Start OpenClaw and unYOLO against their local API roots.
3. Send an ordinary private message. Confirm OpenClaw receives and answers it.
4. Trigger an unYOLO approval. Confirm the shared bot sends the approval message.
5. Press Approve and Deny on separate requests. Confirm unYOLO handles each callback and OpenClaw does not receive `bk:` callbacks in exclusive mode.
6. Restart the mux with one client stopped. Confirm its pending update appears after it reconnects.
