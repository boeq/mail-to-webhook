#!/usr/bin/env python3
"""End-to-end check: send real mail over SMTP, assert on the webhook JSON.

Usage: python3 scripts/e2e_test.py /path/to/inbound-binary
"""
import base64
import email.utils
import re
import hashlib
import hmac
import json
import os
import smtplib
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import urllib.error
import urllib.request
from email.message import EmailMessage
from http.server import BaseHTTPRequestHandler, HTTPServer

EMAIL_RE = re.compile(
    r"^[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*"
    r"@(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$")


def is_email(v):
    return isinstance(v, str) and bool(EMAIL_RE.match(v)) and len(v) <= 254


def validate_payload(p):
    """Independent re-implementation of the consumer's validation rules.

    Deliberately written from the rules themselves rather than from the Go
    code, so a mistake in the Go validator does not hide behind the same
    mistake here.
    """
    errs = []

    def req_str(field, v):
        if v is None or v == "" or not isinstance(v, str):
            errs.append(f"{field} fails required|string")

    ff = p.get("FromFull")
    if ff is None:
        errs.append("FromFull fails required")
    elif not isinstance(ff, dict):
        errs.append("FromFull fails array")
    elif not is_email(ff.get("Email")):
        errs.append(f"FromFull.Email fails email ({ff.get('Email')!r})")

    tf = p.get("ToFull")
    if tf is None:
        errs.append("ToFull fails required")
    elif not isinstance(tf, list):
        errs.append("ToFull fails array")
    elif len(tf) < 1:
        errs.append("ToFull fails min:1")
    else:
        for i, e in enumerate(tf):
            if not isinstance(e, dict) or not is_email(e.get("Email")):
                errs.append(f"ToFull.{i}.Email fails email")

    cc = p.get("CcFull")
    if cc is not None:
        if not isinstance(cc, list):
            errs.append("CcFull fails array")
        else:
            for i, e in enumerate(cc):
                if not isinstance(e, dict) or not is_email(e.get("Email")):
                    errs.append(f"CcFull.{i}.Email fails email")

    for f in ("Subject", "TextBody", "HtmlBody", "Date"):
        v = p.get(f)
        if v is not None and not isinstance(v, str):
            errs.append(f"{f} fails string")

    hs = p.get("Headers")
    if hs is not None:
        if not isinstance(hs, list):
            errs.append("Headers fails array")
        else:
            for i, h in enumerate(hs):
                req_str(f"Headers.{i}.Name", h.get("Name"))
                if h.get("Value") is not None and not isinstance(h.get("Value"), str):
                    errs.append(f"Headers.{i}.Value fails string")

    ats = p.get("Attachments")
    if ats is not None:
        if not isinstance(ats, list):
            errs.append("Attachments fails array")
        else:
            for i, a in enumerate(ats):
                for k in ("Name", "Content", "ContentType"):
                    req_str(f"Attachments.{i}.{k}", a.get(k))
                if a.get("ContentID") is not None and not isinstance(a.get("ContentID"), str):
                    errs.append(f"Attachments.{i}.ContentID fails string")
    return errs


SECRET = "s3cret"
received = []
mode = {"status": 200, "status_for": {}}

