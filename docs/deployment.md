# Deployment

## The image

```
ghcr.io/boeq/mail-to-webhook:latest
```

GitHub Actions builds the image for `linux/amd64` and `linux/arm64` after the tests are successful.

- The tag `latest` follows `main`.
- Each build also has a tag with the short commit hash.
- A release with the tag `v1.2.3` gets the image tags `1.2.3` and `1.2`.

In production, use a version tag or a commit tag, not `latest`.

A server uses only `docker-compose.yml` and `.env`. To update the service, use this command:

```bash
docker compose pull && docker compose up -d
```

## Before you set the MX record

1. Set `ALLOWED_DOMAINS`. If it is empty, the service accepts all recipient domains and posts the messages to your endpoint.
2. Set `SMTP_DOMAIN` to the hostname in the MX record. Example: `mx.example.org`. If your MX records use more than one hostname, give all of them, with commas between them.
3. Protect the UI, because it shows message content. Docker Compose binds the UI to `127.0.0.1`. Use a reverse proxy with authentication, or set `UI_AUTH_USER` and `UI_AUTH_PASS`.
4. Make sure that the size limits agree. Your endpoint and its proxy must accept a body of approximately 1.4 times `MAX_MESSAGE_BYTES`.

## DNS

| Record | Value |
|---|---|
| `MX` for the domain that receives messages | `10 mx.example.org.` |
| `A` / `AAAA` for `mx.example.org` | The IP address of the server |

## Port 25

The container listens on port 2525 as a user without privileges. Publish this port as port 25:

```yaml
ports:
  - "25:2525"
```

Make sure that your provider permits inbound connections on port 25. Open the port in the firewall.

## STARTTLS

When the service offers STARTTLS, a sender encrypts the connection. Without STARTTLS, messages arrive as clear text.

The service always accepts a sender that does not use STARTTLS.

### Automatic certificate from Let's Encrypt

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

The service orders a certificate for each hostname in `SMTP_DOMAIN`. It renews a certificate when one third of the lifetime remains.

If you set `ACME_ENABLED=true`, you accept the [Let's Encrypt Subscriber Agreement](https://letsencrypt.org/repository/).

The CA gives a certificate for a hostname only if these two conditions are true:

- The hostname points to this server in public DNS.
- Port 80 of that host connects to the service.

The CA gets a token from `http://<SMTP_DOMAIN>/.well-known/acme-challenge/...`. The listener on `ACME_HTTP_ADDR` answers that path. It answers `404` for all other paths.

The service offers STARTTLS from the moment that the certificate exists. This is usually less than one minute after the first start. A restart is not necessary. Before that moment, the service accepts messages without STARTTLS.

The service keeps the certificate and the account key in `DATA_DIR/acme`. As a result, it does not order a new certificate after a restart.

If the order fails:

- The log gives the cause.
- The service tries again after 2 minutes. The subsequent attempts come after 10, 30, 60 and 120 minutes.
- Let's Encrypt permits five failed validations for each host in one hour. This schedule stays below that limit.

For tests, set `ACME_DIRECTORY_URL` to the staging directory: `https://acme-staging-v02.api.letsencrypt.org/directory`. Clients do not trust the staging certificates. The staging limits are much higher.

`/healthz` gives the status in `starttls`. The sidebar of the UI also shows the status.

#### More than one hostname

```
SMTP_DOMAIN=mx.example.org,mx.example.net
```

- Each hostname gets its own certificate.
- If the CA refuses one hostname, the other hostnames keep their certificates.
- A sender can ask for a hostname when it starts TLS. The service then gives the certificate for that hostname.
- If the sender does not ask for a hostname, the service gives the certificate of the first hostname.
- The service orders the certificates one at a time.

#### If a web server uses port 80

Port 80 can be in use. The web server must only forward one path to the service.

Keep the listener on localhost:

```yaml
ports:
  - "25:2525"
  - "127.0.0.1:8089:8089"
```

Forward the path from the web server.

Caddy:

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

Caddy continues to get certificates for its other sites. It answers its own challenges and forwards the challenges that it does not know. `make acme-e2e` does a test of this configuration with a real Caddy.

For this configuration, the MX hostname must point to the machine of the web server. If no service on port 80 of that IP address can connect to the listener, use your own certificate.

### Your own certificate

Mount a certificate that contains all hostnames in `SMTP_DOMAIN`. Then set these values:

```
TLS_CERT=/certs/fullchain.pem
TLS_KEY=/certs/privkey.pem
```

The service reads the files when it starts. After you renew the certificate, restart the container.

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

`/healthz` answers `200` when the service can read its history.

To remove the UI, set `UI_ENABLED=false`. `/healthz` stays available.

## Data

All data is in `DATA_DIR` (`/data`, a volume):

- `app.db` contains the messages, the deliveries, the address list and the refused attempts.
- `spool/` contains the raw messages. Each message is one `.eml` file.
- `acme/` contains the certificate, its private key and the CA account key. It exists only with `ACME_ENABLED`.

The service removes messages and raw files after `RETENTION_DAYS` (30). The directory contains message content. Permit only the necessary persons to read it.

With `STORE_RAW=false`, the service keeps only metadata. You then cannot see the content or send a message again.

The history is a record. It is not a queue. The service confirms a message to the sender only after your endpoint accepts it. If you lose the volume, you lose the record. You do not lose messages.

## Memory

During the process, the service holds a message in memory more than one time: the source, the decoded attachments and the JSON.

For each message in process, make approximately five times `MAX_MESSAGE_BYTES` available. `MAX_CONNECTIONS` (50) is the maximum number of messages in process at the same time.

## Stop and upgrade

On `SIGTERM`, the service does not accept new connections. It completes the messages that are in process. Set `stop_grace_period: 90s`.

If a delivery stops before it is complete, the service did not confirm the message. The sender then sends the message again.

## Make sure that it operates

```bash
docker compose exec mail-to-webhook /usr/local/bin/mail-to-webhook healthcheck && echo ok
docker compose logs -f mail-to-webhook
```

The log is JSON, one line for each event, on stdout.

Then use "Send a test mail" in the UI. As an alternative, send a message from a different server and look for it on the Messages page.
