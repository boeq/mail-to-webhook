# Configuration

All settings are environment variables. Only `WEBHOOK_URL` is mandatory.

If a value is not valid, the service does not start. It reports all errors at the same time.

Write a duration as `30s`, `5m` or `24h`.

## Webhook

| Variable | Default | Purpose |
|---|---|---|
| `WEBHOOK_URL` | | The endpoint that receives each message. Mandatory. |
| `WEBHOOK_AUTH_USER`, `WEBHOOK_AUTH_PASS` | | HTTP basic authentication. If the URL contains credentials, the service moves them to these settings. |
| `WEBHOOK_SECRET` | | The HMAC key for the signature header. If it is empty, the requests have no signature. |
| `WEBHOOK_HEADERS` | | More static headers: `Name=value,Other=value`. |
| `WEBHOOK_TIMEOUT` | `20s` | The time limit for each attempt. |
| `WEBHOOK_RETRIES` | `2` | The number of additional attempts during the SMTP session. |
| `PERMANENT_STATUS_CODES` | `400,406,410,413,415,422` | The answers that make the sender return the message. All other answers that are not `2xx` defer the message. |
| `WEBHOOK_MAX_ATTEMPTS` | `6` | The number of attempts for a resend. The attempts come after 1 min, 5 min, 15 min, 1 h and 6 h. |
| `WEBHOOK_SWEEP_SECONDS` | `15` | The interval, in seconds, at which the service looks for resends in the queue. |

## Message acceptance

| Variable | Default | Purpose |
|---|---|---|
| `ALLOWED_DOMAINS` | all | The domains for which you are the final destination. Set this in production. |
| `ALLOWED_MAILBOXES` | all | The local parts that the service accepts. The comparison ignores a `+suffix`. |
| `RECIPIENT_CHECK_URL` | | The endpoint that tells which addresses exist. If it is empty, the check is off. Refer to [recipient-check.md](recipient-check.md). |
| `RECIPIENT_CHECK_TIMEOUT` | `5s` | The time limit for one call. |
| `RECIPIENT_RECHECK` | `24h` | The time for which the service trusts a known address. |
| `RECIPIENT_UNKNOWN_TTL` | `5m` | The time for which the service keeps an unknown answer. |

## Limits

`0` sets a limit to off.

| Variable | Default | Purpose |
|---|---|---|
| `MAX_CONNECTIONS` | `50` | The maximum number of connections at the same time. |
| `MAX_CONNECTIONS_PER_IP` | `10` | The maximum number of connections at the same time from one IP address. |
| `MAX_CONNECTIONS_PER_IP_PER_MINUTE` | `60` | The maximum number of new connections from one IP address in one minute. |
| `MAX_BAD_RECIPIENTS` | `3` | The number of unknown recipients after which the service closes a connection. |
| `BLOCK_AFTER_BAD_RECIPIENTS` | `20` | The number of unknown recipients from one IP address in ten minutes after which the service blocks that address. |
| `BLOCK_DURATION` | `15m` | The duration of the block. |

Large providers send messages for many customers from the same IP addresses. Do not set the limits for each IP address too low.

## SMTP

| Variable | Default | Purpose |
|---|---|---|
| `SMTP_ADDR` | `:2525` | The listen address. Publish it as port 25. |
| `SMTP_DOMAIN` | `inbound.local` | The hostname of the server, as in the MX record. You can give more than one hostname, with commas between them. The service uses the first hostname in the greeting, in the default of `FALLBACK_SENDER` and in the message IDs that it makes. |
| `MAX_MESSAGE_BYTES` | `26214400` | A larger message gets `552`. |
| `MAX_RECIPIENTS` | `50` | The maximum number of recipients for each message. |
| `MAX_LINE_BYTES` | `1048576` | The maximum length of a line. |
| `READ_TIMEOUT`, `WRITE_TIMEOUT` | `60s` | The socket time limits. |

## STARTTLS

Without a certificate, messages arrive without encryption. There are two methods to get a certificate. Use only one. Refer to [deployment.md](deployment.md#starttls).

| Variable | Default | Purpose |
|---|---|---|
| `ACME_ENABLED` | `false` | The service gets a certificate for each hostname in `SMTP_DOMAIN` from Let's Encrypt and renews it. If you set it to `true`, you accept the terms of service of the CA. |
| `ACME_EMAIL` | | A contact address for the CA. Optional. |
| `ACME_DIRECTORY_URL` | Let's Encrypt | The directory of a different ACME CA, or of the Let's Encrypt staging environment. |
| `ACME_HTTP_ADDR` | `:8089` | The listener for the validation request of the CA. Publish it as port 80. |
| `TLS_CERT`, `TLS_KEY` | | Your own certificate and key, as PEM files. Set the two together. The certificate must contain all hostnames in `SMTP_DOMAIN`. |

## Payload

| Variable | Default | Purpose |
|---|---|---|
| `PER_RECIPIENT` | `false` | `true` posts one request for each recipient and not one for each message. |
| `INCLUDE_ATTACHMENTS` | `true` | `false` keeps `Attachments` empty. |
| `MAX_ATTACHMENT_BYTES` | `0` | The maximum size of an attachment that the service forwards. `0` sets no limit. |
| `OVERSIZE_ATTACHMENT_POLICY` | `skip` | `skip` removes a larger attachment. `reject` refuses the message with `552`. |
| `DEDUPE_ATTACHMENT_NAMES` | `true` | Adds `-2`, `-3` to file names that occur more than one time. |
| `INCLUDE_RAW_EMAIL` | `false` | Adds the message source as base64 in `RawEmail`. |
| `FALLBACK_SENDER` | `mailer-daemon@SMTP_DOMAIN` | The sender for a message that has no sender you can use. |
| `ENSURE_RECIPIENT_IN_TO` | `true` | Adds the envelope recipient to `ToFull` if the headers do not contain it. |
| `SYNTHESIZE_MESSAGE_ID` | `true` | Gives a stable ID to a message without `Message-ID`. |
| `TEXT_FROM_HTML` | `true` | Makes `TextBody` from the HTML if there is no text part. |
| `EXPAND_ATTACHED_MESSAGES` | `true` | Also forwards the attachments in an attached message. |
| `EXPAND_TNEF` | `true` | Replaces `winmail.dat` with the files in it. |
| `STRICT_COMPAT` | `true` | Defers a payload that a strict consumer rejects. The service does not post it. |

## History and UI

| Variable | Default | Purpose |
|---|---|---|
| `DATA_DIR` | `/data` | The database and the raw messages. |
| `RETENTION_DAYS` | `30` | The age, in days, at which the service removes history. |
| `STORE_RAW` | `true` | Keeps the raw messages. This is necessary to show the content and to send a message again. |
| `HTTP_ADDR` | `:8080` | `/healthz` and the UI. |
| `UI_ENABLED` | `true` | `false` removes `/ui`. |
| `UI_AUTH_USER`, `UI_AUTH_PASS` | | Basic authentication for the UI. Set the two together. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. The log is JSON on stdout. |
