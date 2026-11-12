# Configuration

All settings are environment variables. Only `WEBHOOK_URL` is required. An invalid value stops the service at startup, and every mistake is reported at once.

Durations are written like `30s`, `5m` or `24h`.

## Webhook

| Variable | Default | Purpose |
|---|---|---|
| `WEBHOOK_URL` | | Endpoint that receives each message. Required. |
| `WEBHOOK_AUTH_USER`, `WEBHOOK_AUTH_PASS` | | HTTP basic auth. Credentials written in the URL are moved here. |
| `WEBHOOK_SECRET` | | HMAC key for the signature header. Empty sends unsigned requests. |
| `WEBHOOK_HEADERS` | | Extra static headers: `Name=value,Other=value`. |
| `WEBHOOK_TIMEOUT` | `20s` | Per attempt. |
| `WEBHOOK_RETRIES` | `2` | Extra attempts inside the SMTP conversation. |
| `PERMANENT_STATUS_CODES` | `400,406,410,413,415,422` | Answers that bounce the mail. Every other non-2xx defers it. |
| `WEBHOOK_MAX_ATTEMPTS` | `6` | Attempts of a resend, after 1 min, 5 min, 15 min, 1 h and 6 h. |
| `WEBHOOK_SWEEP_SECONDS` | `15` | How often queued resends are looked at. |

## Accepting mail

| Variable | Default | Purpose |
|---|---|---|
| `ALLOWED_DOMAINS` | all | Domains you are the final destination for. Set this in production. |
| `ALLOWED_MAILBOXES` | all | Local parts that are accepted. A `+suffix` is ignored when matching. |
| `RECIPIENT_CHECK_URL` | | Endpoint that says which addresses exist. Empty turns the check off. See [recipient-check.md](recipient-check.md). |
| `RECIPIENT_CHECK_TIMEOUT` | `5s` | |
| `RECIPIENT_RECHECK` | `24h` | How long a known address is trusted. |
| `RECIPIENT_UNKNOWN_TTL` | `5m` | How long an unknown answer is remembered. |

## Limits

`0` switches a limit off.

| Variable | Default | Purpose |
|---|---|---|
| `MAX_CONNECTIONS` | `50` | Connections at once. |
| `MAX_CONNECTIONS_PER_IP` | `10` | Connections at once from one address. |
| `MAX_CONNECTIONS_PER_IP_PER_MINUTE` | `60` | New connections from one address per minute. |
| `MAX_BAD_RECIPIENTS` | `3` | Unknown recipients before a connection is closed. |
| `BLOCK_AFTER_BAD_RECIPIENTS` | `20` | Unknown recipients from one address in ten minutes before it is blocked. |
| `BLOCK_DURATION` | `15m` | Length of that block. |

Large providers send for many customers from the same addresses. Keep the per-address limits generous.

## SMTP

| Variable | Default | Purpose |
|---|---|---|
| `SMTP_ADDR` | `:2525` | Listen address. Publish it as port 25. |
| `SMTP_DOMAIN` | `inbound.local` | Hostname in the greeting. |
| `MAX_MESSAGE_BYTES` | `26214400` | Larger messages get `552`. |
| `MAX_RECIPIENTS` | `50` | Recipients per message. |
| `MAX_LINE_BYTES` | `1048576` | Longest line accepted. |
| `READ_TIMEOUT`, `WRITE_TIMEOUT` | `60s` | Socket timeouts. |
| `TLS_CERT`, `TLS_KEY` | | Set both to offer STARTTLS. |

## Payload

| Variable | Default | Purpose |
|---|---|---|
| `PER_RECIPIENT` | `false` | `true` posts once per recipient instead of once per message. |
| `INCLUDE_ATTACHMENTS` | `true` | `false` leaves `Attachments` empty. |
| `MAX_ATTACHMENT_BYTES` | `0` | Largest attachment forwarded. `0` means no limit of its own. |
| `OVERSIZE_ATTACHMENT_POLICY` | `skip` | `skip` drops a larger attachment. `reject` refuses the message with `552`. |
| `DEDUPE_ATTACHMENT_NAMES` | `true` | Suffix repeated file names with `-2`, `-3`. |
| `INCLUDE_RAW_EMAIL` | `false` | Add the message source as base64 in `RawEmail`. |
| `FALLBACK_SENDER` | `mailer-daemon@SMTP_DOMAIN` | Sender for a message without a usable one. |
| `ENSURE_RECIPIENT_IN_TO` | `true` | Add the envelope recipient to `ToFull` when the headers do not name it. |
| `SYNTHESIZE_MESSAGE_ID` | `true` | Give a message without `Message-ID` a stable one. |
| `TEXT_FROM_HTML` | `true` | Derive `TextBody` from HTML when there is no text part. |
| `EXPAND_ATTACHED_MESSAGES` | `true` | Also forward the attachments inside an attached mail. |
| `EXPAND_TNEF` | `true` | Replace `winmail.dat` by the files inside it. |
| `STRICT_COMPAT` | `true` | Defer, instead of post, a payload a strict consumer would reject. |

## History and UI

| Variable | Default | Purpose |
|---|---|---|
| `DATA_DIR` | `/data` | Database and raw messages. |
| `RETENTION_DAYS` | `30` | Age at which history is pruned. |
| `STORE_RAW` | `true` | Keep raw messages. Needed to view content and to resend. |
| `HTTP_ADDR` | `:8080` | `/healthz` and the UI. |
| `UI_ENABLED` | `true` | `false` removes `/ui`. |
| `UI_AUTH_USER`, `UI_AUTH_PASS` | | Basic auth for the UI. Set both. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. JSON on stdout. |
