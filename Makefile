# `make check` is what every change must pass.
#
# Go runs through scripts/go.sh: a local toolchain when there is one, the
# official image otherwise, so nothing has to be installed to build or test.

GO      := scripts/go.sh
BIN     := bin/mail-to-webhook
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build css test vet node-check e2e acme-e2e check run docker clean

# The stylesheet: Tailwind v4 builds internal/ui/styles/index.css into
# internal/ui/static/app.css, which git ignores and the binary embeds.
css:
	@test -d node_modules || npm ci
	npm run css

build: css
	$(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/mail-to-webhook

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...
	@out="$$($(GO) fmt ./...)"; if [ -n "$$out" ]; then echo "gofmt rewrote:"; echo "$$out"; exit 1; fi

# Every page-side script except the vendored, minified htmx.
node-check:
	@for f in $$(ls internal/ui/static/*.js | grep -v '/htmx\.min\.js$$'); do echo "node --check $$f"; node --check "$$f" || exit 1; done

# Real SMTP client, real binary, real webhook receiver.
e2e: build
	python3 scripts/e2e_test.py $(BIN)

# Orders a real certificate from Pebble, the test CA Let's Encrypt publishes,
# and checks STARTTLS with it. Needs Docker and the host network.
acme-e2e: build
	python3 scripts/acme_e2e.py $(BIN)

check: vet test node-check e2e

# A local instance: SMTP on 127.0.0.1:2525, UI on http://127.0.0.1:8080/ui/,
# data in ./.data. Without a WEBHOOK_URL of your own it posts to its built-in
# test receiver.
run: build
	@set -a; [ -f .env ] && . ./.env; set +a; \
	DATA_DIR="./.data" SMTP_ADDR="$${SMTP_ADDR:-127.0.0.1:2525}" HTTP_ADDR="$${HTTP_ADDR:-127.0.0.1:8080}" \
	WEBHOOK_URL="$${WEBHOOK_URL:-http://127.0.0.1:8080/ui/api/webhook-sink}" $(BIN)

docker:
	docker build --build-arg VERSION=$(VERSION) -t mail-to-webhook:local .

clean:
	rm -f $(BIN)
