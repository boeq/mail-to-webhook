# Contributing

## What you need

- Docker, or Go 1.27. `scripts/go.sh` uses a local `go` when there is one and the official image otherwise.
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

`make check` also runs on GitHub Actions for every push and pull request. From `main` and from `v*` tags the same workflow publishes the container image.

To run Docker Compose with an image built from your checkout, copy `docker-compose.override.example.yml` to `docker-compose.override.yml` and run `docker compose up --build`.

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

## Rules of the house

- A parse failure never loses mail. Anything `mailparse` cannot read is still delivered. A new mail type gets a test in `internal/mailparse`.
- A payload change gets a compatibility test. `internal/payload` tests build a payload and run it through `internal/consumer`. If you change the rules there, change the Python copy in `scripts/e2e_test.py` as well. The two are written separately on purpose, so one mistake cannot hide behind the same mistake.
- History never blocks mail. A failing database is logged and mail goes on without a record.
- No secret reaches a log, a page or an error message.
- Settings stay documented. A test fails when a variable is missing from `.env.example` or `docs/configuration.md`.

## The README images

`make banners` renders the header and footer from `.github/assets/banner.html` with a headless Chromium (`pip install playwright && playwright install chromium`).
