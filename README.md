<a href="https://boeq.ai">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/header-dark.png">
    <img src=".github/assets/header-light.png" alt="mail-to-webhook by boeq: inbound email over SMTP, delivered to your webhook" width="100%">
  </picture>
</a>

# mail-to-webhook

An SMTP server for inbound mail that posts each message to your HTTP endpoint as JSON.

It is a self-hosted stand-in for Postmark's inbound webhook: the JSON has the same field names, so a handler written for Postmark keeps working. The sending server gets its `250 OK` only after your endpoint has answered `2xx`. If your endpoint is down, the sender keeps the mail and retries.

```
sender ──SMTP :25──▶ mail-to-webhook ──HTTPS POST──▶ your endpoint
                       │
                       ├─ RCPT TO   allowed domain? known address?   else 550
                       ├─ DATA      parse MIME, build JSON, sign it
                       └─ reply     2xx ▸ 250 · 400/422 ▸ 550 · anything else ▸ 451
```

## Quick start

```bash
cp .env.example .env            # set WEBHOOK_URL and ALLOWED_DOMAINS
docker compose up --build -d
```

Send it a message:

```bash
python3 - <<'EOF'
import smtplib
from email.message import EmailMessage

msg = EmailMessage()
msg["From"], msg["To"], msg["Subject"] = "you@example.com", "inbound@example.org", "Hello"
msg.set_content("First message.")
msg.add_attachment(b"%PDF-1.4", maintype="application", subtype="pdf", filename="invoice.pdf")
smtplib.SMTP("127.0.0.1", 25).send_message(msg)
EOF
```

The operator UI is at <http://127.0.0.1:8080/ui/>. To try it before you have an endpoint, `make run` starts a local instance that posts to its own test receiver.

## What it does

- Reads any mail. Every charset, nested multiparts, inline images, RFC 2047 and RFC 2231 filenames, Outlook `winmail.dat`, mail forwarded as an attachment, signed mail, bounces. A message it cannot parse is still delivered, with the original attached as `message.eml`.
- Maps the webhook answer to an SMTP reply. `2xx` accepts the mail. `400`, `406`, `410`, `413`, `415` and `422` refuse it, and the sender bounces it to its author. Every other answer, `401` and `500` included, defers it: the sender retries, and a deployment mistake delays mail instead of destroying it.
- Checks recipients before accepting mail. It can ask your application whether an address exists and refuse the rest at `RCPT TO`, before any content is sent. Answers are kept in a local list. See [docs/recipient-check.md](docs/recipient-check.md).
- Normalises what strict consumers reject. A blind-copied or forwarded message still names your address in `ToFull`. `Date` is always parseable. A message without a `Message-ID` gets a stable one. See [docs/payload.md](docs/payload.md).
- Signs each request. `X-Mail-To-Webhook-Signature` is an HMAC-SHA256 over the timestamp and the raw body.
- Offers STARTTLS. It can order its certificate from Let's Encrypt and renew it by itself. See [docs/deployment.md](docs/deployment.md#starttls).
- Keeps a history. Every message, each delivery attempt and each refused recipient is stored for 30 days and shown in the UI, where a message can be sent again.

## The payload

```json
{
  "FromName": "Alice Böhm",
  "From": "alice@sender.test",
  "FromFull": { "Email": "alice@sender.test", "Name": "Alice Böhm", "MailboxHash": "" },
  "To": "Support <inbound+order42@example.org>",
  "ToFull": [{ "Email": "inbound+order42@example.org", "Name": "Support", "MailboxHash": "order42" }],
  "Cc": "", "CcFull": [], "Bcc": "", "BccFull": [],
  "OriginalRecipient": "inbound+order42@example.org",
  "Subject": "Café invoice ☕",
  "MessageID": "abc123@sender.test",
  "Date": "Thu, 12 Nov 2026 17:25:39 +0000",
  "TextBody": "Please see attached.\r\n",
  "HtmlBody": "<p>Please see <b>attached</b>.</p>\r\n",
  "Headers": [{ "Name": "Message-ID", "Value": "<abc123@sender.test>" }],
  "Attachments": [
    { "Name": "invoice.pdf", "Content": "JVBERi0xLjQgZmFrZQ==", "ContentType": "application/pdf", "ContentLength": 13 }
  ],
  "Envelope": {
    "MailFrom": "alice@sender.test",
    "RcptTo": ["inbound+order42@example.org"],
    "RemoteAddr": "203.0.113.7",
    "Helo": "mail.sender.test",
    "TLS": true,
    "ReceivedAt": "2026-11-12T17:25:39.208Z",
    "Size": 1076
  }
}
```

`Envelope` is the one addition to Postmark's shape. It holds what the SMTP conversation said, which the headers of a message cannot be trusted for. The full field list is in [docs/payload.md](docs/payload.md).

## Settings you will need

| Variable | Purpose |
|---|---|
| `WEBHOOK_URL` | Your endpoint. The only required setting. |
| `WEBHOOK_AUTH_USER`, `WEBHOOK_AUTH_PASS` | HTTP basic auth for the endpoint. |
| `WEBHOOK_SECRET` | Key for the signature header. |
| `ALLOWED_DOMAINS` | The domains you receive mail for. Leaving it empty accepts mail for any domain. |
| `RECIPIENT_CHECK_URL` | Endpoint that says which addresses exist. Empty turns the check off. |
| `SMTP_DOMAIN` | The hostname your MX record points to. |
| `ACME_ENABLED` | Get a STARTTLS certificate from Let's Encrypt and keep it renewed. |
| `TLS_CERT`, `TLS_KEY` | Or a certificate of your own for STARTTLS. |
| `UI_AUTH_USER`, `UI_AUTH_PASS` | Basic auth for the operator UI. |

All settings are in [docs/configuration.md](docs/configuration.md). Running it as a real MX is in [docs/deployment.md](docs/deployment.md).

## What it does not do

- It does not score spam. It does not check SPF, DKIM or DMARC, and it does not look at content. All headers reach your endpoint, so you can check them there. The recipient check stops mail to addresses that do not exist. It does not stop spam sent to one that does.
- It has no queue of its own. Mail is never accepted and held. Reliability comes from the sending server's queue, which every mail server has.
- It does not send mail.

## Development

```bash
make check    # vet, unit tests, and an end-to-end run over real SMTP
```

Go runs in a container when it is not installed, so Docker, Node and Python 3 are enough. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

<a href="https://boeq.ai">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/footer-dark.png">
    <img src=".github/assets/footer-light.png" alt="Built by boeq. boeq.ai" width="100%">
  </picture>
</a>