# The recipient check, as docs/recipient-check.md describes it.
known = set()
lookups = []
check = {"status": 200}


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        if self.path == "/recipients":
            return self.recipients(json.loads(body))
        doc = json.loads(body)
        received.append({"headers": dict(self.headers), "body": doc, "raw": body})
        status = mode["status_for"].get(doc.get("OriginalRecipient"), mode["status"])
        self.send_response(status)
        self.end_headers()
        self.wfile.write(b"{}")

    def recipients(self, doc):
        lookups.append({"headers": dict(self.headers), "addresses": doc["addresses"]})
        if check["status"] != 200:
            self.send_response(check["status"])
            self.end_headers()
            return
        answer = json.dumps({"known": [a for a in doc["addresses"] if a in known]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(answer)

    def log_message(self, *a):
        pass


def wait_port(port, timeout=10):
    import socket
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            socket.create_connection(("127.0.0.1", port), 0.2).close()
            return True
        except OSError:
            time.sleep(0.1)
    return False


def classic(binary, failures):
    """The scenarios of the original design: one POST per recipient, no recipient check."""
    data_dir = tempfile.mkdtemp(prefix="m2w-e2e-")

    env = dict(os.environ,
               SMTP_ADDR="127.0.0.1:12525",
               SMTP_DOMAIN="mx.test",
               WEBHOOK_URL="http://127.0.0.1:9099/inbound",
               WEBHOOK_SECRET=SECRET,
               WEBHOOK_RETRIES="1",
               ALLOWED_DOMAINS="example.org",
               INCLUDE_RAW_EMAIL="true",
               FALLBACK_SENDER="mailer-daemon@mx.test",
               DEDUPE_ATTACHMENT_NAMES="true",
               MAX_ATTACHMENT_BYTES="4096",
               HTTP_ADDR="127.0.0.1:9098",
               DATA_DIR=data_dir,
               PER_RECIPIENT="true",
               MAX_LINE_BYTES="2000",
               MAX_CONNECTIONS_PER_IP_PER_MINUTE="0")
    proc = subprocess.Popen([binary], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    try:
        assert wait_port(12525), "smtp did not start"

        # --- 1. happy path -------------------------------------------------
        msg = EmailMessage()
        msg["From"] = "Alice Böhm <alice@sender.test>"
        msg["To"] = "Support <inbound+order42@example.org>"
        msg["Cc"] = "watcher@example.org"
        msg["Subject"] = "Café invoice ☕"
        msg["Message-ID"] = "<abc123@sender.test>"
        msg.set_content("Please see attached.\n\nOn Mon Bob wrote:\n> old\n")
        msg.add_alternative("<p>Please see <b>attached</b>.</p>", subtype="html")
        msg.add_attachment(b"%PDF-1.4 fake", maintype="application",
                           subtype="pdf", filename="invoice.pdf")

        with smtplib.SMTP("127.0.0.1", 12525, timeout=10) as s:
            s.send_message(msg)
        time.sleep(0.6)

        # To + Cc are two envelope recipients, so we expect one POST each.
        if len(received) != 2:
            failures.append(f"expected 2 webhooks, got {len(received)}")
        else:
            r = next(x for x in received
                     if x["body"]["OriginalRecipient"] == "inbound+order42@example.org")
            p = r["body"]
            checks = {
                "FromName": (p["FromName"], "Alice Böhm"),
                "From": (p["From"], "alice@sender.test"),
                "Subject": (p["Subject"], "Café invoice ☕"),
                "MailboxHash": (p["MailboxHash"], "order42"),
                "OriginalRecipient": (p["OriginalRecipient"], "inbound+order42@example.org"),
                "StrippedTextReply": (p["StrippedTextReply"], "Please see attached."),
                "attachment name": (p["Attachments"][0]["Name"], "invoice.pdf"),
                "attachment bytes": (base64.b64decode(p["Attachments"][0]["Content"]), b"%PDF-1.4 fake"),
                "envelope MailFrom": (p["Envelope"]["MailFrom"], "alice@sender.test"),
                "envelope rcpt count": (len(p["Envelope"]["RcptTo"]), 2),
                "MessageID": (p["MessageID"], "abc123@sender.test"),
            }
            for name, (got, want) in checks.items():
                if got != want:
                    failures.append(f"{name}: got {got!r} want {want!r}")
            if "Please see" not in p["TextBody"]:
                failures.append(f"TextBody: {p['TextBody']!r}")
            if "<b>attached</b>" not in p["HtmlBody"]:
                failures.append(f"HtmlBody: {p['HtmlBody']!r}")
            if not p["RawEmail"]:
                failures.append("RawEmail empty")
            if not any(h["Name"] == "Message-ID" for h in p["Headers"]):
                failures.append("headers missing Message-ID")

            # signature
            ts = r["headers"]["X-Mail-To-Webhook-Timestamp"]
            expect = "sha256=" + hmac.new(SECRET.encode(), ts.encode() + b"." + r["raw"],
                                          hashlib.sha256).hexdigest()
            if r["headers"].get("X-Mail-To-Webhook-Signature") != expect:
                failures.append("HMAC signature mismatch")

        # --- 2. per-recipient fan-out -------------------------------------
        received.clear()
        msg2 = EmailMessage()
        msg2["From"] = "bob@sender.test"
        msg2["To"] = "a@example.org, b@example.org"
        msg2["Subject"] = "fanout"
        msg2.set_content("hi")
        with smtplib.SMTP("127.0.0.1", 12525, timeout=10) as s:
            s.send_message(msg2)
        time.sleep(0.6)
        if len(received) != 2:
            failures.append(f"fan-out: expected 2 webhooks, got {len(received)}")
        elif {r["body"]["OriginalRecipient"] for r in received} != {"a@example.org", "b@example.org"}:
            failures.append("fan-out: wrong recipients")

        # --- 3. relay refused for unknown domain --------------------------
        received.clear()
        try:
            with smtplib.SMTP("127.0.0.1", 12525, timeout=10) as s:
                s.sendmail("bob@sender.test", ["nobody@notmine.com"], "Subject: x\n\nx")
            failures.append("relay to foreign domain was accepted")
        except smtplib.SMTPRecipientsRefused as e:
            code = list(e.recipients.values())[0][0]
            if code != 550:
                failures.append(f"relay refusal code = {code}")

        # --- 4. webhook 500 -> SMTP temp failure --------------------------
        mode["status"] = 500
        try:
            with smtplib.SMTP("127.0.0.1", 12525, timeout=20) as s:
                s.sendmail("bob@sender.test", ["a@example.org"], "Subject: x\n\nx")
            failures.append("webhook 500 did not produce an SMTP failure")
        except smtplib.SMTPDataError as e:
            if e.smtp_code != 451:
                failures.append(f"webhook 500 -> SMTP {e.smtp_code}, want 451")

        # --- 5. webhook 400 -> permanent failure --------------------------
        mode["status"] = 400
        try:
            with smtplib.SMTP("127.0.0.1", 12525, timeout=20) as s:
                s.sendmail("bob@sender.test", ["a@example.org"], "Subject: x\n\nx")
            failures.append("webhook 400 did not produce an SMTP failure")
        except smtplib.SMTPDataError as e:
            if e.smtp_code != 550:
                failures.append(f"webhook 400 -> SMTP {e.smtp_code}, want 550")

        # --- 6. the consumer's validation rules ------------------------------
        mode["status"] = 200
        compat_cases = []

        # 6a. Bcc-only: no To header at all.
        received.clear()
        with smtplib.SMTP("127.0.0.1", 12525, timeout=10) as s:
            s.sendmail("sender@out.test", ["hidden@example.org"],
                       "From: Sender <sender@out.test>\r\nSubject: bcc only\r\n\r\nbody\r\n")
        time.sleep(0.5)
        if not received:
            failures.append("6a bcc-only: no webhook")
        else:
            p = received[0]["body"]
            compat_cases.append(("6a bcc-only", p))
            if [e["Email"] for e in p["ToFull"]] != ["hidden@example.org"]:
                failures.append(f"6a ToFull = {p['ToFull']}")

        # 6b. Null sender bounce with an unparseable From.
        received.clear()
        with smtplib.SMTP("127.0.0.1", 12525, timeout=10) as s:
            s.sendmail("", ["a@example.org"],
                       "From: Mailer Daemon\r\nTo: a@example.org\r\nSubject: Undelivered\r\n\r\nbody\r\n")
        time.sleep(0.5)
        if not received:
            failures.append("6b null sender: no webhook")
        else:
            p = received[0]["body"]
            compat_cases.append(("6b null sender", p))
            if p["FromFull"]["Email"] != "mailer-daemon@mx.test":
                failures.append(f"6b FromFull.Email = {p['FromFull']['Email']!r}")
            if p["FromName"] != "Mailer Daemon":
                failures.append(f"6b FromName lost: {p['FromName']!r}")

        # 6c. undisclosed-recipients group syntax.
        received.clear()
        with smtplib.SMTP("127.0.0.1", 12525, timeout=10) as s:
            s.sendmail("sender@out.test", ["a@example.org"],
                       "From: sender@out.test\r\nTo: undisclosed-recipients:;\r\n\r\nbody\r\n")
        time.sleep(0.5)
        if received:
            compat_cases.append(("6c undisclosed", received[0]["body"]))
        else:
            failures.append("6c undisclosed: no webhook")

        # 6d. Attachment edge cases: zero-byte, unnamed, duplicate names,
        #     oversize, and an inline image with a Content-ID.
        received.clear()
        big = "\r\n".join(textwrap.wrap(base64.b64encode(b"Z" * 6000).decode(), 76))
        raw = ("From: sender@out.test\r\nTo: a@example.org\r\nSubject: attachments\r\n"
               "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
               "--B\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n"
               "--B\r\nContent-Type: application/octet-stream\r\n"
               "Content-Disposition: attachment; filename=\"empty.dat\"\r\n\r\n"
               "--B\r\nContent-Type: application/octet-stream\r\n"
               "Content-Disposition: attachment\r\nContent-Transfer-Encoding: base64\r\n\r\nZGF0YQ==\r\n"
               "--B\r\nContent-Type: application/pdf; name=\"report.pdf\"\r\n"
               "Content-Transfer-Encoding: base64\r\n\r\nb25l\r\n"
               "--B\r\nContent-Type: application/pdf; name=\"report.pdf\"\r\n"
               "Content-Transfer-Encoding: base64\r\n\r\ndHdv\r\n"
               "--B\r\nContent-Type: image/png\r\nContent-ID: <logo1>\r\n"
               "Content-Disposition: inline; filename*=utf-8\'\'%2E%2E%2Fcaf%C3%A9.png\r\n"
               "Content-Transfer-Encoding: base64\r\n\r\naW1n\r\n"
               "--B\r\nContent-Type: application/zip; name=\"huge.zip\"\r\n"
               "Content-Transfer-Encoding: base64\r\n\r\n" + big + "\r\n"
               "--B--\r\n")
        with smtplib.SMTP("127.0.0.1", 12525, timeout=10) as s:
            s.sendmail("sender@out.test", ["a@example.org"], raw)
        time.sleep(0.5)
        if not received:
            failures.append("6d attachments: no webhook")
        else:
            p = received[0]["body"]
            compat_cases.append(("6d attachments", p))
            names = [a["Name"] for a in p["Attachments"]]
            want = ["attachment.bin", "report.pdf", "report-2.pdf", "café.png"]
            if names != want:
                failures.append(f"6d attachment names = {names}, want {want}")
            by_name = {a["Name"]: a for a in p["Attachments"]}
            if "report.pdf" in by_name and base64.b64decode(by_name["report.pdf"]["Content"]) != b"one":
                failures.append("6d report.pdf content wrong")
            if "report-2.pdf" in by_name and base64.b64decode(by_name["report-2.pdf"]["Content"]) != b"two":
                failures.append("6d duplicate attachment collided")
            if "café.png" in by_name and by_name["café.png"].get("ContentID") != "logo1":
                failures.append("6d inline ContentID lost")
            for a in p["Attachments"]:
                if any(c in a["Name"] for c in "/\\"):
                    failures.append(f"6d path separator in name {a['Name']!r}")

        # Every payload must satisfy the consumer's rules.
        for label, p in compat_cases:
            errs = validate_payload(p)
            if errs:
                failures.append(f"{label} would 422: {errs}")

        # --- 6e. an over-long line is permanent, not a retry loop -----------
        try:
            with smtplib.SMTP("127.0.0.1", 12525, timeout=10) as s:
                s.sendmail("sender@out.test", ["a@example.org"],
                           "From: sender@out.test\r\nTo: a@example.org\r\n\r\n" + "Z" * 5000 + "\r\n")
            failures.append("6e over-long line was accepted")
        except smtplib.SMTPResponseException as e:
            if e.smtp_code // 100 != 5:
                failures.append(f"6e over-long line -> {e.smtp_code}, want a 5xx (never retryable)")

        # --- 7. auth failures must NOT bounce the message -------------------
        mode["status"] = 401   # a rejected credential: a deployment mistake
        try:
            with smtplib.SMTP("127.0.0.1", 12525, timeout=30) as s:
                s.sendmail("bob@sender.test", ["a@example.org"], "Subject: x\n\nx")
            failures.append("401 did not produce an SMTP failure")
        except smtplib.SMTPDataError as e:
            if e.smtp_code != 451:
                failures.append(f"401 -> SMTP {e.smtp_code}, want 451 (retry, not bounce)")

        mode["status"] = 422   # a validation rejection
        try:
            with smtplib.SMTP("127.0.0.1", 12525, timeout=30) as s:
                s.sendmail("bob@sender.test", ["a@example.org"], "Subject: x\n\nx")
            failures.append("422 did not produce an SMTP failure")
        except smtplib.SMTPDataError as e:
            if e.smtp_code != 550:
                failures.append(f"422 -> SMTP {e.smtp_code}, want 550")

        # --- 8. one recipient fails, the other is not delivered twice ---------
        mode["status"] = 200
        mode["status_for"] = {"second@example.org": 503}
        received.clear()
        partial = ("From: sender@out.test\r\nTo: first@example.org, second@example.org\r\n"
                   "Subject: partial\r\nMessage-ID: <partial@out.test>\r\n\r\nbody\r\n")
        try:
            with smtplib.SMTP("127.0.0.1", 12525, timeout=30) as s:
                s.sendmail("sender@out.test", ["first@example.org", "second@example.org"], partial)
            failures.append("8 partial failure was acknowledged")
        except smtplib.SMTPDataError as e:
            if e.smtp_code != 451:
                failures.append(f"8 partial failure -> SMTP {e.smtp_code}, want 451")
        mode["status_for"] = {}
        with smtplib.SMTP("127.0.0.1", 12525, timeout=30) as s:
            s.sendmail("sender@out.test", ["first@example.org", "second@example.org"], partial)
        time.sleep(0.5)
        to_first = [r for r in received if r["body"]["OriginalRecipient"] == "first@example.org"]
        to_second = [r for r in received if r["body"]["OriginalRecipient"] == "second@example.org"]
        if len(to_first) != 1:
            failures.append(f"8 the recipient that already had the mail got it {len(to_first)} times")
        if not to_second or to_second[-1]["headers"].get("X-Mail-To-Webhook-Delivery") != to_second[0]["headers"].get("X-Mail-To-Webhook-Delivery"):
            failures.append("8 the retried delivery did not keep its id")
    finally:
        stop(proc)


def stop(proc):
    proc.terminate()
    try:
        out, _ = proc.communicate(timeout=10)
        print("--- server log tail ---")
        print("\n".join(out.strip().splitlines()[-4:]))
    except subprocess.TimeoutExpired:
        proc.kill()


def http(method, url, headers=None, data=None):
    req = urllib.request.Request(url, method=method, headers=headers or {}, data=data)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, lower(resp.headers), resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, lower(e.headers), e.read().decode("utf-8", "replace")


def lower(headers):
    """Header names are case-insensitive; the server writes them its own way."""
    return {k.lower(): v for k, v in headers.items()}


def rcpt_codes(sender, recipients):
    """The reply code of each RCPT TO in one session."""
    codes = []
    s = smtplib.SMTP("127.0.0.1", 12526, timeout=10)
    try:
        s.ehlo()
        s.mail(sender)
        for r in recipients:
            try:
                codes.append(s.rcpt(r)[0])
            except smtplib.SMTPServerDisconnected:
                codes.append(None)
                break
    finally:
        try:
            s.quit()
        except Exception:
            pass
    return codes


def strict(binary, failures):
    """The defaults, in front of a strict consumer: one POST per message, basic
    auth, the recipient check, sender retries, the history and the UI."""
    data_dir = tempfile.mkdtemp(prefix="m2w-e2e-")
    box, second = "AbC123.purchase@example.org", "Second.sales@example.org"
    known.update({box, second})
    mode.update(status=200, status_for={})
    received.clear()
    lookups.clear()
    ui = "http://127.0.0.1:9097"

    env = dict(os.environ,
               SMTP_ADDR="127.0.0.1:12526",
               SMTP_DOMAIN="mx.test",
               WEBHOOK_URL="http://hook:t0ken@127.0.0.1:9099/inbound",
               WEBHOOK_SECRET=SECRET,
               WEBHOOK_RETRIES="0",
               WEBHOOK_SWEEP_SECONDS="1",
               RECIPIENT_CHECK_URL="http://127.0.0.1:9099/recipients",
               RECIPIENT_UNKNOWN_TTL="2s",
               ALLOWED_DOMAINS="example.org",
               MAX_BAD_RECIPIENTS="3",
               BLOCK_AFTER_BAD_RECIPIENTS="0",
               MAX_CONNECTIONS_PER_IP_PER_MINUTE="0",
               HTTP_ADDR="127.0.0.1:9097",
               DATA_DIR=data_dir)
    proc = subprocess.Popen([binary], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    try:
        assert wait_port(12526), "smtp did not start"

        def send(sender, recipients, raw, port=12526):
            with smtplib.SMTP("127.0.0.1", port, timeout=30) as s:
                s.sendmail(sender, recipients, raw)
            time.sleep(0.4)

        # --- S1. credentials, headers, one POST for two recipients ---------
        send("sender@out.test", [box, second],
             f"From: Sender <sender@out.test>\r\nTo: {box}\r\nCc: {second}\r\n"
             "Subject: two recipients\r\nMessage-ID: <s1@out.test>\r\n"
             "Date: Sun, 16 Aug 2026 09:20:00 +0200\r\n\r\nbody\r\n")
        if len(received) != 1:
            failures.append(f"S1 expected one webhook for the message, got {len(received)}")
        else:
            r = received[0]
            h, p = r["headers"], r["body"]
            want_auth = "Basic " + base64.b64encode(b"hook:t0ken").decode()
            if h.get("Authorization") != want_auth:
                failures.append(f"S1 Authorization = {h.get('Authorization')!r}")
            if h.get("X-Mail-To-Webhook-Event") != "message.received":
                failures.append(f"S1 event header = {h.get('X-Mail-To-Webhook-Event')!r}")
            if not h.get("X-Mail-To-Webhook-Delivery", "").startswith("whd_"):
                failures.append(f"S1 delivery header = {h.get('X-Mail-To-Webhook-Delivery')!r}")
            if h.get("User-Agent") != "mail-to-webhook/1.0":
                failures.append(f"S1 user agent = {h.get('User-Agent')!r}")
            ts = h.get("X-Mail-To-Webhook-Timestamp", "")
            expect = "sha256=" + hmac.new(SECRET.encode(), ts.encode() + b"." + r["raw"], hashlib.sha256).hexdigest()
            if h.get("X-Mail-To-Webhook-Signature") != expect:
                failures.append("S1 signature mismatch")
            if [e["Email"] for e in p["ToFull"]] != [box] or [e["Email"] for e in p["CcFull"]] != [second]:
                failures.append(f"S1 ToFull/CcFull = {p['ToFull']} / {p['CcFull']}")
            if p["Date"] != "Sun, 16 Aug 2026 09:20:00 +0200":
                failures.append(f"S1 Date = {p['Date']!r}")
            if "t0ken" in r["raw"].decode():
                failures.append("S1 the password is in the payload")

        # --- S2. the envelope recipient is not in To: forwarded mail ----------
        received.clear()
        send("sender@out.test", [box],
             "From: sender@out.test\r\nTo: invoices@customer.test\r\nSubject: forwarded\r\n\r\nbody\r\n")
        if len(received) != 1:
            failures.append(f"S2 expected one webhook, got {len(received)}")
        else:
            p = received[0]["body"]
            if [e["Email"] for e in p["ToFull"]] != ["invoices@customer.test", box]:
                failures.append(f"S2 ToFull = {p['ToFull']}")
            ids = [h["Value"] for h in p["Headers"] if h["Name"].lower() == "message-id"]
            if len(ids) != 1 or not ids[0].startswith("<m2w.") or p["MessageID"] != ids[0].strip("<>"):
                failures.append(f"S2 Message-ID headers = {ids}, MessageID = {p['MessageID']!r}")
            try:
                email.utils.parsedate_to_datetime(p["Date"])
            except Exception:
                failures.append(f"S2 Date is not readable: {p['Date']!r}")

        # --- S3. html only, an attached mail, an odd date ---------------------
        received.clear()
        inner = ("From: supplier@shop.test\r\nSubject: Factuur 42\r\nContent-Type: multipart/mixed; boundary=IN\r\n\r\n"
                 "--IN\r\nContent-Type: text/plain\r\n\r\ninner\r\n"
                 "--IN\r\nContent-Type: application/pdf; name=\"factuur.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n"
                 "--IN--\r\n")
        send("sender@out.test", [box],
             f"From: sender@out.test\r\nTo: {box}\r\nSubject: fwd\r\nDate: last tuesday\r\n"
             "Content-Type: multipart/mixed; boundary=OUT\r\n\r\n"
             "--OUT\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Zie <b>bijlage</b></p>\r\n"
             "--OUT\r\nContent-Type: message/rfc822\r\n\r\n" + inner + "\r\n--OUT--\r\n")
        if len(received) != 1:
            failures.append(f"S3 expected one webhook, got {len(received)}")
        else:
            p = received[0]["body"]
            names = [a["Name"] for a in p["Attachments"]]
            if names != ["Factuur 42.eml", "factuur.pdf"]:
                failures.append(f"S3 attachment names = {names}")
            if "bijlage" not in p["TextBody"] or "<b>" in p["TextBody"]:
                failures.append(f"S3 TextBody = {p['TextBody']!r}")
            try:
                email.utils.parsedate_to_datetime(p["Date"])
            except Exception:
                failures.append(f"S3 Date is not readable: {p['Date']!r}")

        # Every payload so far must satisfy the consumer's rules.
        for r in received:
            errs = validate_payload(r["body"])
            if errs:
                failures.append(f"S3 would 422: {errs}")

        # --- S4. the recipient check -----------------------------------------
        received.clear()
        asked = lambda a: sum(1 for l in lookups if a in l["addresses"])
        if asked(box) != 1:
            failures.append(f"S4 the known address was looked up {asked(box)} times in three mails, want 1")
        first = lookups[0]["headers"]
        if first.get("Authorization") != "Basic " + base64.b64encode(b"hook:t0ken").decode() \
                or first.get("X-Mail-To-Webhook-Event") != "recipient.check":
            failures.append(f"S4 the lookup's headers = {first}")

        codes = rcpt_codes("spam@out.test", ["nobody@example.org"])
        if codes != [550]:
            failures.append(f"S4 unknown recipient -> {codes}, want [550]")
        rcpt_codes("spam@out.test", ["nobody@example.org"])
        if asked("nobody@example.org") != 1:
            failures.append(f"S4 a repeated unknown address was looked up {asked('nobody@example.org')} times")
        if received:
            failures.append("S4 a refused recipient reached the webhook")

        # The domain is lower-cased, the local part is left alone.
        if rcpt_codes("a@out.test", ["AbC123.purchase@EXAMPLE.ORG"]) != [250]:
            failures.append("S4 an upper-case domain was refused")
        if rcpt_codes("a@out.test", ["abc123.purchase@example.org"]) != [550]:
            failures.append("S4 another case of the local part was accepted")
        if asked("AbC123.purchase@EXAMPLE.ORG") != 0:
            failures.append("S4 the lookup was made with the domain as the sender wrote it")

        # A mailbox created after a refusal works once the answer has expired.
        known.add("new@example.org")
        if rcpt_codes("a@out.test", ["new@example.org"]) != [250]:
            failures.append("S4 a new address was refused")
        known.discard("nobody@example.org")
        rcpt_codes("a@out.test", ["late@example.org"])
        known.add("late@example.org")
        time.sleep(2.2)
        if rcpt_codes("a@out.test", ["late@example.org"]) != [250]:
            failures.append("S4 an address created after a refusal stayed refused past the TTL")

        # Outside the allowed domains nothing is looked up at all.
        before = len(lookups)
        if rcpt_codes("a@out.test", ["x@elsewhere.test"]) != [550] or len(lookups) != before:
            failures.append("S4 a foreign domain was looked up or accepted")

        # The fourth unknown recipient on one connection ends it.
        codes = rcpt_codes("spam@out.test", [f"guess{i}@example.org" for i in range(1, 6)])
        if codes[:4] != [550, 550, 550, 421]:
            failures.append(f"S4 guessing run -> {codes}, want 550, 550, 550, 421")

        # The consumer cannot be asked: a new address waits, a known one is fine.
        check["status"] = 503
        if rcpt_codes("a@out.test", ["unseen@example.org"]) != [451]:
            failures.append("S4 a new address during an outage was not deferred")
        if rcpt_codes("a@out.test", [box]) != [250]:
            failures.append("S4 a known address was refused during an outage")
        check["status"] = 200

        # --- S5. a sender's retry is the same delivery ------------------------
        received.clear()
        retry = f"From: sender@out.test\r\nTo: {box}\r\nSubject: retry me\r\n\r\nbody\r\n"
        mode["status"] = 503
        try:
            send("sender@out.test", [box], retry)
            failures.append("S5 a 503 was acknowledged")
        except smtplib.SMTPDataError as e:
            if e.smtp_code != 451:
                failures.append(f"S5 503 -> SMTP {e.smtp_code}, want 451")
        mode["status"] = 200
        send("sender@out.test", [box], retry)
        send("sender@out.test", [box], retry)   # the sender never saw our 250
        if len(received) != 2:
            failures.append(f"S5 expected two POSTs (one failed, one accepted), got {len(received)}")
        else:
            a, b = received
            if a["headers"]["X-Mail-To-Webhook-Delivery"] != b["headers"]["X-Mail-To-Webhook-Delivery"]:
                failures.append("S5 the retry got a new delivery id")
            if a["raw"] != b["raw"]:
                failures.append("S5 the retry's payload differs from the first attempt's")

        # --- S6. the operator UI ----------------------------------------------
        if os.environ.get("E2E_SKIP_UI"):
            return
        status, _, body = http("GET", ui + "/healthz")
        if status != 200 or '"ok"' not in body:
            failures.append(f"S6 /healthz -> {status} {body}")
        status, _, page = http("GET", ui + "/ui/messages")
        if status != 200 or "retry me" not in page or "two recipients" not in page:
            failures.append(f"S6 /ui/messages -> {status}, subjects missing")
        if "t0ken" in page or SECRET in page:
            failures.append("S6 a credential is on the messages page")
        status, _, refused = http("GET", ui + "/ui/refused")
        if status != 200 or "nobody@example.org" not in refused:
            failures.append(f"S6 /ui/refused -> {status}, the refused address is missing")
        status, _, addresses = http("GET", ui + "/ui/addresses")
        if status != 200 or box not in addresses:
            failures.append(f"S6 /ui/addresses -> {status}, the known address is missing")

        ids = re.findall(r"/ui/messages/(msg_[0-9A-Z]+)", page)
        status, _, table = http("GET", ui + "/ui/messages?subject=retry+me")
        target = re.findall(r"/ui/messages/(msg_[0-9A-Z]+)", table)
        if not ids or len(set(target)) != 1:
            failures.append(f"S6 could not find the message in the list ({len(ids)} ids, {len(set(target))} matches)")
            return
        message = target[0]
        status, _, detail = http("GET", f"{ui}/ui/messages/{message}")
        if status != 200 or "Webhook deliveries" not in detail or "Resend webhook" not in detail:
            failures.append(f"S6 detail page -> {status}")

        # Resend: a new delivery id, the same bytes.
        status, _, _ = http("POST", f"{ui}/ui/api/messages/{message}/webhook/resend")
        if status != 403:
            failures.append(f"S6 a cross-site POST was not refused ({status})")
        received_before = len(received)
        status, headers, answer = http("POST", f"{ui}/ui/api/messages/{message}/webhook/resend", {"HX-Request": "true"})
        if status != 200 or headers.get("hx-trigger") != "message-changed":
            failures.append(f"S6 resend -> {status} {answer} trigger={headers.get('hx-trigger')!r}")
        deadline = time.time() + 10
        while len(received) == received_before and time.time() < deadline:
            time.sleep(0.2)
        if len(received) != received_before + 1:
            failures.append("S6 the resend never reached the webhook")
        else:
            again, original = received[-1], received[-2]
            if again["headers"]["X-Mail-To-Webhook-Delivery"] == original["headers"]["X-Mail-To-Webhook-Delivery"]:
                failures.append("S6 the resend reused the delivery id")
            if again["raw"] != original["raw"]:
                failures.append("S6 the resend's payload differs from the original")
    finally:
        stop(proc)


def main():
    binary = sys.argv[1]
    httpd = HTTPServer(("127.0.0.1", 9099), Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()

    failures = []
    classic(binary, failures)
    strict(binary, failures)

    if failures:
        print("FAIL")
        for f in failures:
            print("  -", f)
        return 1
    print("E2E PASS: all scenarios")
    return 0


if __name__ == "__main__":
    sys.exit(main())
