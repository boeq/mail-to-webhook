# The recipient check

A public mail server is sent mail for addresses that do not exist, mistyped or guessed. With `RECIPIENT_CHECK_URL` set, mail-to-webhook asks your application whether an address exists and refuses the rest at `RCPT TO`, before the sender transmits anything.

## The contract

One endpoint, called with the same credentials and signature headers as the webhook.

```
POST <RECIPIENT_CHECK_URL>
Content-Type: application/json
Authorization: Basic ...                      (WEBHOOK_AUTH_USER / WEBHOOK_AUTH_PASS)
X-Mail-To-Webhook-Event: recipient.check
X-Mail-To-Webhook-Timestamp: 1794504339
X-Mail-To-Webhook-Signature: sha256=...       (when WEBHOOK_SECRET is set)

{"addresses": ["AbC123@inbound.example.org"]}
```

Answer `200` with the addresses you know, written exactly as they were sent:

```json
{"known": ["AbC123@inbound.example.org"]}
```

An address missing from `known` is unknown. Any other status, a timeout or an unreadable body means "could not answer".

The request holds one address today. Accept a list anyway, so that batching can be added without a change on your side.

## What happens with the answer

| Answer | SMTP reply at `RCPT TO` | Remembered |
|---|---|---|
| Known | `250` | For `RECIPIENT_RECHECK` (24 h). No further calls for that address until then. |
| Unknown | `550 5.1.1 No such mailbox here` | For `RECIPIENT_UNKNOWN_TTL` (5 min). |
| Could not answer, address not on the list | `451`. The sender retries later. | Not at all. |
| Could not answer, address already known | `250` | The earlier answer stands. |

Two consequences:

- A new mailbox works at once, unless someone mailed it in the five minutes before it existed. Then it works when that answer expires, or when you press "Check again" on the Addresses page.
- A removed mailbox keeps receiving for up to a day. Lower `RECIPIENT_RECHECK` if that matters to you.

## Matching

The domain is lower-cased before the call. The local part is sent as the sender wrote it, because mailbox names can be case-sensitive. Whether `Box@` and `box@` are the same mailbox is for your endpoint to decide. Whatever it decides must agree with how your webhook handler finds a mailbox, or mail is accepted here and then goes nowhere there.

Only addresses in `ALLOWED_DOMAINS` are ever asked about.

## Limits

An address-guessing run costs you little:

- A repeated unknown address is answered from the list, not by another call.
- After `MAX_BAD_RECIPIENTS` (3) unknown recipients a connection is closed with `421`.
- After `BLOCK_AFTER_BAD_RECIPIENTS` (20) unknown recipients in ten minutes, the sending address gets no connections for `BLOCK_DURATION` (15 min).

Rate-limit the endpoint on your side as well. A `429` is read as "could not answer".

## What is recorded

A refusal happens before any content is sent, so there is no message to keep. The attempt is kept: time, sending address, `HELO`, envelope sender, recipient and reason. Repeats within an hour are counted on one row. The UI lists them under "Refused".

## Implementing the endpoint

1. Check the credentials the way your webhook endpoint does, and the signature if you set `WEBHOOK_SECRET`.
2. Read `addresses` as a list of plain strings. Do not validate them as email addresses: an odd address is not known, and a `422` here would defer mail that should be refused.
3. Look the strings up with the comparison your webhook handler uses to find a mailbox, and answer with the ones that exist.

Try it with curl:

```bash
curl -u user:password -H 'Content-Type: application/json' \
  -d '{"addresses": ["box@inbound.example.org", "nobody@inbound.example.org"]}' \
  https://app.example.org/inbound/recipients
# {"known":["box@inbound.example.org"]}
```
