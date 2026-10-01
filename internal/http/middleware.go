package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
)

type contextKey string

const (
	ctxUser      contextKey = "user"
	ctxCompany   contextKey = "company"
	ctxToken     contextKey = "token"
	ctxRequestID contextKey = "request_id"
)

// hstsValue pins a host to HTTPS for a year, subdomains included. It is only
// ever sent for requests that already arrived over TLS - see isTLSRequest.
const hstsValue = "max-age=31536000; includeSubDomains"

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func RequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID, err := randomToken()
		if err != nil {
			requestID = fmt.Sprintf("req-%d", time.Now().UnixNano())
		} else {
			requestID = requestID[:16]
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), ctxRequestID, requestID)
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(recorder, r.WithContext(ctx))
		attrs := []any{"request_id", requestID, "method", r.Method,
			"path", r.URL.Path, "status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(), "remote_ip", clientIP(r)}
		switch r.URL.Path {
		// Liveness and readiness probes fire every few seconds from the hosting
		// platform (Render, Docker HEALTHCHECK, the load balancer). At Info they
		// bury real traffic under noise, so they drop to Debug.
		case "/healthz", "/readyz":
			slog.Debug("http_probe", attrs...)
		default:
			slog.Info("http_request", attrs...)
		}
	})
}

// SecurityHeaders sets browser-hardening headers on every API response.
//
// The CSP here is `default-src 'none'` because these responses are JSON and
// are never rendered as a document: there is no legitimate reason for one to
// load a script, a frame or an image. The Vue single-page app served on the
// same origin needs a different policy and gets its own headers in
// static.go (applySPAHeaders) - the two must not be merged.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if isTLSRequest(r) {
			w.Header().Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}

// isTLSRequest reports whether the request reached this process over TLS,
// either terminated here (r.TLS) or by a front proxy (X-Forwarded-Proto).
// HSTS is emitted only when this is true: sending the header over plain HTTP
// makes a browser cache the HTTPS pin and breaks local development on
// http://localhost:8080 for the remainder of the max-age.
func isTLSRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// TokenStore issues opaque bearer tokens while persisting only SHA-256 hashes.
type TokenStore struct {
	store repository.Store
	ttl   time.Duration
	now   func() time.Time
}

func NewTokenStore(store repository.Store) *TokenStore {
	return &TokenStore{
		store: store,
		// SessionTTL, not a second literal: handleResetPassword revokes every
		// session of the account being reset, and that containment is only
		// worth as much as the longest TTL it has to cover.
		ttl: SessionTTL,
		now: time.Now,
	}
}

func (t *TokenStore) Issue(userID uint) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	user, ok := t.store.FindUserByID(userID)
	if !ok {
		return "", domain.ErrUnauthorized("user not found")
	}
	now := t.now()
	_ = t.store.DeleteExpiredSessions(now)
	// Sweep stale password-reset tokens on the same trigger. A failure here must
	// never block sign-in, but it must be visible: swallowing it silently is how
	// the table grows without bound until every login pays for it.
	if err := t.store.DeleteExpiredPasswordResetTokens(context.Background(), now); err != nil {
		slog.Warn("password_reset_token_sweep_failed", "err", err)
	}
	err = t.store.CreateSession(&domain.Session{
		TokenHash: tokenHash(token), UserID: userID, CompanyID: user.CompanyID,
		ExpiresAt: now.Add(t.ttl), CreatedAt: now,
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (t *TokenStore) Revoke(token string) {
	if token == "" {
		return
	}
	_ = t.store.RevokeSession(tokenHash(token), t.now())
}

func (t *TokenStore) UserID(token string) (uint, bool) {
	session, ok := t.store.GetSession(tokenHash(token), t.now())
	if !ok {
		return 0, false
	}
	return session.UserID, true
}

func tokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// defaultCORSOrigins is the allow-list used when TERA_ALLOWED_ORIGINS is unset,
// and the fallback when it is set to a wildcard.
var defaultCORSOrigins = []string{
	"http://localhost:5173",
	"http://127.0.0.1:5173",
}

// CORS middleware permits the Vite dev server during local development.
//
// Authentication is a Bearer header, not a cookie, so Access-Control-Allow-
// Credentials is deliberately absent: it is not needed, and pairing it with a
// reflected or wildcard origin is the classic way to turn "any site can call
// my API" into "any site can act as the signed-in user".
func CORS(next http.Handler) http.Handler {
	allowed := make([]string, 0, len(defaultCORSOrigins)+2)
	allowed = append(allowed, defaultCORSOrigins...)
	wildcard := false
	for _, origin := range strings.Split(os.Getenv("TERA_ALLOWED_ORIGINS"), ",") {
		origin = strings.TrimSpace(origin)
		switch origin {
		case "":
			// Unset or trailing comma; nothing to add.
		case "*":
			wildcard = true
		default:
			allowed = append(allowed, origin)
		}
	}
	if wildcard {
		// A literal "*" cannot legally be paired with credentialed requests and
		// browsers reject the combination outright. Rather than emit a wildcard
		// that silently does nothing, fall back to the default allow-list and
		// make the misconfiguration loud.
		slog.Warn("TERA_ALLOWED_ORIGINS contains a wildcard; ignoring it and using the default allow-list",
			"defaults", defaultCORSOrigins)
		allowed = defaultCORSOrigins
	}
	allowedOrigins := make(map[string]bool, len(allowed))
	for _, origin := range allowed {
		allowedOrigins[origin] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Add("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Idempotency-Key")
		if r.Method == http.MethodOptions {
			// A preflight is only ever sent by a browser, so a request carrying
			// no Origin is not one: pass it through to normal routing.
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !allowedOrigins[origin] {
				// Answering 204 to every OPTIONS told the browser the preflight
				// had passed and pushed the failure onto the real request, where
				// it surfaces as an opaque console CORS error. Reject it here,
				// with a body the caller can actually read.
				Error(w, domain.ErrForbidden("origin not allowed"))
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			NoContent(w)
			return
		}
		if origin != "" && allowedOrigins[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		next.ServeHTTP(w, r)
	})
}

// Auth resolves a Bearer token into the request context.
func Auth(tokens *TokenStore, store repository.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := strings.Fields(r.Header.Get("Authorization"))
			if len(authHeader) != 2 || !strings.EqualFold(authHeader[0], "Bearer") {
				Error(w, domain.ErrUnauthorized("token not found"))
				return
			}
			token := authHeader[1]
			userID, ok := tokens.UserID(token)
			if !ok {
				Error(w, domain.ErrUnauthorized("invalid token"))
				return
			}
			user := findUserByID(store, userID)
			if user == nil {
				Error(w, domain.ErrUnauthorized("user not found"))
				return
			}
			ctx := context.WithValue(r.Context(), ctxUser, user)
			ctx = context.WithValue(ctx, ctxCompany, user.CompanyID)
			ctx = context.WithValue(ctx, ctxToken, token)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// findUserByID scans users (small MVP store; would be a lookup in PostgreSQL).
func findUserByID(store repository.Store, userID uint) *domain.User {
	user, _ := store.FindUserByID(userID)
	return user
}

// UserFromContext returns the authenticated user.
func UserFromContext(r *http.Request) *domain.User {
	u, _ := r.Context().Value(ctxUser).(*domain.User)
	return u
}

// CompanyFromContext returns the authenticated user's company ID.
func CompanyFromContext(r *http.Request) uint {
	id, _ := r.Context().Value(ctxCompany).(uint)
	return id
}

func TokenFromContext(r *http.Request) string {
	token, _ := r.Context().Value(ctxToken).(string)
	return token
}
