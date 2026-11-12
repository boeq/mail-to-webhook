# The payload

Each message is one `POST` to `WEBHOOK_URL` with a JSON body. The field names are those of Postmark's inbound webhook.

## Request

```
POST <WEBHOOK_URL>
Content-Type: application/json
User-Agent: mail-to-webhook/1.0
Authorization: Basic ...                      (when WEBHOOK_AUTH_USER is set)
X-Mail-To-Webhook-Event: message.received
X-Mail-To-Webhook-Delivery: whd_01M78CN9BW5AFKWV2X7QPN8VCD
X-Mail-To-Webhook-Timestamp: 1794504339
X-Mail-To-Webhook-Signature: sha256=3f0c...   (when WEBHOOK_SECRET is set)
```

- The delivery id is stable. Every retry of one delivery carries the same id, so you can deduplicate on it. A resend from the UI is a new delivery with a new id.
- The body is stable too. A retry or a resend posts the same bytes as the first attempt.
- Redirects are not followed. A `3xx` answer counts as a failed attempt.

## Fields

| Field | Content |
|---|---|
| `From`, `FromName`, `FromFull` | The sender. `FromFull.Email` is always a valid address (see below). |
| `To`, `ToFull`, `Cc`, `CcFull`, `Bcc`, `BccFull` | Recipients from the headers, as text and as `{Email, Name, MailboxHash}` lists. |
| `OriginalRecipient` | The envelope recipient this request is for. |
| `MailboxHash` | The part after `+` in `OriginalRecipient` (`order42` for `inbound+order42@...`). |
| `Subject`, `ReplyTo` | Decoded to UTF-8. |
| `MessageID` | The `Message-ID` header without angle brackets. |
| `Date` | Always RFC 2822, for example `Sun, 16 Aug 2026 09:20:00 +0200`. |
| `TextBody`, `HtmlBody` | The bodies, converted to UTF-8. |
| `StrippedTextReply` | `TextBody` without quoted history and signature, on a best-effort basis. |
| `Headers` | Every header as `{Name, Value}`, in the original order, duplicates included, values still encoded. |
| `Attachments` | `{Name, Content, ContentType, ContentLength, ContentID}`. `Content` is single-line base64. |
| `Envelope` | `MailFrom`, `RcptTo`, `RemoteAddr`, `Helo`, `TLS`, `ReceivedAt`, `Size`. Not part of Postmark's shape. |
| `RawEmail` | The full message source as base64. Only with `INCLUDE_RAW_EMAIL=true`. |
| `Tag` | Always empty. Present for compatibility. |

For anything security-sensitive use `Envelope.MailFrom`, not `From`. The `From` header is whatever the sender wrote.

## Normalisations

A strict consumer rejects mail that senders produce every day. Each rule below is on by default and has a setting.

| Mail as it arrives | What is posted | Setting |
|---|---|---|
| Your address is only in the envelope: a blind copy, an auto-forward, an alias | The envelope recipient is appended to `ToFull` | `ENSURE_RECIPIENT_IN_TO` |
| No `Date` header, or one that does not parse | The time the message was received | always |
| No `Message-ID`, or an empty one | `m2w.<hash of the message>@<SMTP_DOMAIN>`, the same on every attempt, in `MessageID` and in `Headers` | `SYNTHESIZE_MESSAGE_ID` |
| `From` has no usable address (`From: Mailer Daemon`, a bounce, an internationalised address) | The envelope sender, or else `FALLBACK_SENDER`. The display name is kept | `FALLBACK_SENDER` |
| HTML only | `TextBody` is derived from the HTML | `TEXT_FROM_HTML` |
| A mail forwarded as an attachment | The `.eml` is forwarded, and so are the attachments inside it | `EXPAND_ATTACHED_MESSAGES` |
| Outlook `winmail.dat` | The files inside it. The wrapper is kept only when it cannot be read | `EXPAND_TNEF` |
| An attachment without a name | `attachment.<ext>`, from its content type | always |
| Two attachments with the same name | `report.pdf`, `report-2.pdf` | `DEDUPE_ATTACHMENT_NAMES` |
| An attachment of zero bytes | Dropped | always |
| Control characters, invalid UTF-8 | Removed, or replaced by U+FFFD | always |

Attachment bytes are never changed. A text attachment keeps its original encoding.

With `STRICT_COMPAT=true` each payload is checked against the rules of a strict Postmark consumer before it is posted. A payload that would fail is not posted. The sender gets a `451` and the reason is logged, because a `422` from your endpoint would bounce the mail.

## How your answer is read

| Your answer | SMTP reply | What the sender does |
|---|---|---|
| `2xx` | `250` | Nothing more. The mail is yours. |
| `400`, `406`, `410`, `413`, `415`, `422` | `550` | Bounces the mail to its author. |
| Anything else, a timeout, no connection | `451` | Keeps the mail and retries. Postfix does so for five days by default. |

Change the middle row with `PERMANENT_STATUS_CODES`. Before answering `451`, the same request is tried `WEBHOOK_RETRIES` more times.

Keep your handler fast: accept, queue, answer. The SMTP connection stays open until you do.

A mail can reach you more than once. If your `2xx` is lost on the way, the sender tries again. mail-to-webhook recognises a repeated message and does not post it a second time, but only for as long as it keeps history. Deduplicate on `MessageID` in your handler too.

## One request per message

A message to two of your addresses is one request. Both addresses are in `ToFull`, and `OriginalRecipient` is the first. Set `PER_RECIPIENT=true` to get one request per address instead, each with its own `OriginalRecipient` and `MailboxHash`.

## Verifying the signature

The signature is `sha256=` followed by the hex HMAC-SHA256 of `<timestamp>.<raw body>`, keyed with `WEBHOOK_SECRET`. Compute it over the body exactly as received, compare in constant time, and reject a timestamp more than five minutes off.

With OpenSSL, the digest this prints is the part after `sha256=`:

```bash
printf '%s.%s' "$timestamp" "$body" | openssl dgst -sha256 -hmac "$secret" -hex
```

A complete check in Python:

```python
def verify(secret: str, timestamp: str, body: bytes, signature: str) -> bool:
    if not timestamp.isdigit() or abs(time.time() - int(timestamp)) > 300:
        return False
    expected = "sha256=" + hmac.new(secret.encode(), timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)
```

Test vector, for a body of exactly `{"a":1}`:

| | |
|---|---|
| Secret | `example-secret` |
| Timestamp | `1800000000` |
| Signature | `sha256=2ded0f9cf1c6e2fe6053dca0a54e9f11827c3fdbd7e2d4a766001a13cdfb41e9` |

A bad signature should get a `401` or `403`, not a `422`. Those are retried, so a rotated secret delays mail instead of bouncing it.

## Size

Base64 makes attachments a third larger, so a 25 MB message is a request of about 34 MB. Raise the body limit of your proxy and application to match `MAX_MESSAGE_BYTES`, or lower `MAX_MESSAGE_BYTES`. With `INCLUDE_ATTACHMENTS=false`, `Attachments` is always `[]`.
