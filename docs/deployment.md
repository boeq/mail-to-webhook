# Deployment

## The image

```
ghcr.io/boeq/mail-to-webhook:latest
```

GitHub Actions builds it for `linux/amd64` and `linux/arm64` after the tests pass. `latest` follows `main`. Each build also has a tag with its short commit hash, and a release tagged `v1.2.3` is published as `1.2.3` and `1.2`. Pin one of those in production.

A server needs only `docker-compose.yml` and `.env`. To update:

```bash
docker compose pull && docker compose up -d
```

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

A sending server encrypts the connection when STARTTLS is offered. Without it, mail arrives in clear text. Senders that skip STARTTLS are still accepted either way.

### Automatic, with Let's Encrypt

```
SMTP_DOMAIN=mx.example.org
ACME_ENABLED=true
ACME_EMAIL=you@example.org
```

```yaml
ports:
  - "25:2525"
  - "80:8089"
```

The service orders a certificate for `SMTP_DOMAIN` and renews it when a third of its lifetime is left. Setting `ACME_ENABLED=true` accepts the [Let's Encrypt Subscriber Agreement](https://letsencrypt.org/repository/).

Two things must be true before the CA will issue:

- `SMTP_DOMAIN` resolves to this server in public DNS.
- Port 80 of that host reaches the service. The CA fetches a token from `http://<SMTP_DOMAIN>/.well-known/acme-challenge/...`. The listener on `ACME_HTTP_ADDR` answers that path and returns `404` for everything else.

STARTTLS is offered from the moment the certificate exists, usually within a minute of the first start, and no restart is needed. Until then mail is accepted without it. The certificate and the account key are kept in `DATA_DIR/acme`, so a restart does not order again.

If the order fails, the reason is in the log and the next attempt comes after 2 minutes, then 10, 30, 60 and 120. Let's Encrypt allows five failed validations per host and hour, and this stays below it. While you are setting things up, point `ACME_DIRECTORY_URL` at the staging directory, `https://acme-staging-v02.api.letsencrypt.org/directory`. Its certificates are not trusted, and its limits are far higher.

`/healthz` reports the state under `starttls`, and the sidebar of the UI shows it.

#### When a web server already holds port 80

Port 80 does not have to be free. It has to pass that one path on. Keep the listener on localhost:

```yaml
ports:
  - "25:2525"
  - "127.0.0.1:8089:8089"
```

and forward the path from the web server. Caddy:

```
http://mx.example.org {
    reverse_proxy /.well-known/acme-challenge/* 127.0.0.1:8089
}
```

nginx:

```nginx
server {
    listen 80;
    server_name mx.example.org;
    location /.well-known/acme-challenge/ {
        proxy_pass http://127.0.0.1:8089;
    }
}
```

A Caddy that obtains certificates for its other sites keeps doing so: it answers its own challenges and forwards the ones it does not know. `make acme-e2e` tests this arrangement against a real Caddy.

This needs the MX hostname to resolve to the machine the web server runs on. If nothing on port 80 of that address can reach the service, use a certificate of your own.

### With your own certificate

Mount a certificate for `SMTP_DOMAIN` and set:

```
TLS_CERT=/certs/fullchain.pem
TLS_KEY=/certs/privkey.pem
```

The files are read at startup. Restart the container after renewing them.

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
- `acme/`: the certificate, its private key and the CA account key, when `ACME_ENABLED` is set.

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
