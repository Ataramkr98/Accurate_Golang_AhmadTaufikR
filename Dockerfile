# ---- Build stage ----
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache dependency downloads separately from the source copy.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary so the runtime image needs no libc.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tera-server ./cmd/server

# ---- Runtime stage ----
FROM alpine:3.20

# ca-certificates: required for TLS connections to Neon / any managed Postgres.
# tzdata: required for correct Asia/Jakarta date handling in reports.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 tera

ENV TZ=Asia/Jakarta

WORKDIR /app
COPY --from=build /out/tera-server /app/tera-server

USER tera
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/tera-server"]
