# Tera backend

Apply the [Anti Slop code rules](https://github.com/miqdadbadjuber/anti-slop/tree/1969b459f843603ba2efbbb24e8e1bf963c889b3/skills/antislop-code). Use direct Go code; keep comments that explain a constraint or invariant, not ones that restate the code. Avoid abstractions without a second use.

Preserve tenant isolation, posted-journal truth, balanced postings, and the HTTP response envelope. Public health responses must not expose store errors or credentials. Keep liveness independent of PostgreSQL and readiness dependent on it.
