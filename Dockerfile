# ── Stage 1: builder ─────────────────────────────────────────────────────────
FROM golang:1.26.1-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -ldflags="-w -s" \
    -o tracker \
    ./cmd/tracker/main.go

# ── Stage 2: runner PRODUCCIÓN ────────────────────────────────────────────────
FROM scratch AS runner

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /app/tracker /tracker

EXPOSE 8081

# La imagen scratch no tiene shell ni wget: el propio binario hace el healthcheck.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/tracker", "healthcheck"]

ENTRYPOINT ["/tracker"]