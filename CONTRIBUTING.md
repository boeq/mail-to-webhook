# Contributing

## What is necessary

- Docker, or Go 1.27. `scripts/go.sh` uses a local `go` if there is one. If not, it uses the official image.
- Node 22, for the stylesheet.
- Python 3, for the end-to-end test.

## The commands

```bash
make check      # vet, unit tests, script syntax, end-to-end. Run it before every commit.
make test       # unit tests only
make e2e        # real SMTP client, real binary, real webhook receiver
make run        # a local instance on 127.0.0.1:2525, UI on http://127.0.0.1:8080/ui/
make css        # rebuild the stylesheet after changing a template or a style
make acme-e2e   # order a certificate from Pebble, Let's Encrypt's test CA (needs Docker)
```

GitHub Actions also runs `make check` for each push and each pull request. From `main` and from `v*` tags, the same workflow publishes the container image.

To use Docker Compose with an image from your checkout:

1. Copy `docker-compose.override.example.yml` to `docker-compose.override.yml`.
2. Use the command `docker compose up --build`.

## Layout

```
cmd/mail-to-webhook/   entry point and wiring
internal/config/       environment variables
internal/smtpsrv/      the SMTP conversation and its reply codes
internal/recipients/   the known-address list and the lookup
internal/limits/       connection and abuse limits
internal/tlscert/      the STARTTLS certificate: from files, or through ACME
internal/mailparse/    MIME parsing
internal/payload/      the JSON document and its normalisations
internal/consumer/     the validation rules of a strict consumer
internal/webhook/      signing, one HTTP attempt, reading the answer
internal/delivery/     attempts, retries, resends
internal/store/        SQLite history and the raw-message spool
internal/ui/           the operator UI
scripts/               end-to-end test, banner rendering
docs/                  user documentation
```

## Rules

- A parse failure must not cause the loss of a message.
  - The service delivers all data that `mailparse` cannot read.
  - Add a test in `internal/mailparse` for each new message type.
- A change to the payload must have a compatibility test.
  - The tests in `internal/payload` build a payload and examine it with `internal/consumer`.
  - If you change the rules in `internal/consumer`, also change the Python copy in `scripts/e2e_test.py`.
  - The two copies are separate by design. One error then cannot hide behind the same error in the other copy.
- The history must not stop a message. If the database fails, the service writes the error to the log and continues without a record.
- Do not write a secret to a log, a page or an error message.
- Keep the settings in the documentation. A test fails if a variable is not in `.env.example` or in `docs/configuration.md`.

## The README images

`make banners` makes the header image and the footer image from `.github/assets/banner.html`. It uses a headless Chromium. To install one, use `pip install playwright && playwright install chromium`.
