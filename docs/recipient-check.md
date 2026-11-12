# The recipient check

A public mail server receives messages for addresses that do not exist. Some senders make an error in the address. Other senders try addresses at random.

With `RECIPIENT_CHECK_URL` set, the service asks your application if an address exists. It refuses the unknown addresses at `RCPT TO`, before the sender transmits the content.

## The contract

You supply one endpoint. The service calls it with the same credentials and signature headers as the webhook.

```
POST <RECIPIENT_CHECK_URL>
Content-Type: application/json
Authorization: Basic ...                      (WEBHOOK_AUTH_USER / WEBHOOK_AUTH_PASS)
X-Mail-To-Webhook-Event: recipient.check
X-Mail-To-Webhook-Timestamp: 1794504339
X-Mail-To-Webhook-Signature: sha256=...       (when WEBHOOK_SECRET is set)

{"addresses": ["AbC123@inbound.example.org"]}
```

Answer `200` with the addresses that you know. Write each address exactly as the request has it.

```json
{"known": ["AbC123@inbound.example.org"]}
```

An address that is not in `known` is unknown. All other status codes, a timeout or a body that is not JSON mean "no answer".

At this time, a request contains one address. Accept a list. The service can then send more than one address in the future, and you do not change your endpoint.

## What the service does with the answer

| Answer | SMTP reply at `RCPT TO` | How long the service keeps the answer |
|---|---|---|
| Known | `250` | For `RECIPIENT_RECHECK` (24 h). It does not ask again for this address in that time. |
| Unknown | `550 5.1.1 No such mailbox here` | For `RECIPIENT_UNKNOWN_TTL` (5 min). |
| No answer, and the address is not in the list | `451`. The sender tries again later. | It does not keep it. |
| No answer, and the address is known | `250` | The earlier answer stays. |

This has two results:

- A new mailbox operates immediately. There is one exception: a message came for the address in the five minutes before you made the mailbox. In that case, wait until the answer expires, or use "Check again" on the Addresses page.
- A mailbox that you removed continues to receive messages for a maximum of one day. If this is a problem, decrease `RECIPIENT_RECHECK`.

## How addresses are compared

The service changes the domain to lower case before the call. It sends the local part as the sender wrote it, because mailbox names can be case-sensitive.

Your endpoint decides if `Box@` and `box@` are the same mailbox. This decision must agree with the method that your webhook handler uses to find a mailbox. If the two do not agree, the service accepts a message that your handler cannot deliver.

The service asks only for addresses in `ALLOWED_DOMAINS`.

## Limits

A sender that tries addresses at random causes few calls to your endpoint:

- For an unknown address that comes again, the service uses the list. It does not call your endpoint again.
- After `MAX_BAD_RECIPIENTS` (3) unknown recipients, the service closes the connection with `421`.
- After `BLOCK_AFTER_BAD_RECIPIENTS` (20) unknown recipients in ten minutes, the service refuses all connections from that IP address for `BLOCK_DURATION` (15 min).

Also set a rate limit on your endpoint. The service reads `429` as "no answer".

## What the service records

The service refuses a recipient before the sender transmits the content. As a result, there is no message to keep.

The service records the attempt: the time, the IP address of the sender, `HELO`, the envelope sender, the recipient and the cause. If the same attempt comes again in the same hour, the service increases a counter on the same row. The UI shows the attempts on the "Refused" page.

## How to make the endpoint

1. Examine the credentials with the same method as your webhook endpoint. If you set `WEBHOOK_SECRET`, also verify the signature.
2. Read `addresses` as a list of text strings.
3. Do not validate the strings as email addresses. An unusual address is an unknown address. If you answer `422`, the service defers a message that it must refuse.
4. Compare the strings with the same method that your webhook handler uses to find a mailbox.
5. Answer with the addresses that exist.

Do a test with curl:

```bash
curl -u user:password -H 'Content-Type: application/json' \
  -d '{"addresses": ["box@inbound.example.org", "nobody@inbound.example.org"]}' \
  https://app.example.org/inbound/recipients
# {"known":["box@inbound.example.org"]}
```
