# Security Policy & Architecture

## Security Model Overview

Tera is built on defensive-by-default engineering principles designed for public portfolio deployment. All financial data access adheres strictly to authenticated tenant boundaries.

### Core Security Controls

1. **Authentication & Session Management**
   - Sessions use high-entropy bearer tokens generated via crypto/rand.
   - Tokens are stored exclusively as **SHA-256 hashes** in the database. Raw tokens are only returned once to the client upon successful authentication.
   - Sessions have an enforced 24-hour expiration window.
   - Passwords are encrypted using **bcrypt** with a work factor of 12.
   - Passwords require a minimum of 8 characters containing uppercase, lowercase, and numeric characters.

2. **Strict Multi-Tenant Isolation**
   - Tenant scoping (`CompanyID`) is determined solely from the authenticated session context (`UserFromContext(r).CompanyID`).
   - Client requests cannot specify, override, or elevate tenant context through query parameters or request payloads.
   - Multi-branch filtering validates that the requested branch strictly belongs to the authenticated tenant.

3. **HTTP Transport & Header Hardening**

   Tera sends **two different Content Security Policies**, because one origin
   serves two different kinds of response. They are applied in two different
   places and must not be merged: merging them would either lock the
   application out of its own bundle (the API policy) or weaken the API to
   `script-src 'self'` on a response that is never rendered (the SPA policy).

   - **API responses** - set by `SecurityHeaders` in
     `backend/internal/http/middleware.go`, on every `/api/*` and `/internal/*`
     route. These return JSON and are never rendered as a document, so they get
     the maximally restrictive policy:

     ```
     default-src 'none'; frame-ancestors 'none'
     ```

     A JSON body has no legitimate reason to load a script, a frame, a font or
     an image. `frame-ancestors 'none'` additionally blocks clickjacking.

   - **SPA responses** - set by `applySPAHeaders` in
     `backend/internal/http/static.go`, on `index.html` and every `/assets/*`
     file. The browser *does* render these as a document, so the policy has to
     permit the application's own bundle while still blocking third-party code:

     ```
     default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';
     font-src 'self' data:; img-src 'self' data:; connect-src 'self';
     frame-ancestors 'none'; base-uri 'self'; form-action 'self'
     ```

     The two relaxations, and why they are the only two:
     - `style-src` allows `'unsafe-inline'` because Vue single-file component
       `<style scoped>` blocks and Vite's injected `<style>` elements carry
       inline CSS that cannot be hashed out of the document. `script-src` allows
       **neither** `'unsafe-inline'` **nor** `'unsafe-eval'` - the production
       build emits external module scripts only, and dev-mode HMR is served from
       the same origin.
     - `font-src` and `img-src` stay at `'self' data:` because fonts are
       self-hosted via `@fontsource`; the app loads nothing from a third-party
       asset host. `connect-src 'self'` is sufficient because the SPA only ever
       calls its own `/api/` prefix.
     - `base-uri 'self'` and `form-action 'self'` close off `<base>` hijacking
       and off-origin form posts.

   The SPA handler is mounted **outside** the middleware chain in
   `backend/cmd/server/main.go`, so the API's `SecurityHeaders` middleware never
   runs for the application document. That is precisely why `applySPAHeaders`
   exists: without it, `index.html` and every hashed JavaScript and CSS asset
   would ship with no CSP and no `X-Frame-Options` at all.

   - **X-Frame-Options**: `DENY` (both surfaces)
   - **X-Content-Type-Options**: `nosniff` (both surfaces)
   - **Referrer-Policy**: `strict-origin-when-cross-origin` (both surfaces)
   - **Permissions-Policy**: `camera=(), microphone=(), geolocation=()` (both surfaces)
   - **Strict-Transport-Security (HSTS)**: `max-age=31536000; includeSubDomains`.
     Sent **only when the request already arrived over TLS**, detected as
     `r.TLS != nil` or `X-Forwarded-Proto: https`. Emitting HSTS over a plain
     HTTP origin makes the browser cache the HTTPS pin and break local
     development on `http://localhost:8080`, so the check is deliberate.
   - **Cross-Origin Resource Sharing (CORS)**: strict allow-list validation.
     `Access-Control-Allow-Credentials` is **not** sent - Tera authenticates
     with a Bearer header, not a cookie, so credentialed CORS is unnecessary,
     and pairing it with a reflected or wildcard origin is the standard route to
     letting any site act as the signed-in user. If `TERA_ALLOWED_ORIGINS`
     contains `*`, the wildcard is refused, a warning is logged, and the
     built-in development allow-list (`http://localhost:5173`,
     `http://127.0.0.1:5173`) is used instead. A preflight (`OPTIONS`) whose
     origin is not on the allow-list is answered `403` with the standard error
     envelope, not a blanket `204` that defers the failure to an opaque console
     error on the real request.
4. **Abuse Prevention & Input Protection**
   - **Rate Limiting**: Login endpoint enforces maximum 5 failed attempts per 15-minute window per client IP.
   - **Payload Limits**: Request bodies are capped at 1 MB using `http.MaxBytesReader` to prevent buffer exhaustion.
   - **Strict JSON Parsing**: Request decoding disallows unknown fields (`DisallowUnknownFields`) and verifies single-object payloads.

5. **Auditing & Traceability**
   - Unique `X-Request-ID` is assigned to each request or propagated from reverse proxies.
   - All mutations to journals, invoices, and company settings append immutable audit log entries recording actor, action, timestamp, and entity diffs.

---

## Known Limitations

- **Rate Limiter Topology**: The default login rate limiter is in-memory and scoped per server process. Distributed deployments should front the application with a reverse proxy rate limiter (e.g. Cloudflare or Nginx) or a Redis-backed token bucket.
- **Shared Demo Account**: The public demo tenant (`demo@tera.co.id`) is shared and reset periodically.

---

## Reporting a Vulnerability

If you discover a security vulnerability in Tera, please report it responsibly by opening a confidential security advisory on GitHub or emailing `security@tera.example`.

Please include:
- Description of the issue
- Steps to reproduce or proof-of-concept
- Impact assessment
