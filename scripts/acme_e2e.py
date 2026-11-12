#!/usr/bin/env python3
"""Order a real certificate over ACME and use it for STARTTLS.

The CA is Pebble, the test server Let's Encrypt publishes, together with its
mock DNS server. Both run with Docker on the host network. Pebble validates
the http-01 challenge for real: it resolves the hostname (to 127.0.0.1) and
fetches the token over HTTP.

Two setups are tested:

  direct   the service's challenge listener is what the CA reaches
  proxied  a Caddy web server holds the port the CA reaches, manages
           certificates of its own, and forwards only the challenge path

Usage: python3 scripts/acme_e2e.py /path/to/mail-to-webhook
"""
import json
import os
import re
import smtplib
import socket
import ssl
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HOST = "mx.example.org"
PEBBLE, DNS, CADDY = "m2w-acme-pebble", "m2w-acme-dns", "m2w-acme-caddy"
SMTP_PORT, HTTP_PORT = 12560, 9160
# Pebble fetches http-01 tokens from this port, as a real CA does from port 80.
VALIDATION_PORT = 5002
# Where the service listens when a web server holds the validation port.
BEHIND_PROXY_PORT = 5003

CADDYFILE = f"""{{
	admin off
	http_port {VALIDATION_PORT}
	https_port 5005
	acme_ca https://localhost:14000/dir
	acme_ca_root /pebble-api-ca.pem
	skip_install_trust
}}

# A site of the web server's own, so that its ACME machinery is active on the
# same port the forwarded challenge arrives on.
portal.example.org {{
	respond "portal"
}}

http://{HOST} {{
	reverse_proxy /.well-known/acme-challenge/* 127.0.0.1:{BEHIND_PROXY_PORT}
}}
"""

failures = []


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + (f": {detail}" if detail and not ok else ""))
    if not ok:
        failures.append(name)


def docker(*args, check=True):
    return subprocess.run(["docker", *args], check=check, capture_output=True, text=True).stdout.strip()


def wait_for(what, probe, timeout=60):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            result = probe()
            if result:
                return result
        except Exception as e:  # not up yet
            last = e
        time.sleep(0.3)
    raise SystemExit(f"timed out waiting for {what}: {last}")


def start_ca(workdir):
    docker("rm", "-f", PEBBLE, DNS, CADDY, check=False)
    # The mock DNS answers every name with 127.0.0.1. Its own challenge
    # servers are switched off: the service under test answers the challenge.
    docker("run", "-d", "--name", DNS, "--network", "host",
           "ghcr.io/letsencrypt/pebble-challtestsrv:latest",
           "-defaultIPv4", "127.0.0.1", "-defaultIPv6", "",
           "-dnsserver", ":8053", "-management", ":8055",
           "-http01", "", "-https01", "", "-tlsalpn01", "", "-doh", "")
    docker("run", "-d", "--name", PEBBLE, "--network", "host",
           "-e", "PEBBLE_VA_NOSLEEP=1", "-e", "PEBBLE_WFE_NONCEREJECT=0",
           "ghcr.io/letsencrypt/pebble:latest",
           "-config", "/test/config/pebble-config.json", "-dnsserver", "127.0.0.1:8053")
    # Pebble's own HTTPS endpoints are signed by a CA shipped in the image.
    api_ca = os.path.join(workdir, "pebble-api-ca.pem")
    docker("cp", f"{PEBBLE}:/test/certs/pebble.minica.pem", api_ca)
    os.chmod(api_ca, 0o644)
    api = ssl.create_default_context(cafile=api_ca)
    wait_for("the ACME directory", lambda: urllib.request.urlopen("https://localhost:14000/dir", context=api, timeout=2).status == 200)
    # The root Pebble signs issued certificates with is made up at each start.
    issuing_root = urllib.request.urlopen("https://localhost:15000/roots/0", context=api, timeout=5).read().decode()
    return api_ca, issuing_root


def start_caddy(workdir, api_ca):
    caddyfile = os.path.join(workdir, "Caddyfile")
    with open(caddyfile, "w") as f:
        f.write(CADDYFILE)
    docker("run", "-d", "--name", CADDY, "--network", "host",
           "-v", f"{caddyfile}:/etc/caddy/Caddyfile:ro", "-v", f"{api_ca}:/pebble-api-ca.pem:ro", "caddy:2")
    wait_for("the web server", lambda: socket.create_connection(("127.0.0.1", VALIDATION_PORT), 0.5).close() or True)


