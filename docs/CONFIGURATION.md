# Configuration

Telegram Bot Mux loads one strict JSON configuration file at startup. The file contains paths to credentials, not credential values.

## Minimal file

```json
{
  "version": 1,
  "database": "/var/lib/telegram-bot-mux/state.db",
  "telegram": {
    "token_file": "/run/secrets/telegram-token"
  },
  "clients": [
    {
      "id": "assistant",
      "token_file": "/run/secrets/assistant-token"
    }
  ]
}
```

Validate a file with:

```sh
telegram-bot-mux doctor --config /absolute/path/config.json --offline
```

Unknown fields, trailing JSON values, missing required fields, duplicate identifiers, and invalid values are fatal.

## Fields

| Field | Required | Type | Meaning |
| --- | --- | --- | --- |
| `version` | Yes | integer | Configuration schema version. Must be `1`. |
| `listen` | No | string | Downstream HTTP address. Defaults to `127.0.0.1:8080`. |
| `allow_remote` | No | boolean | Permits a non-loopback listener. Defaults to `false`. |
| `database` | Yes | string | Absolute SQLite path. |
| `telegram` | Yes | object | Physical Telegram connection. |
| `clients` | Yes | array | Independent downstream queues and credentials. |
| `routing` | No | object | Delivery policy. Defaults to broadcast. |
| `retention` | No | object | Backlog and acknowledged-update bounds. |

## `telegram`

| Field | Required | Type | Default |
| --- | --- | --- | --- |
| `token_file` | Yes | string | None |
| `api_base` | No | URL | `https://api.telegram.org` |
| `file_base` | No | URL | Value of `api_base` |
| `allow_insecure_upstream` | No | boolean | `false` |
| `poll_timeout_seconds` | No | integer | `30` |
| `max_retry_seconds` | No | integer | `30` |
| `allowed_updates` | No | string array | Telegram default |

`token_file` must be absolute and point to a regular file with no group or other permissions. It must contain exactly one nonempty line.

`api_base` and `file_base` must be absolute HTTP or HTTPS URLs without a query or fragment. Plain HTTP is accepted on loopback. Plain HTTP to another host requires `allow_insecure_upstream: true`.

`poll_timeout_seconds` must be between 1 and 50. `max_retry_seconds` must be between 1 and 600.

`allowed_updates` is sent to Telegram's single upstream `getUpdates` call. Downstream `allowed_updates` parameters are accepted for client compatibility but do not change the shared upstream subscription.

## `clients`

Each client has these fields:

| Field | Required | Type | Meaning |
| --- | --- | --- | --- |
| `id` | Yes | string | Durable queue and routing identity. |
| `token_file` | Yes | string | Absolute path to the local client token. |

Client IDs must match `^[a-z][a-z0-9-]{0,31}$` and be unique.

The token file follows the same file-permission and one-line rules as the Telegram token. A local token must use Telegram's token shape so unmodified Bot API libraries accept it: 6 to 12 digits, a colon, and at least 32 URL-safe secret characters. Client tokens must be unique and must not equal the physical Telegram token.

Generate one safely:

```sh
telegram-bot-mux generate-client-token --out /run/secrets/client-token
```

The command creates a new `0600` file and refuses to overwrite an existing path.

## `routing`

### Broadcast

Broadcast is the default:

```json
{
  "mode": "broadcast"
}
```

Every update targets every client. Broadcast mode must not contain `rules` or `fallback_clients`.

### Exclusive

Exclusive routing sends an update to the first matching rule. Unmatched updates go to the fallback clients.

```json
{
  "mode": "exclusive",
  "rules": [
    {
      "clients": ["approvals"],
      "update_types": ["callback_query"],
      "callback_data_prefixes": ["bk:"]
    }
  ],
  "fallback_clients": ["assistant"]
}
```

Each rule requires:

- one or more existing `clients`
- at least one `update_types` or `callback_data_prefixes` condition

When both conditions are present, both must match. Rules are evaluated in file order. Client names and condition values must not be duplicated inside one list.

`callback_data_prefixes` matches only `callback_query.data`. It does not inspect message text.

## `retention`

| Field | Required | Type | Default |
| --- | --- | --- | --- |
| `max_pending_per_client` | No | positive integer | `10000` |
| `acknowledged_safety_window` | No | nonnegative integer | `1000` |

`max_pending_per_client` is a loss-prevention bound. If an ingest transaction would exceed it, the transaction rolls back and upstream polling pauses.

`acknowledged_safety_window` keeps that many recent update IDs after every targeted client acknowledges them. The rows no longer have delivery records and are not returned again. Set it to `0` to prune acknowledged rows immediately.

## Loading and validation

The process performs these steps before serving:

1. Read at most 1 MiB from the absolute configuration path.
2. Decode exactly one JSON object and reject unknown fields.
3. Apply documented defaults.
4. Validate addresses, URLs, identifiers, references, and bounds.
5. Read and validate each protected secret file.
6. Open and migrate SQLite.
7. Reconcile configured client identities.
8. Start the downstream listener and upstream poller.

The runtime does not reread configuration or secret files. Restart it to apply changes.

## Boundaries

The configuration does not contain inline credentials, executable hooks, remote routing rules, inherited files, environment expansion, or extension maps. Version `1` readers reject any other version.
