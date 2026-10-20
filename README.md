# Tera — a double-entry accounting engine in Go

A production-shaped accounting backend: journal-backed truth, mathematically enforced
balance, tenant-isolated persistence. The service layer refuses to post an unbalanced
entry, and every balance, report and KPI is derived from posted journal lines rather
than stored in a cache.

Go 1.25, PostgreSQL, and the standard library only — no web framework, no ORM.

---

## Live demo

**https://tera-web.helipod.app**

| | |
|---|---|
| Email | `demo@tera.co.id` |
| Password | `Demo1234` |

The demo tenant is fully interactive: create a draft journal, post it, issue an
invoice, record a receipt against it, and watch the balance sheet react. It runs on
Helipod (Kubernetes) behind two services — this API on the internal network, and the
Vue SPA on the public edge, proxying `/api` and `/internal` to this service over
private DNS.

The UI is a separate private repository (`Accurate_Frontend_AhmadTaufikR`); this
repository is the backend, and it stands on its own.

---

## Architecture

Clean Architecture with Domain-Driven Design. Dependencies point inward; the domain
knows nothing about HTTP or SQL.

```
cmd/server        process entry point: config, store selection, graceful shutdown
internal/http     transport — routing, middleware, request/response envelopes
internal/service  business rules — journal, invoice, account, report, tax, company
internal/repository  persistence — PostgreSQL and in-memory behind one interface
internal/domain   pure models, errors and constants — no imports from the layers above
```

The repository is an interface (`internal/repository/store.go`). Two implementations
satisfy it: a PostgreSQL store built on `pgx/v5`, and an in-memory store guarded by an
`RWMutex`. The same contract suite runs against both.

Routing uses `net/http`'s method-matching `ServeMux` (`"GET /api/v1/accounts"`), which
is why the dependency list stays at three modules.

---

## Accounting invariants

These are enforced in the service layer, not left to convention:

- **A journal balances.** `Σ debit == Σ credit` is checked before a transaction is
  written. An unbalanced entry is rejected; it never reaches the ledger.
- **Posted entries are immutable.** Drafts can be edited and deleted freely. Once
  posted, a journal is immutable — corrections go through a reversal, so the audit
  trail stays complete.
- **Balances are derived, never cached.** Account balances, the trial balance, the
  balance sheet and the dashboard all read posted journal lines. The `accounts.balance`
  column is a legacy cache and is explicitly documented as unusable as a source of
  truth.
- **A posting is atomic.** Issuing an invoice writes the receivable, the revenue and
  the tax journal entries in one transaction, together with the audit record. A failure
  rolls the whole thing back.
- **Invoices are documents, not numbers.** A cash receipt becomes a journal entry; the
  invoice status (`Draft → Issued → PartiallyPaid → Paid`, or `Overdue`) is derived
  from the receipt history.

---

## Persistence

PostgreSQL 16, accessed through `pgx/v5` with a connection pool. Schema changes are
versioned SQL files embedded into the binary (`internal/repository/migrations/`), so
local, CI and hosted environments apply exactly the same ordered changes.

| Migration | What it adds |
|---|---|
| `001_initial.sql` | Companies, branches, users, sessions, accounts, journals, invoices, audit log |
| `002_portfolio_core.sql` | Invoice payments, password reset tokens, number sequences |
| `003_performance.sql` | Tenant-scoped indexes for the queries that were full-scanning the line table, plus `CHECK` constraints that backstop the debit/credit invariants |

The migration runner takes a session-level advisory lock before applying anything, so
two instances starting at the same time cannot both apply the same migration — the
loser waits instead of dying on a duplicate-object error.

`DATABASE_URL` is optional. Without it the process starts against the in-memory store,
seeded with a realistic demo tenant, which is what makes a fresh clone runnable in one
command.

---

## Security model

| Concern | How it is handled |
|---|---|
| Passwords | bcrypt, work factor 12 |
| Sessions | 32 bytes of crypto/rand, stored only as a SHA-256 hash; the raw token never touches disk |
| Tenant isolation | `CompanyID` comes from the validated session context only — no query parameter or JSON payload can move a request across tenants |
| Login throttling | 5 failed attempts per 15-minute window per IP |
| Password reset | Single-use tokens with an expiry, swept on a trigger rather than by a cron |
| Response headers | `Content-Security-Policy: default-src 'none'` on API responses, `X-Frame-Options: DENY`, `nosniff`, `Strict-Transport-Security` when the request arrived over TLS |
| Errors | Domain errors carry a stable code and a safe message; internals are logged, not returned |

Every API response uses one envelope: `{ "data": ..., "meta": ..., "errors": [...] }`.

---

## Testing

```bash
go test -race ./...        # unit, service and contract suites
gofmt -l .                 # formatting
go vet ./...               # static analysis
govulncheck ./...          # known vulnerabilities in the dependency graph
```

The contract suite (`internal/contract`) runs the same scenarios against the in-memory
store and against PostgreSQL — set `TEST_DATABASE_URL` to include the database half.
CI runs both jobs on every push.

---

## Running locally

Requires Go 1.25 or newer.

```bash
git clone https://github.com/Ataramkr98/Accurate_Golang_AhmadTaufikR.git
cd Accurate_Golang_AhmadTaufikR

# Zero-dependency start: in-memory store, demo data seeded on boot
go run ./cmd/server
```

The server listens on `:8080` and answers `GET /healthz`. A host that injects `PORT`
(Helipod, Render, Heroku) wins over `TERA_ADDR`.

To use PostgreSQL instead:

```bash
export DATABASE_URL='postgresql://tera:tera_dev_password@localhost:5432/tera?sslmode=disable'
go run ./cmd/server
```

The schema is migrated and the demo tenant is seeded on start-up.

---

## API

All endpoints live under `/api/v1` and take a bearer token.

```bash
# Log in
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@tera.co.id","password":"Demo1234"}'
# -> {"data":{"token":"...","user":{...},"company":{...},"branches":[...]}, ...}

# Post a balanced journal entry
curl -X POST http://localhost:8080/api/v1/journal-entries \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "date": "2026-10-20",
    "memo": "Office equipment purchase",
    "status": "Draft",
    "lines": [
      {"accountCode": "1610", "debit": 15000000, "credit": 0, "memo": "Office Equipment"},
      {"accountCode": "1100", "debit": 0, "credit": 15000000, "memo": "Paid via Cash"}
    ]
  }'

# Read the balance sheet
curl "http://localhost:8080/api/v1/reports/balance-sheet" \
  -H "Authorization: Bearer $TOKEN"
```

The full contract is described in `docs/openapi.yaml`; the architecture notes are in
`docs/ARCHITECTURE.md`.

---

## License

MIT — see [LICENSE](LICENSE).
