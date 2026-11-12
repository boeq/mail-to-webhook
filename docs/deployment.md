# Deployment

## Before you point an MX record at it

1. Set `ALLOWED_DOMAINS`. With it empty, every recipient domain is accepted and posted to your endpoint.
2. Set `SMTP_DOMAIN` to the hostname the MX record names, such as `mx.example.org`.
3. Protect the UI. It shows mail. Docker Compose binds it to `127.0.0.1`. Reach it through a reverse proxy with authentication, or set `UI_AUTH_USER` and `UI_AUTH_PASS`.
4. Match the size limits. Your endpoint and its proxy must accept a body of about 1.4 times `MAX_MESSAGE_BYTES`.

## DNS

| Record | Value |
|---|---|
| `MX` for the receiving domain | `10 mx.example.org.` |
| `A` / `AAAA` for `mx.example.org` | The server's address |

## Port 25

The container listens on 2525 as an unprivileged user. Publish it as 25:

```yaml
ports:
  - "25:2525"
```

Check that your hosting provider allows inbound connections on port 25, and open the port in the firewall.

## STARTTLS

A sending server encrypts the connection when STARTTLS is offered. Without it, mail arrives in clear text. Mount a certificate for `SMTP_DOMAIN` and set:

```
TLS_CERT=/certs/fullchain.pem
TLS_KEY=/certs/privkey.pem
```

The certificate is read at startup. Restart the container after renewing it.

## The UI behind a proxy

Caddy:

```
mail-admin.example.org {
    basic_auth {
        operator <bcrypt hash>
    }
    reverse_proxy 127.0.0.1:8080
}
```

`/healthz` answers `200` while the service can read its history. Set `UI_ENABLED=false` to remove the UI. `/healthz` stays.

## Data

Everything is under `DATA_DIR` (`/data`, a volume):

- `app.db`: messages, deliveries, the address list, refused attempts.
- `spool/`: the raw messages, one `.eml` each.

Both are pruned after `RETENTION_DAYS` (30). The directory holds mail content, so restrict who can read it. `STORE_RAW=false` keeps only metadata: nothing can then be viewed or sent again.

The history is a record, not a queue. Mail is acknowledged only once your endpoint has accepted it, so losing the volume loses the record and no mail.

## Memory

A message is held in memory while it is processed, several times over: the source, the decoded attachments and the JSON. Allow about five times `MAX_MESSAGE_BYTES` for each message in flight. `MAX_CONNECTIONS` (50) is the upper bound on how many there can be at once.

## Stopping and upgrading

On `SIGTERM` the service stops accepting connections and lets the messages in flight finish. Give it `stop_grace_period: 90s`. A message cut off mid-delivery was never acknowledged, so its sender offers it again.

## Checking that it works

```bash
docker compose exec mail-to-webhook /usr/local/bin/mail-to-webhook healthcheck && echo ok
docker compose logs -f mail-to-webhook
```

Logs are JSON lines on stdout. Then use "Send a test mail" in the UI, or send one from outside and watch it appear under Messages.
