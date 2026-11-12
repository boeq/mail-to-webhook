# The payload

The service sends each message as one `POST` to `WEBHOOK_URL`. The body is JSON. The field names are the same as in the inbound webhook of Postmark.

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

- The delivery ID does not change. Each retry of one delivery has the same ID. Use the ID to find duplicates. A resend from the UI is a new delivery with a new ID.
- The body does not change. A retry or a resend posts the same bytes as the first attempt.
- The service does not follow redirects. A `3xx` answer is a failed attempt.

## Fields

| Field | Content |
|---|---|
| `From`, `FromName`, `FromFull` | The sender. `FromFull.Email` is always a valid address. Refer to [Normalisations](#normalisations). |
| `To`, `ToFull`, `Cc`, `CcFull`, `Bcc`, `BccFull` | The recipients from the headers, as text and as lists of `{Email, Name, MailboxHash}`. |
| `OriginalRecipient` | The envelope recipient of this request. |
| `MailboxHash` | The part after `+` in `OriginalRecipient`. Example: `order42` for `inbound+order42@...`. |
| `Subject`, `ReplyTo` | Decoded to UTF-8. |
| `MessageID` | The `Message-ID` header without angle brackets. |
| `Date` | Always in RFC 2822 format. Example: `Sun, 16 Aug 2026 09:20:00 +0200`. |
| `TextBody`, `HtmlBody` | The bodies in UTF-8. |
| `StrippedTextReply` | `TextBody` without the quoted history and the signature. The result is an estimate. |
| `Headers` | All headers as `{Name, Value}`. The order is the original order. Duplicates stay. The values stay encoded. |
| `Attachments` | `{Name, Content, ContentType, ContentLength, ContentID}`. `Content` is base64 on one line. |
| `Envelope` | `MailFrom`, `RcptTo`, `RemoteAddr`, `Helo`, `TLS`, `ReceivedAt`, `Size`. This field is not in the Postmark format. |
| `RawEmail` | The full message source as base64. Only with `INCLUDE_RAW_EMAIL=true`. |
| `Tag` | Always empty. It is there for compatibility. |

For security decisions, use `Envelope.MailFrom` and not `From`. The sender writes the `From` header and can write any value.

## Normalisations

A normalisation is a correction that the service makes to the message data.

A strict consumer rejects some messages that senders make each day. The service corrects these messages. Each rule in the table is on by default.

| Message as it arrives | What the service posts | Setting |
|---|---|---|
| Your address is only in the envelope: a blind copy, an automatic forward, an alias | The service adds the envelope recipient to `ToFull` | `ENSURE_RECIPIENT_IN_TO` |
| No `Date` header, or a `Date` header that a parser cannot read | The time of receipt | always |
| No `Message-ID`, or an empty `Message-ID` | `m2w.<hash of the message>@<SMTP_DOMAIN>` in `MessageID` and in `Headers`. The value is the same on each attempt | `SYNTHESIZE_MESSAGE_ID` |
| `From` has no address that you can use (`From: Mailer Daemon`, a bounce, an internationalised address) | The envelope sender. If there is none, `FALLBACK_SENDER`. The display name stays | `FALLBACK_SENDER` |
| HTML only | The service makes `TextBody` from the HTML | `TEXT_FROM_HTML` |
| A message forwarded as an attachment | The `.eml` file and also the attachments in it | `EXPAND_ATTACHED_MESSAGES` |
| Outlook `winmail.dat` | The files in it. The service keeps `winmail.dat` only if it cannot read the file | `EXPAND_TNEF` |
| An attachment without a name | `attachment.<ext>`. The extension comes from the content type | always |
| Two attachments with the same name | `report.pdf`, `report-2.pdf` | `DEDUPE_ATTACHMENT_NAMES` |
| An attachment of zero bytes | The service removes it | always |
| Control characters, invalid UTF-8 | The service removes them or replaces them with U+FFFD | always |

The service does not change the bytes of an attachment. A text attachment keeps its original encoding.

With `STRICT_COMPAT=true`, the service examines each payload before it posts the payload. It uses the rules of a strict Postmark consumer.

If a payload does not obey the rules, the service does not post it. The sender gets `451`, and the log gives the cause. Without this check, your endpoint answers `422` and the sender returns the message to the author.

## How the service reads your answer

| Your answer | SMTP reply | What the sender does |
|---|---|---|
| `2xx` | `250` | Nothing more. You have the message. |
| `400`, `406`, `410`, `413`, `415`, `422` | `550` | It returns the message to the author. |
| All other answers, a timeout, no connection | `451` | It keeps the message and tries again. Postfix does this for five days by default. |

To change the second row, set `PERMANENT_STATUS_CODES`. Before the service answers `451`, it tries the same request `WEBHOOK_RETRIES` more times.

Make your handler fast. Accept the request, put the work in a queue, then answer. The SMTP connection stays open until you answer.

You can receive a message more than one time. If your `2xx` answer does not arrive, the sender tries again. The service knows a message that it posted before and does not post it again. This is true only while the message is in the history. For this reason, also look for duplicates by `MessageID` in your handler.

## One request for each message

A message to two of your addresses is one request. The two addresses are in `ToFull`. `OriginalRecipient` is the first address.

With `PER_RECIPIENT=true`, you get one request for each address. Each request then has its own `OriginalRecipient` and `MailboxHash`.

## Signature verification

The signature is `sha256=` and then the hexadecimal HMAC-SHA256 of `<timestamp>.<raw body>`. The key is `WEBHOOK_SECRET`.

1. Calculate the HMAC from the body exactly as you received it.
2. Compare the result with the signature in constant time.
3. Reject a timestamp that is more than five minutes from the current time.

With OpenSSL, this command prints the part after `sha256=`:

```bash
printf '%s.%s' "$timestamp" "$body" | openssl dgst -sha256 -hmac "$secret" -hex
```

A full check in Python:

```python
def verify(secret: str, timestamp: str, body: bytes, signature: str) -> bool:
    if not timestamp.isdigit() or abs(time.time() - int(timestamp)) > 300:
        return False
    expected = "sha256=" + hmac.new(secret.encode(), timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)
```

Test vector for a body that is exactly `{"a":1}`:

| | |
|---|---|
| Secret | `example-secret` |
| Timestamp | `1800000000` |
| Signature | `sha256=2ded0f9cf1c6e2fe6053dca0a54e9f11827c3fdbd7e2d4a766001a13cdfb41e9` |

Answer a bad signature with `401` or `403`, not with `422`. The service tries these answers again. As a result, a changed secret causes a delay and not a bounce.

## Size

Base64 makes each attachment one third larger. As a result, a 25 MB message becomes a request of approximately 34 MB.

Increase the body limit of your proxy and your application to agree with `MAX_MESSAGE_BYTES`. As an alternative, decrease `MAX_MESSAGE_BYTES`.

With `INCLUDE_ATTACHMENTS=false`, `Attachments` is always `[]`.
