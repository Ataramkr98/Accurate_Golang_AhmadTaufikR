package httpapi

import (
	"context"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"tera/internal/domain"
)

// Router builds the versioned API mux. Public routes (login/register) sit
// outside the auth group; all business routes require a Bearer token.
func (s *Server) Router() http.Handler {
	// Authenticated routes. api registers a method-qualified pattern and records
	// the verbs each path accepts, so a request with the wrong verb is answered
	// from the same envelope as everything else instead of falling through to
	// net/http's plain-text 405.
	authed := http.NewServeMux()
	verbs := map[string][]string{}
	api := func(method, pattern string, handler http.HandlerFunc) {
		authed.HandleFunc(method+" "+pattern, handler)
		verbs[pattern] = append(verbs[pattern], method)
	}

	// Auth
	api(http.MethodGet, "/api/v1/auth/me", s.handleMe)
	api(http.MethodPut, "/api/v1/auth/profile", s.handleProfileUpdate)
	api(http.MethodPost, "/api/v1/auth/logout", s.handleLogout)

	// Company & Branches
	api(http.MethodPut, "/api/v1/company", s.handleCompanyUpdate)
	api(http.MethodGet, "/api/v1/branches", s.handleBranchesList)
	api(http.MethodPost, "/api/v1/branches", s.handleBranchCreate)
	api(http.MethodPut, "/api/v1/branches/{id}", s.handleBranchUpdate)
	api(http.MethodDelete, "/api/v1/branches/{id}", s.handleBranchDelete)

	// Accounts
	api(http.MethodGet, "/api/v1/accounts", s.handleAccountsList)
	api(http.MethodPost, "/api/v1/accounts", s.handleAccountCreate)
	api(http.MethodPut, "/api/v1/accounts/{id}", s.handleAccountUpdate)
	api(http.MethodDelete, "/api/v1/accounts/{id}", s.handleAccountDelete)
	api(http.MethodGet, "/api/v1/accounts/{id}/ledger", s.handleAccountLedger)

	// Journal entries
	api(http.MethodGet, "/api/v1/journal-entries", s.handleJournalsList)
	api(http.MethodPost, "/api/v1/journal-entries", s.handleJournalCreate)
	api(http.MethodGet, "/api/v1/journal-entries/{id}", s.handleJournalGet)
	api(http.MethodPut, "/api/v1/journal-entries/{id}", s.handleJournalUpdate)
	api(http.MethodDelete, "/api/v1/journal-entries/{id}", s.handleJournalDelete)
	api(http.MethodPost, "/api/v1/journal-entries/{id}/post", s.handleJournalPost)

	// Invoices
	api(http.MethodGet, "/api/v1/invoices", s.handleInvoicesList)
	api(http.MethodPost, "/api/v1/invoices", s.handleInvoiceCreate)
	api(http.MethodGet, "/api/v1/invoices/{id}", s.handleInvoiceGet)
	api(http.MethodPut, "/api/v1/invoices/{id}", s.handleInvoiceUpdate)
	api(http.MethodDelete, "/api/v1/invoices/{id}", s.handleInvoiceDelete)
	api(http.MethodPost, "/api/v1/invoices/{id}/issue", s.handleInvoiceIssue)
	api(http.MethodGet, "/api/v1/invoices/{id}/payments", s.handleInvoicePaymentsList)
	api(http.MethodPost, "/api/v1/invoices/{id}/payments", s.handleInvoicePaymentCreate)

	// Customers
	api(http.MethodGet, "/api/v1/customers", s.handleCustomersList)
	api(http.MethodPost, "/api/v1/customers", s.handleCustomerCreate)
	api(http.MethodGet, "/api/v1/customers/{id}", s.handleCustomerGet)
	api(http.MethodPut, "/api/v1/customers/{id}", s.handleCustomerUpdate)
	api(http.MethodDelete, "/api/v1/customers/{id}", s.handleCustomerDelete)

	// Reports & dashboard
	api(http.MethodGet, "/api/v1/dashboard", s.handleDashboard)
	api(http.MethodGet, "/api/v1/reports/balance-sheet", s.handleBalanceSheet)
	api(http.MethodGet, "/api/v1/reports/profit-loss", s.handleProfitLoss)
	api(http.MethodGet, "/api/v1/reports/cash-flow", s.handleCashFlow)
	api(http.MethodGet, "/api/v1/reports/ar-aging", s.handleReceivablesAging)
	api(http.MethodGet, "/api/v1/tax/summary", s.handleTaxSummary)
	api(http.MethodGet, "/api/v1/audit-logs", s.handleAuditLogsList)

	// A method-less twin of every path, registered after the method-qualified
	// patterns. Go's precedence rule makes the qualified pattern strictly more
	// specific, so the twin never shadows a real handler - it only catches the
	// verbs the path does not accept and renders them as the JSON envelope with
	// an Allow header. Registering the twin is what keeps the API subtree
	// symmetric: without it, net/http answers with plain text.
	for pattern, methods := range verbs {
		authed.HandleFunc(pattern, methodNotAllowed(sortedMethods(methods)))
	}
	// Unmatched /api/** paths get the same JSON 404 as everything else.
	authed.HandleFunc("/api/", notFound)

	final := http.NewServeMux()
	final.HandleFunc("/api/v1/auth/login", requireMethod(http.MethodPost, s.handleLogin))
	final.HandleFunc("/api/v1/auth/register", requireMethod(http.MethodPost, s.handleRegister))
	final.HandleFunc("/api/v1/auth/forgot-password", requireMethod(http.MethodPost, s.handleForgotPassword))
	final.HandleFunc("/api/v1/auth/reset-password", requireMethod(http.MethodPost, s.handleResetPassword))
	if s.demoResetSecret != "" {
		final.HandleFunc("/internal/demo/reset", requireMethod(http.MethodPost, s.handleDemoReset))
	}
	final.Handle("/api/", Auth(s.tokens, s.store)(authed))
	final.HandleFunc("/healthz", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		// Liveness answers in the standard envelope like every other endpoint,
		// which nests the pair under "data":
		//   {"data":{"revision":"...","status":"ok"}}
		// The nightly demo-reset workflow and the hosting probes grep the raw
		// body for the literal "status":"ok"; it is still there, and every
		// consumer that understands the envelope keeps working.
		OK(w, map[string]string{"status": "ok", "revision": buildRevision()})
	}))
	final.HandleFunc("/readyz", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.store.Ready(ctx); err != nil {
			Error(w, domain.ErrNotReady("database is not ready"))
			return
		}
		OK(w, map[string]string{"status": "ready", "revision": buildRevision()})
	}))

	return RequestContext(SecurityHeaders(CORS(final)))
}

func buildRevision() string {
	for _, name := range []string{"BUILD_REVISION", "RENDER_GIT_COMMIT"} {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return "development"
}

func requireMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			Error(w, domain.ErrMethodNotAllowed("HTTP method not supported"))
			return
		}
		next(w, r)
	}
}

// methodNotAllowed renders a JSON 405 for a path that exists under other verbs.
func methodNotAllowed(methods []string) http.HandlerFunc {
	allow := strings.Join(methods, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		Error(w, domain.ErrMethodNotAllowed("HTTP method "+r.Method+" is not supported for this resource"))
	}
}

// notFound renders a JSON 404 for an unmatched path inside the API subtree.
func notFound(w http.ResponseWriter, r *http.Request) {
	Error(w, domain.ErrNotFound("no API route matches "+r.URL.Path))
}

// sortedMethods copies and sorts an allowed-verb list so the Allow header is
// stable regardless of registration order.
func sortedMethods(methods []string) []string {
	out := append([]string(nil), methods...)
	sort.Strings(out)
	return out
}
