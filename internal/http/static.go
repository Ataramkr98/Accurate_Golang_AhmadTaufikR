package httpapi

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"tera/internal/domain"
)

// spaCSP is the Content Security Policy for the compiled Vue single-page app.
//
// This is deliberately NOT the same policy the JSON API sends. The API replies
// with `default-src 'none'` because a response body that is never rendered as a
// document has no legitimate reason to load anything at all. The SPA is the one
// response the browser does render, so it needs a policy that permits its own
// bundle while still blocking third-party code:
//
//   - script-src 'self' with no 'unsafe-inline' / no 'unsafe-eval': the build
//     emits external module scripts only, and Vite's dev-mode HMR is served
//     from the same origin.
//   - style-src 'self' 'unsafe-inline': required. Vue SFC <style scoped> blocks
//     and Vite's injected <style> elements carry inline CSS that cannot be
//     hashed out of the document.
//   - font-src / img-src 'self' data: for the self-hosted @fontsource woff2
//     files and inlined SVG/data images. No third-party asset hosts are used.
//   - connect-src 'self': the SPA only ever calls its own /api/ prefix.
//   - frame-ancestors 'none' + base-uri 'self' + form-action 'self': no
//     framing, no <base> hijack, no off-origin form posts.
//
// The two policies must stay separate. Merging them would either lock the
// application out of its own bundle (API policy) or weaken the API to
// script-src 'self' on a non-HTML response (SPA policy). Neither is a win.
const spaCSP = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"font-src 'self' data:; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// NewSPAHandler serves the compiled Vue single-page app from dir while
// delegating API traffic to api. It is enabled by setting TERA_STATIC_DIR,
// which lets a single web service host both the UI and the API on one origin:
// no CORS configuration, one deployment unit, and SPA reloads work for deep
// links like /reports/balance-sheet.
func NewSPAHandler(api http.Handler, dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/internal/") ||
			p == "/healthz" || p == "/readyz" {
			api.ServeHTTP(w, r)
			return
		}
		// This handler is mounted OUTSIDE the middleware chain (cmd/server/main.go
		// wraps it around the fully-middled router), so the API's SecurityHeaders
		// middleware never runs for these responses. The SPA has to carry its own
		// headers or index.html and every /assets/* file ship with none at all.
		applySPAHeaders(w, r)
		if servesStaticFile(dir, p) {
			// Vite fingerprints asset filenames, so they are safe to cache hard.
			if strings.HasPrefix(p, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r, dir)
	})
}

// applySPAHeaders sets the browser-hardening headers on every response this
// handler serves itself. See spaCSP for why this is a different policy from the
// one the API sends. Delegated API responses are left alone: they are already
// hardened by SecurityHeaders on the inner router.
func applySPAHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", spaCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if isTLSRequest(r) {
		w.Header().Set("Strict-Transport-Security", hstsValue)
	}
}

// servesStaticFile reports whether the request path maps to a real file.
// Directories are excluded so /journals (a route, not a folder) falls back to
// the SPA entry point instead of a directory listing.
//
// The filepath.Clean + Join below is the path-traversal guard and it is
// intentional: Clean collapses any "..", "." or duplicate-separator segments
// before the path ever reaches the filesystem, so /../../etc/passwd can only
// ever resolve to a path underneath dir. http.FileServer performs the same
// normalisation on its own; this check is the allow/deny decision on top of it.
func servesStaticFile(dir, urlPath string) bool {
	clean := filepath.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	if clean == "/" || clean == "." {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(clean)))
	return err == nil && !info.IsDir()
}

func serveIndex(w http.ResponseWriter, r *http.Request, dir string) {
	data, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		// The build produced no entry point, or TERA_STATIC_DIR points somewhere
		// wrong. The detail (including the configured directory) goes to the
		// server log only; the client gets a generic envelope so a misconfigured
		// deploy cannot be used to probe the host filesystem.
		slog.Error("spa_index_unreadable", "dir", dir, "err", err)
		Error(w, domain.ErrNotFound("index.html not found"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