def start_service(binary, data_dir, api_ca, log_path, challenge_port):
    env = dict(os.environ,
               SMTP_ADDR=f"127.0.0.1:{SMTP_PORT}",
               HTTP_ADDR=f"127.0.0.1:{HTTP_PORT}",
               SMTP_DOMAIN=HOST,
               ALLOWED_DOMAINS="example.org",
               WEBHOOK_URL=f"http://127.0.0.1:{HTTP_PORT}/ui/api/webhook-sink",
               ACME_ENABLED="true",
               ACME_EMAIL="ops@example.org",
               ACME_DIRECTORY_URL="https://localhost:14000/dir",
               ACME_HTTP_ADDR=f"127.0.0.1:{challenge_port}",
               SSL_CERT_FILE=api_ca,
               MAX_CONNECTIONS_PER_IP_PER_MINUTE="0",
               DATA_DIR=data_dir)
    log = open(log_path, "w")
    proc = subprocess.Popen([binary], env=env, stdout=log, stderr=subprocess.STDOUT)
    wait_for("the SMTP port", lambda: socket.create_connection(("127.0.0.1", SMTP_PORT), 0.5).close() or True)
    return proc


def stop(proc):
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def health():
    return json.load(urllib.request.urlopen(f"http://127.0.0.1:{HTTP_PORT}/healthz", timeout=2))


def http_status(url):
    try:
        return urllib.request.urlopen(url, timeout=5).status
    except urllib.error.HTTPError as e:
        return e.code


def starttls_certificate(issuing_root):
    """Upgrade one SMTP connection and return the certificate the server presents.

    The handshake verifies the chain up to the CA's root and the hostname.
    """
    ctx = ssl.create_default_context(cadata=issuing_root)
    sock = socket.create_connection(("127.0.0.1", SMTP_PORT), 10)
    reader = sock.makefile("rb")

    def reply():
        lines = []
        while True:
            line = reader.readline().decode().rstrip()
            lines.append(line)
            if len(line) < 4 or line[3] != "-":
                return lines

    reply()
    sock.sendall(b"EHLO client.test\r\n")
    extensions = reply()
    if not any("STARTTLS" in line for line in extensions):
        raise AssertionError(f"STARTTLS is not offered: {extensions}")
    sock.sendall(b"STARTTLS\r\n")
    reply()
    with ctx.wrap_socket(sock, server_hostname=HOST) as tls:
        return tls.getpeercert(), tls.version()


