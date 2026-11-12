# The stylesheet is built first; only its output reaches the later stages,
# and the runtime image has neither Node nor a Go toolchain.
FROM node:22-alpine AS css
WORKDIR /build
COPY package.json package-lock.json ./
RUN npm ci
COPY internal/ui/styles ./internal/ui/styles
COPY internal/ui/templates ./internal/ui/templates
COPY internal/ui/static ./internal/ui/static
RUN npm run css

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=css /build/internal/ui/static/app.css ./internal/ui/static/app.css
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/mail-to-webhook ./cmd/mail-to-webhook \
 && mkdir -p /out/data

# Static binary, CA certificates, an unprivileged user and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mail-to-webhook /usr/local/bin/mail-to-webhook
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV DATA_DIR=/data SMTP_ADDR=:2525 HTTP_ADDR=:8080
VOLUME ["/data"]
# 2525 is SMTP: publish it as 25. 8080 is health and the operator UI: keep it
# private. 8089 answers the CA's validation request when ACME_ENABLED is set:
# publish it as 80.
EXPOSE 2525 8080 8089
USER nonroot
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/usr/local/bin/mail-to-webhook", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/mail-to-webhook"]