def direct(binary, workdir, api_ca, issuing_root):
    """The CA reaches the service's own challenge listener."""
    print("--- direct ---")
    data_dir = os.path.join(workdir, "direct")
    proc = start_service(binary, data_dir, api_ca, os.path.join(workdir, "direct-1.log"), VALIDATION_PORT)
    try:
        state = wait_for("the certificate", lambda: health().get("starttls", {}).get("ready") and health())
        check("health reports the certificate", state["starttls"]["mode"] == "acme" and "expires" in state["starttls"], state)

        cert, version = starttls_certificate(issuing_root)
        names = [value for kind, value in cert.get("subjectAltName", ()) if kind == "DNS"]
        check("the handshake verifies against the CA's root and the hostname", names == [HOST], names)
        check("TLS 1.2 or later", version in ("TLSv1.2", "TLSv1.3"), version)
        serial = cert["serialNumber"]

        unverified = ssl.create_default_context()
        unverified.check_hostname, unverified.verify_mode = False, ssl.CERT_NONE
        with smtplib.SMTP("127.0.0.1", SMTP_PORT, timeout=15) as s:
            s.starttls(context=unverified)
            s.sendmail("a@sender.test", ["box@example.org"],
                       "From: a@sender.test\r\nTo: box@example.org\r\nSubject: over acme tls\r\n\r\nencrypted\r\n")
        page = urllib.request.urlopen(f"http://127.0.0.1:{HTTP_PORT}/ui/messages?subject=over+acme+tls", timeout=5).read().decode()
        message = re.findall(r"/ui/messages/(msg_[0-9A-Z]+)", page)[0]
        detail = urllib.request.urlopen(f"http://127.0.0.1:{HTTP_PORT}/ui/messages/{message}", timeout=5).read().decode()
        check("a mail sent over it is recorded as encrypted", bool(re.search(r"<dt>TLS</dt>\s*<dd[^>]*>\s*yes", detail)))

        status = urllib.request.urlopen(f"http://127.0.0.1:{HTTP_PORT}/ui/api/shell/status", timeout=5).read().decode()
        check("the UI status row says STARTTLS is on", bool(re.search(r"STARTTLS</span>\s*<span[^>]*>on<", status)), status[-400:])

        for path in ("/", "/.well-known/acme-challenge/not-a-token", "/ui/"):
            code = http_status(f"http://127.0.0.1:{VALIDATION_PORT}{path}")
            check(f"the challenge listener answers 404 for {path}", code == 404, str(code))

        stored = []
        for root, _, files in os.walk(os.path.join(data_dir, "acme")):
            stored += [os.path.join(root, f) for f in files]
        check("certificate, key and account key are stored", sorted(os.path.basename(f) for f in stored) == ["account.key", "cert.pem", "key.pem"], str(stored))
        check("they are readable by the owner only", all(stat.S_IMODE(os.stat(f).st_mode) == 0o600 for f in stored))

        # A restart uses the stored certificate and orders nothing.
        stop(proc)
        second_log = os.path.join(workdir, "direct-2.log")
        proc = start_service(binary, data_dir, api_ca, second_log, VALIDATION_PORT)
        wait_for("the certificate after a restart", lambda: health().get("starttls", {}).get("ready"), timeout=15)
        again, _ = starttls_certificate(issuing_root)
        check("after a restart the same certificate is served", again["serialNumber"] == serial)
        log = open(second_log).read()
        check("and no new one was ordered", "certificate loaded" in log and "certificate obtained" not in log, log[-600:])
    finally:
        stop(proc)


def proxied(binary, workdir, api_ca, issuing_root):
    """A web server holds the port the CA reaches and forwards the challenge."""
    print("--- behind a web server ---")
    start_caddy(workdir, api_ca)
    proc = start_service(binary, os.path.join(workdir, "proxied"), api_ca, os.path.join(workdir, "proxied.log"), BEHIND_PROXY_PORT)
    try:
        wait_for("the certificate", lambda: health().get("starttls", {}).get("ready"))
        cert, _ = starttls_certificate(issuing_root)
        names = [value for kind, value in cert.get("subjectAltName", ()) if kind == "DNS"]
        check("a certificate was issued through the web server", names == [HOST], names)

        # The web server kept its own business: it got a certificate for its
        # own site from the same CA, over the same port.
        def own_site():
            ctx = ssl.create_default_context(cadata=issuing_root)
            with socket.create_connection(("127.0.0.1", 5005), 5) as raw, ctx.wrap_socket(raw, server_hostname="portal.example.org") as tls:
                return tls.getpeercert()
        own = wait_for("the web server's own certificate", own_site, timeout=90)
        own_names = [value for kind, value in own.get("subjectAltName", ()) if kind == "DNS"]
        check("the web server still obtained its own certificate", own_names == ["portal.example.org"], own_names)

        # Only the challenge path is forwarded.
        req = urllib.request.Request(f"http://127.0.0.1:{VALIDATION_PORT}/ui/", headers={"Host": HOST})
        try:
            body = urllib.request.urlopen(req, timeout=5).read().decode()
        except urllib.error.HTTPError as e:
            body = e.read().decode()
        check("the operator UI is not reachable through the web server", "Mail to Webhook" not in body)
    finally:
        stop(proc)


def main():
    binary = sys.argv[1]
    workdir = tempfile.mkdtemp(prefix="m2w-acme-")
    os.chmod(workdir, 0o755)
    try:
        api_ca, issuing_root = start_ca(workdir)
        direct(binary, workdir, api_ca, issuing_root)
        proxied(binary, workdir, api_ca, issuing_root)
    finally:
        docker("rm", "-f", PEBBLE, DNS, CADDY, check=False)

    if failures:
        print(f"\nACME E2E FAIL: {len(failures)} failed")
        return 1
    print("\nACME E2E PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
