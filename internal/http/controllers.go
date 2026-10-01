package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"tera/internal/domain"
	"tera/internal/repository"
	"tera/internal/security"
	"tera/internal/service"
)

// SessionTTL is how long an issued bearer token stays valid.
//
// It is exported and lives here so the TTL has one definition: a password reset
// revokes every session (see handleResetPassword), and that is only a meaningful
// containment measure if the value the reset invalidates is the same one
// NewTokenStore hands out.
const SessionTTL = 24 * time.Hour

// Pagination bounds. The demo tenant seeds 31 accounts and 32 audit rows, so the
// previous 25/100 pair truncated whole views on the first page and made the
// pager totals disagree with what the user could see.
const (
	defaultPerPage = 50
	maxPerPage     = 200
)

// Server wires controllers to services.
type Server struct {
	store              repository.Store
	tokens             *TokenStore
	accounts           *service.AccountService
	companies          *service.CompanyService
	journals           *service.JournalService
	invoices           *service.InvoiceService
	reports            *service.ReportService
	tax                *service.TaxService
	allowSignup        bool
	allowPasswordReset bool
	demoResetSecret    string
	loginAttempts      *loginAttemptLimiter
	mailer             Mailer
}

func NewServer(store repository.Store) *Server {
	return NewServerWithMailer(store, LogMailer{})
}

func NewServerWithMailer(store repository.Store, mailer Mailer) *Server {
	tax := service.NewTaxService()
	journals := service.NewJournalService(store)
	accounts := service.NewAccountService(store)
	return &Server{
		store:              store,
		tokens:             NewTokenStore(store),
		accounts:           accounts,
		companies:          service.NewCompanyService(store, accounts),
		journals:           journals,
		invoices:           service.NewInvoiceService(store, tax, journals),
		reports:            service.NewReportService(store),
		tax:                tax,
		allowSignup:        envFlag("TERA_ENABLE_SIGNUP"),
		allowPasswordReset: envFlag("TERA_ENABLE_PASSWORD_RESET"),
		demoResetSecret:    strings.TrimSpace(os.Getenv("DEMO_RESET_SECRET")),
		loginAttempts:      newLoginAttemptLimiter(),
		mailer:             mailer,
	}
}

type demoResetStore interface {
	WipeDemoTenant(context.Context) error
}

// handleDemoReset is deliberately separate from the public API surface. It
// exists only for the hosted, resettable portfolio tenant and requires a
// deployment secret; normal users cannot invoke it with their Bearer token.
func (s *Server) handleDemoReset(w http.ResponseWriter, r *http.Request) {
	if s.demoResetSecret == "" {
		// Unreachable in practice: the route is only mounted when the secret is
		// configured. Kept correct anyway, because a plain-text 404 here would
		// be the one response in the file outside the envelope.
		Error(w, domain.ErrNotFound("demo reset is not enabled on this deployment"))
		return
	}
	provided := r.Header.Get("X-Demo-Reset-Secret")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(s.demoResetSecret)) != 1 {
		Error(w, domain.ErrUnauthorized("invalid demo reset credential"))
		return
	}
	resetter, ok := s.store.(demoResetStore)
	if !ok {
		// The in-memory store has no WipeDemoTenant: its demo tenant is built in
		// the constructor and cannot be cleared row by row, so a reset would have
		// to restart the process.
		Error(w, domain.NewError(domain.CodeDemoResetUnavailable,
			"the in-memory store cannot be wiped; demo reset requires a PostgreSQL-backed store",
			http.StatusServiceUnavailable))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := resetter.WipeDemoTenant(ctx); err != nil {
		Error(w, err)
		return
	}
	if err := repository.SeedDemoData(s.store); err != nil {
		Error(w, err)
		return
	}
	OK(w, map[string]bool{"reset": true})
}

// --- Auth ---

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type registerRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Company  string `json:"companyName"`
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

type resetPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeBody(w, r, &req); err != nil {
		Error(w, err)
		return
	}
	email := normalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		Error(w, domain.ErrValidation("email and password are required"))
		return
	}
	key, allowed := s.allowLogin(w, r, email)
	if !allowed {
		return
	}
	user, ok := s.store.FindUserByEmail(email)
	if !ok || !security.CheckPassword(user.Password, req.Password) {
		s.loginAttempts.failed(key, time.Now())
		Error(w, domain.ErrUnauthorized("invalid email or password"))
		return
	}
	s.loginAttempts.succeeded(key)
	company, err := s.store.GetCompany(user.CompanyID)
	if err != nil {
		Error(w, err)
		return
	}
	token, err := s.tokens.Issue(user.ID)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, map[string]any{
		"token":    token,
		"user":     user,
		"company":  company,
		"branches": s.store.ListBranches(user.CompanyID),
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.allowSignup {
		Error(w, domain.ErrForbidden("sign-up is disabled in this environment"))
		return
	}
	// Sign-up is the other unauthenticated write that a bot can hammer: it
	// creates a company, a branch, a chart of accounts and a user per call.
	if !s.allowVolume(w, r, "register:") {
		return
	}
	var req registerRequest
	if err := decodeBody(w, r, &req); err != nil {
		Error(w, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = normalizeEmail(req.Email)
	req.Company = strings.TrimSpace(req.Company)
	missing := map[string]string{}
	if req.Name == "" {
		missing["name"] = "name is required"
	}
	if req.Email == "" {
		missing["email"] = "email is required"
	}
	if req.Password == "" {
		missing["password"] = "password is required"
	}
	if req.Company == "" {
		missing["companyName"] = "company name is required"
	}
	if len(missing) > 0 {
		ValidationError(w, "name, email, password, and company name are required", missing)
		return
	}
	if err := validateRequestLimits(
		fieldLimit{"name", req.Name, limitUserName},
		fieldLimit{"email", req.Email, limitUserEmail},
		fieldLimit{"companyName", req.Company, limitCompanyName},
	); err != nil {
		Error(w, err)
		return
	}
	if !validEmail(req.Email) {
		Error(w, domain.ErrValidation("invalid email format"))
		return
	}
	if !validPassword(req.Password) {
		Error(w, domain.ErrValidation("password must be at least 8 characters and include uppercase, lowercase, and a number"))
		return
	}
	if _, exists := s.store.FindUserByEmail(req.Email); exists {
		Error(w, domain.ErrConflict(domain.CodeDuplicate, "email is already registered"))
		return
	}
	company := &domain.Company{Name: req.Company, SAKMode: domain.SAKEMKM}
	if err := s.store.CreateCompany(company); err != nil {
		Error(w, err)
		return
	}
	if err := s.store.CreateBranch(&domain.Branch{CompanyID: company.ID, Name: "Main Office"}); err != nil {
		Error(w, err)
		return
	}
	if err := s.accounts.EnsureDefaults(company.ID); err != nil {
		Error(w, err)
		return
	}
	passwordHash, err := security.HashPassword(req.Password)
	if err != nil {
		Error(w, err)
		return
	}
	user := &domain.User{
		CompanyID: company.ID, Email: req.Email, Password: passwordHash,
		Name: req.Name, Role: "Owner",
	}
	if err := s.store.CreateUser(user); err != nil {
		Error(w, err)
		return
	}
	created, err := s.store.GetCompany(company.ID)
	if err != nil {
		Error(w, err)
		return
	}
	token, err := s.tokens.Issue(user.ID)
	if err != nil {
		Error(w, err)
		return
	}
	Created(w, map[string]any{
		"token":    token,
		"user":     user,
		"company":  created,
		"branches": s.store.ListBranches(company.ID),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.tokens.Revoke(TokenFromContext(r))
	NoContent(w)
}

func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !s.allowPasswordReset {
		Error(w, domain.ErrForbidden("password recovery is disabled in this environment"))
		return
	}
	// Each call writes a token row and hands the work to the mailer, so it is
	// limited per caller rather than per account: a shared address must not be
	// usable to enumerate which accounts exist.
	if !s.allowVolume(w, r, "forgot:") {
		return
	}
	var req forgotPasswordRequest
	if err := decodeBody(w, r, &req); err != nil {
		Error(w, err)
		return
	}
	email := normalizeEmail(req.Email)
	if !validEmail(email) {
		Error(w, domain.ErrValidation("invalid email format"))
		return
	}
	if user, ok := s.store.FindUserByEmail(email); ok {
		rawToken, err := randomToken()
		if err == nil {
			now := time.Now()
			err = s.store.CreatePasswordResetToken(&domain.PasswordResetToken{
				TokenHash: tokenHash(rawToken), UserID: user.ID,
				ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now,
			})
			if err == nil {
				base := strings.TrimRight(strings.TrimSpace(os.Getenv("TERA_FRONTEND_URL")), "/")
				if base == "" {
					base = "http://localhost:5173"
				}
				_ = s.mailer.SendPasswordReset(r.Context(), user.Email,
					base+"/reset-password?token="+url.QueryEscape(rawToken))
			}
		}
		if err := s.store.AppendAuditLog(domain.AuditLog{
			CompanyID: user.CompanyID, UserID: user.ID, Action: "auth.password_reset_requested",
			Entity: "user", CreatedAt: time.Now(),
		}); err != nil {
			Error(w, err)
			return
		}
	}
	Accepted(w, map[string]string{
		"message": "If the email exists, a password reset link has been sent.",
	})
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.allowPasswordReset {
		Error(w, domain.ErrForbidden("password recovery is disabled in this environment"))
		return
	}
	// Brute-forcing a reset token is a password-guessing attack with a different
	// endpoint, so it gets the same window.
	if !s.allowVolume(w, r, "reset:") {
		return
	}
	var req resetPasswordRequest
	if err := decodeBody(w, r, &req); err != nil {
		Error(w, err)
		return
	}
	if strings.TrimSpace(req.Token) == "" || !validPassword(req.Password) {
		Error(w, domain.ErrValidation("a valid token and strong password are required"))
		return
	}
	passwordHash, err := security.HashPassword(req.Password)
	if err != nil {
		Error(w, err)
		return
	}
	userID, err := s.store.ResetPassword(tokenHash(req.Token), passwordHash, time.Now())
	if err != nil {
		Error(w, err)
		return
	}
	// A password change must invalidate every existing bearer token. Without
	// this, a token captured before the reset keeps working for the full session
	// TTL (see SessionTTL), which is precisely the window a stolen token is
	// worth. Failing to revoke is reported rather than swallowed: the password
	// has already changed at this point, so the caller has to sign in again.
	if err := s.store.RevokeSessionsForUser(r.Context(), int64(userID)); err != nil {
		Error(w, err)
		return
	}
	NoContent(w)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	company, err := s.store.GetCompany(user.CompanyID)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, map[string]any{
		"user":     user,
		"company":  company,
		"branches": s.store.ListBranches(user.CompanyID),
	})
}

type profileUpdateRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleProfileUpdate(w http.ResponseWriter, r *http.Request) {
	var req profileUpdateRequest
	if err := decodeBody(w, r, &req); err != nil {
		Error(w, err)
		return
	}
	// user comes from the context, not the body, and is written through below -
	// so the name has to be explicitly replaced. Silently ignoring a blank name
	// used to answer 200 having changed nothing, which reads as "saved" in the UI
	// while the old name stayed.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		ValidationError(w, "name is required", map[string]string{"name": "name cannot be empty"})
		return
	}
	if err := validateRequestLimits(fieldLimit{"name", name, limitUserName}); err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	user.Name = name
	if err := s.store.UpdateUser(user); err != nil {
		Error(w, err)
		return
	}
	OK(w, user)
}

func (s *Server) handleCompanyUpdate(w http.ResponseWriter, r *http.Request) {
	var input service.CompanyInput
	if err := decodeBody(w, r, &input); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(companyLimits(input)...); err != nil {
		Error(w, err)
		return
	}
	company, err := s.companies.Update(CompanyFromContext(r), input)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, company)
}

func (s *Server) handleBranchesList(w http.ResponseWriter, r *http.Request) {
	options, err := parseListOptions(r, "name")
	if err != nil {
		Error(w, err)
		return
	}
	branches := s.companies.ListBranches(CompanyFromContext(r))
	filtered := branches[:0]
	q := strings.ToLower(options.Query)
	for _, branch := range branches {
		if q == "" || strings.Contains(strings.ToLower(branch.Name), q) {
			filtered = append(filtered, branch)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if options.Order == "desc" {
			return filtered[i].Name > filtered[j].Name
		}
		return filtered[i].Name < filtered[j].Name
	})
	page, meta := paginate(filtered, options)
	if err := s.verifyReadable(len(page) == 0, func(f repository.FallibleStore) error {
		_, err := f.ReadBranches(CompanyFromContext(r))
		return err
	}); err != nil {
		Error(w, err)
		return
	}
	OKWithMeta(w, page, meta)
}

type branchCreateRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleBranchCreate(w http.ResponseWriter, r *http.Request) {
	var req branchCreateRequest
	if err := decodeBody(w, r, &req); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(branchLimits(req.Name)...); err != nil {
		Error(w, err)
		return
	}
	b, err := s.companies.CreateBranch(CompanyFromContext(r), req.Name)
	if err != nil {
		Error(w, err)
		return
	}
	Created(w, b)
}

func (s *Server) handleBranchUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	var req branchCreateRequest
	if err := decodeBody(w, r, &req); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(branchLimits(req.Name)...); err != nil {
		Error(w, err)
		return
	}
	b, err := s.companies.UpdateBranch(CompanyFromContext(r), id, req.Name)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, b)
}

func (s *Server) handleBranchDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	if err := s.companies.DeleteBranch(CompanyFromContext(r), id); err != nil {
		Error(w, err)
		return
	}
	NoContent(w)
}

// --- Accounts ---

func (s *Server) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	options, err := parseListOptions(r, "code", "name", "type", "balance")
	if err != nil {
		Error(w, err)
		return
	}
	accounts := s.accounts.List(CompanyFromContext(r))
	filtered := accounts[:0]
	q := strings.ToLower(options.Query)
	for _, account := range accounts {
		if q == "" || strings.Contains(strings.ToLower(account.Code+" "+account.Name), q) {
			filtered = append(filtered, account)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		cmp := 0
		switch options.Sort {
		case "name":
			cmp = strings.Compare(filtered[i].Name, filtered[j].Name)
		case "type":
			cmp = strings.Compare(filtered[i].Type, filtered[j].Type)
		case "balance":
			cmp = compareInt64(filtered[i].Balance, filtered[j].Balance)
		default:
			cmp = strings.Compare(filtered[i].Code, filtered[j].Code)
		}
		return orderedLess(cmp, options.Order)
	})
	page, meta := paginate(filtered, options)
	if err := s.verifyReadable(len(page) == 0, func(f repository.FallibleStore) error {
		_, err := f.ReadAccounts(CompanyFromContext(r))
		return err
	}); err != nil {
		Error(w, err)
		return
	}
	OKWithMeta(w, page, meta)
}

func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var input service.AccountInput
	if err := decodeBody(w, r, &input); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(accountLimits(input)...); err != nil {
		Error(w, err)
		return
	}
	acc, err := s.accounts.Create(CompanyFromContext(r), input)
	if err != nil {
		Error(w, err)
		return
	}
	Created(w, acc)
}

func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	var input service.AccountInput
	if err := decodeBody(w, r, &input); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(accountLimits(input)...); err != nil {
		Error(w, err)
		return
	}
	acc, err := s.accounts.Update(CompanyFromContext(r), id, input)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, acc)
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	if err := s.accounts.Delete(CompanyFromContext(r), id); err != nil {
		Error(w, err)
		return
	}
	NoContent(w)
}

func (s *Server) handleAccountLedger(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	detail, err := s.reports.Ledger(CompanyFromContext(r), id)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, detail)
}

// --- Journals ---

func (s *Server) handleJournalsList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	options, err := parseListOptions(r, "number", "date", "memo", "status", "total")
	if err != nil {
		Error(w, err)
		return
	}
	branchID, err := s.branchFilter(r)
	if err != nil {
		Error(w, err)
		return
	}
	from, to, err := dateRange(r)
	if err != nil {
		Error(w, err)
		return
	}
	entries := s.journals.List(CompanyFromContext(r), status)
	filtered := entries[:0]
	q := strings.ToLower(options.Query)
	for _, entry := range entries {
		if branchID != 0 && entry.BranchID != branchID || from != "" && entry.Date < from || to != "" && entry.Date > to {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(entry.Number+" "+entry.Memo), q) {
			filtered = append(filtered, entry)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		cmp := 0
		switch options.Sort {
		case "date":
			cmp = strings.Compare(filtered[i].Date, filtered[j].Date)
		case "memo":
			cmp = strings.Compare(filtered[i].Memo, filtered[j].Memo)
		case "status":
			cmp = strings.Compare(filtered[i].Status, filtered[j].Status)
		case "total":
			cmp = compareInt64(filtered[i].Total, filtered[j].Total)
		default:
			cmp = strings.Compare(filtered[i].Number, filtered[j].Number)
		}
		return orderedLess(cmp, options.Order)
	})
	page, meta := paginate(filtered, options)
	if err := s.verifyReadable(len(page) == 0, func(f repository.FallibleStore) error {
		_, err := f.ReadJournalEntries(CompanyFromContext(r), status)
		return err
	}); err != nil {
		Error(w, err)
		return
	}
	OKWithMeta(w, page, meta)
}

func (s *Server) handleJournalCreate(w http.ResponseWriter, r *http.Request) {
	var input service.JournalInput
	if err := decodeBody(w, r, &input); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(journalLineLimits(input)...); err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	entry, err := s.journals.Create(CompanyFromContext(r), user.ID, input)
	if err != nil {
		Error(w, err)
		return
	}
	Created(w, journalSummaryOf(*entry))
}

func (s *Server) handleJournalGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	entry, err := s.journals.Get(CompanyFromContext(r), id)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, entry)
}

func (s *Server) handleJournalUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	var input service.JournalInput
	if err := decodeBody(w, r, &input); err != nil {
		Error(w, err)
		return
	}
	companyID := CompanyFromContext(r)
	if err := validateRequestLimits(journalLineLimits(input)...); err != nil {
		Error(w, err)
		return
	}
	if err := s.inheritJournalBranch(companyID, id, &input.BranchID); err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	entry, err := s.journals.Update(companyID, user.ID, id, input)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, journalSummaryOf(*entry))
}

func (s *Server) handleJournalPost(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	entry, err := s.journals.Post(CompanyFromContext(r), user.ID, id)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, journalSummaryOf(*entry))
}

func (s *Server) handleJournalDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	if err := s.journals.Delete(CompanyFromContext(r), user.ID, id); err != nil {
		Error(w, err)
		return
	}
	NoContent(w)
}

// --- Invoices ---

func (s *Server) handleInvoicesList(w http.ResponseWriter, r *http.Request) {
	options, err := parseListOptions(r, "number", "date", "dueDate", "customer", "status", "total")
	if err != nil {
		Error(w, err)
		return
	}
	invoices := s.invoices.List(CompanyFromContext(r))
	filtered := invoices[:0]
	q := strings.ToLower(options.Query)
	status := r.URL.Query().Get("status")
	branchID, err := s.branchFilter(r)
	if err != nil {
		Error(w, err)
		return
	}
	for _, invoice := range invoices {
		if branchID != 0 && invoice.BranchID != branchID || status != "" && invoice.Status != status {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(invoice.Number+" "+invoice.CustomerName), q) {
			continue
		}
		filtered = append(filtered, invoice)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		cmp := 0
		switch options.Sort {
		case "date":
			cmp = strings.Compare(filtered[i].Date, filtered[j].Date)
		case "dueDate":
			cmp = strings.Compare(filtered[i].DueDate, filtered[j].DueDate)
		case "customer":
			cmp = strings.Compare(filtered[i].CustomerName, filtered[j].CustomerName)
		case "status":
			cmp = strings.Compare(filtered[i].Status, filtered[j].Status)
		case "total":
			cmp = compareInt64(filtered[i].Total, filtered[j].Total)
		default:
			cmp = strings.Compare(filtered[i].Number, filtered[j].Number)
		}
		return orderedLess(cmp, options.Order)
	})
	page, meta := paginate(filtered, options)
	if err := s.verifyReadable(len(page) == 0, func(f repository.FallibleStore) error {
		_, err := f.ReadInvoices(CompanyFromContext(r))
		return err
	}); err != nil {
		Error(w, err)
		return
	}
	OKWithMeta(w, page, meta)
}

func (s *Server) handleInvoiceCreate(w http.ResponseWriter, r *http.Request) {
	var input service.InvoiceInput
	if err := decodeBody(w, r, &input); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(invoiceLimits(input)...); err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	invoice, err := s.invoices.Create(CompanyFromContext(r), user.ID, input)
	if err != nil {
		Error(w, err)
		return
	}
	Created(w, invoice)
}

func (s *Server) handleInvoiceGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	invoice, err := s.invoices.Get(CompanyFromContext(r), id)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, invoice)
}

func (s *Server) handleInvoiceUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	var input service.InvoiceInput
	if err := decodeBody(w, r, &input); err != nil {
		Error(w, err)
		return
	}
	companyID := CompanyFromContext(r)
	if err := validateRequestLimits(invoiceLimits(input)...); err != nil {
		Error(w, err)
		return
	}
	if err := s.inheritInvoiceBranch(companyID, id, &input.BranchID); err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	inv, err := s.invoices.UpdateDraft(companyID, user.ID, id, input)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, inv)
}

func (s *Server) handleInvoiceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	if err := s.invoices.Delete(CompanyFromContext(r), user.ID, id); err != nil {
		Error(w, err)
		return
	}
	NoContent(w)
}

func (s *Server) handleInvoiceIssue(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	invoice, err := s.invoices.Issue(CompanyFromContext(r), user.ID, id)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, invoice)
}

func (s *Server) handleInvoicePaymentsList(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	payments, err := s.invoices.ListPayments(CompanyFromContext(r), id)
	if err != nil {
		Error(w, err)
		return
	}
	options, parseErr := parseListOptions(r, "date", "amount")
	if parseErr != nil {
		Error(w, parseErr)
		return
	}
	sort.SliceStable(payments, func(i, j int) bool {
		cmp := strings.Compare(payments[i].Date, payments[j].Date)
		if options.Sort == "amount" {
			cmp = compareInt64(payments[i].Amount, payments[j].Amount)
		}
		return orderedLess(cmp, options.Order)
	})
	page, meta := paginate(payments, options)
	if err := s.verifyReadable(len(page) == 0, func(f repository.FallibleStore) error {
		_, err := f.ReadInvoicePayments(CompanyFromContext(r), id)
		return err
	}); err != nil {
		Error(w, err)
		return
	}
	OKWithMeta(w, page, meta)
}

func (s *Server) handleInvoicePaymentCreate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	var input service.InvoicePaymentInput
	if err := decodeBody(w, r, &input); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(fieldLimit{"notes", input.Notes, limitInvoiceNotes}); err != nil {
		Error(w, err)
		return
	}
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	invoice, err := s.invoices.RecordPayment(CompanyFromContext(r), user.ID, id, input)
	if err != nil {
		Error(w, err)
		return
	}
	Created(w, invoice)
}

// --- Customers ---

func (s *Server) handleCustomersList(w http.ResponseWriter, r *http.Request) {
	options, err := parseListOptions(r, "name", "email")
	if err != nil {
		Error(w, err)
		return
	}
	customers := s.invoices.ListCustomers(CompanyFromContext(r))
	filtered := customers[:0]
	q := strings.ToLower(options.Query)
	for _, customer := range customers {
		if q == "" || strings.Contains(strings.ToLower(customer.Name+" "+customer.Email+" "+customer.Phone), q) {
			filtered = append(filtered, customer)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		cmp := strings.Compare(filtered[i].Name, filtered[j].Name)
		if options.Sort == "email" {
			cmp = strings.Compare(filtered[i].Email, filtered[j].Email)
		}
		return orderedLess(cmp, options.Order)
	})
	page, meta := paginate(filtered, options)
	if err := s.verifyReadable(len(page) == 0, func(f repository.FallibleStore) error {
		_, err := f.ReadCustomers(CompanyFromContext(r))
		return err
	}); err != nil {
		Error(w, err)
		return
	}
	OKWithMeta(w, page, meta)
}

func (s *Server) handleCustomerGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	cust, err := s.invoices.GetCustomer(CompanyFromContext(r), id)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, cust)
}

func (s *Server) handleCustomerCreate(w http.ResponseWriter, r *http.Request) {
	var c domain.Customer
	if err := decodeBody(w, r, &c); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(customerLimits(c)...); err != nil {
		Error(w, err)
		return
	}
	created, err := s.invoices.CreateCustomer(CompanyFromContext(r), c)
	if err != nil {
		Error(w, err)
		return
	}
	Created(w, created)
}

func (s *Server) handleCustomerUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	var c domain.Customer
	if err := decodeBody(w, r, &c); err != nil {
		Error(w, err)
		return
	}
	if err := validateRequestLimits(customerLimits(c)...); err != nil {
		Error(w, err)
		return
	}
	updated, err := s.invoices.UpdateCustomer(CompanyFromContext(r), id, c)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, updated)
}

func (s *Server) handleCustomerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		Error(w, err)
		return
	}
	if err := s.invoices.DeleteCustomer(CompanyFromContext(r), id); err != nil {
		Error(w, err)
		return
	}
	NoContent(w)
}

// --- Reports & dashboard ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	branchID, err := s.branchFilter(r)
	if err != nil {
		Error(w, err)
		return
	}
	dash, err := s.reports.DashboardScoped(CompanyFromContext(r), branchID, strings.TrimSpace(r.URL.Query().Get("period")))
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, dash)
}

func (s *Server) handleBalanceSheet(w http.ResponseWriter, r *http.Request) {
	branchID, err := s.branchFilter(r)
	if err != nil {
		Error(w, err)
		return
	}
	asOf, err := isoDateQuery(r, "asOf")
	if err != nil {
		Error(w, err)
		return
	}
	bs, err := s.reports.BalanceSheetScoped(CompanyFromContext(r), branchID, asOf)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, bs)
}

func (s *Server) handleProfitLoss(w http.ResponseWriter, r *http.Request) {
	branchID, err := s.branchFilter(r)
	if err != nil {
		Error(w, err)
		return
	}
	from, to, err := dateRange(r)
	if err != nil {
		Error(w, err)
		return
	}
	pl, err := s.reports.ProfitLossScoped(CompanyFromContext(r), branchID, from, to)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, pl)
}

func (s *Server) handleCashFlow(w http.ResponseWriter, r *http.Request) {
	branchID, err := s.branchFilter(r)
	if err != nil {
		Error(w, err)
		return
	}
	from, to, err := dateRange(r)
	if err != nil {
		Error(w, err)
		return
	}
	cf, err := s.reports.CashFlowScoped(CompanyFromContext(r), branchID, from, to)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, cf)
}

func (s *Server) handleReceivablesAging(w http.ResponseWriter, r *http.Request) {
	branchID, err := s.branchFilter(r)
	if err != nil {
		Error(w, err)
		return
	}
	asOf, err := isoDateQuery(r, "asOf")
	if err != nil {
		Error(w, err)
		return
	}
	report, err := s.reports.ReceivablesAgingScoped(CompanyFromContext(r), branchID, asOf)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, report)
}

func (s *Server) handleTaxSummary(w http.ResponseWriter, r *http.Request) {
	branchID, err := s.branchFilter(r)
	if err != nil {
		Error(w, err)
		return
	}
	from, to, err := dateRange(r)
	if err != nil {
		Error(w, err)
		return
	}
	ts, err := s.reports.TaxSummaryScoped(CompanyFromContext(r), branchID, from, to)
	if err != nil {
		Error(w, err)
		return
	}
	OK(w, ts)
}

type auditLogResponse struct {
	ID         uint   `json:"id"`
	UserID     uint   `json:"userId"`
	UserName   string `json:"userName"`
	Action     string `json:"action"`
	Entity     string `json:"entity"`
	BeforeJSON string `json:"beforeJson"`
	AfterJSON  string `json:"afterJson"`
	CreatedAt  string `json:"createdAt"`
}

func (s *Server) handleAuditLogsList(w http.ResponseWriter, r *http.Request) {
	// The store returns events newest-first; preserve that order. This handler
	// used to walk the slice backwards, which was correct only while the store
	// returned oldest-first — reversing twice yielded oldest-first output.
	companyID := CompanyFromContext(r)
	logs := s.store.ListAuditLogs(companyID)
	// One lookup for the whole page instead of one per row: a 50-row page used
	// to cost 51 queries, which is what made this endpoint the slowest on the
	// dashboard. The display name is resolved for every distinct author on the
	// page, which is a handful of users rather than a row count.
	names := s.userNamesFor(logs)
	result := make([]auditLogResponse, 0, len(logs))
	for _, entry := range logs {
		userName := "Tera User"
		if name, ok := names[entry.UserID]; ok && name != "" {
			userName = name
		}
		result = append(result, auditLogResponse{
			ID: entry.ID, UserID: entry.UserID, UserName: userName,
			Action: entry.Action, Entity: entry.Entity,
			BeforeJSON: entry.BeforeJSON, AfterJSON: entry.AfterJSON,
			CreatedAt: entry.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	options, err := parseListOptions(r, "createdAt", "action", "entity")
	if err != nil {
		Error(w, err)
		return
	}
	if r.URL.Query().Get("order") == "" {
		options.Order = "desc"
	}
	q := strings.ToLower(options.Query)
	filtered := result[:0]
	for _, entry := range result {
		if q == "" || strings.Contains(strings.ToLower(entry.UserName+" "+entry.Action+" "+entry.Entity), q) {
			filtered = append(filtered, entry)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		cmp := strings.Compare(filtered[i].CreatedAt, filtered[j].CreatedAt)
		if options.Sort == "action" {
			cmp = strings.Compare(filtered[i].Action, filtered[j].Action)
		} else if options.Sort == "entity" {
			cmp = strings.Compare(filtered[i].Entity, filtered[j].Entity)
		}
		return orderedLess(cmp, options.Order)
	})
	page, meta := paginate(filtered, options)
	if err := s.verifyReadable(len(page) == 0, func(f repository.FallibleStore) error {
		_, err := f.ReadAuditLogs(CompanyFromContext(r))
		return err
	}); err != nil {
		Error(w, err)
		return
	}
	OKWithMeta(w, page, meta)
}

// --- Helpers ---

// maxRequestBytes caps a decoded body. Larger bodies are rejected by
// http.MaxBytesReader before they are buffered, so an oversized upload cannot
// exhaust the process.
const maxRequestBytes = 1 << 20

// decodeBody reads exactly one JSON object into dst, rejecting unknown fields
// and trailing content.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return decodeError(err)
	}
	return nil
}

// decodeError classifies a decode failure. A body over the size cap is a 413
// with its own code: reporting it as "not valid JSON" told the client its
// payload was malformed when the only problem was its size.
func decodeError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return domain.ErrPayloadTooLarge("request body exceeds the 1 MB limit")
	}
	if err.Error() == "http: request body too large" {
		return domain.ErrPayloadTooLarge("request body exceeds the 1 MB limit")
	}
	return domain.ErrValidation("request body is not valid JSON")
}

// pathID reads a positive integer path parameter. The parse width and the
// uint conversion have to agree: parsing 64 bits and narrowing to a 32-bit uint
// silently turns a large id into a different, valid-looking one.
func pathID(r *http.Request, param string) (uint, error) {
	raw := r.PathValue(param)
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 || id > uint64(^uint(0)) {
		return 0, domain.ErrValidation("invalid id")
	}
	return uint(id), nil
}

type listOptions struct {
	Page    int
	PerPage int
	Query   string
	Sort    string
	Order   string
}

func parseListOptions(r *http.Request, allowedSort ...string) (listOptions, error) {
	// The demo dataset holds ~31 accounts and ~32 audit rows, so a smaller
	// default silently truncated whole views and made the pager totals wrong.
	// 50 covers the seeded tenant in one page and 200 is still a bounded amount
	// of work for a single-tenant MVP.
	options := listOptions{Page: 1, PerPage: defaultPerPage, Query: strings.TrimSpace(r.URL.Query().Get("q")), Order: "asc"}
	var err error
	if raw := r.URL.Query().Get("page"); raw != "" {
		options.Page, err = strconv.Atoi(raw)
		if err != nil || options.Page < 1 {
			return options, domain.ErrValidation("page must be a positive integer")
		}
	}
	if raw := r.URL.Query().Get("perPage"); raw != "" {
		options.PerPage, err = strconv.Atoi(raw)
		if err != nil || options.PerPage < 1 || options.PerPage > maxPerPage {
			return options, domain.ErrValidation(fmt.Sprintf("perPage must be between 1 and %d", maxPerPage))
		}
	}
	options.Sort = strings.TrimSpace(r.URL.Query().Get("sort"))
	if options.Sort == "" && len(allowedSort) > 0 {
		options.Sort = allowedSort[0]
	}
	if options.Sort != "" {
		valid := false
		for _, field := range allowedSort {
			valid = valid || options.Sort == field
		}
		if !valid {
			return options, domain.ErrValidation("unsupported sort field")
		}
	}
	if order := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("order"))); order != "" {
		if order != "asc" && order != "desc" {
			return options, domain.ErrValidation("order must be asc or desc")
		}
		options.Order = order
	}
	return options, nil
}

// paginate slices items for the requested page. Total counts the rows matching
// the filter across every page, not the rows returned, and TotalPages is derived
// from it in NewMeta.
func paginate[T any](items []T, options listOptions) ([]T, Meta) {
	meta := NewMeta(options.Page, options.PerPage, int64(len(items)))
	start := (options.Page - 1) * options.PerPage
	if start >= len(items) {
		return []T{}, meta
	}
	end := start + options.PerPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], meta
}

func orderedLess(comparison int, order string) bool {
	if order == "desc" {
		return comparison > 0
	}
	return comparison < 0
}

func compareInt64(left, right int64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func dateRange(r *http.Request) (string, string, error) {
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	// A fixed order, not map iteration: the previous map-based loop reported
	// whichever invalid parameter the runtime happened to visit first, so the
	// message for the same request differed between calls.
	for _, bound := range []struct{ name, value string }{{"from", from}, {"to", to}} {
		if bound.value == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", bound.value); err != nil {
			return "", "", domain.ErrValidation(bound.name + " must use YYYY-MM-DD")
		}
	}
	if from != "" && to != "" && from > to {
		return "", "", domain.ErrValidation("from cannot be after to")
	}
	return from, to, nil
}

func isoDateQuery(r *http.Request, name string) (string, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return "", nil
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return "", domain.ErrValidation(name + " must use YYYY-MM-DD")
	}
	return value, nil
}

func (s *Server) branchFilter(r *http.Request) (uint, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("branchId"))
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 || id > uint64(^uint(0)) {
		return 0, domain.ErrValidation("branchId must be a positive integer")
	}
	if _, err := s.store.GetBranch(CompanyFromContext(r), uint(id)); err != nil {
		return 0, domain.ErrValidation("branchId does not belong to this company")
	}
	return uint(id), nil
}

func normalizeEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func validEmail(value string) bool {
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value
}

func validPassword(value string) bool {
	if len([]rune(value)) < 8 {
		return false
	}
	var upper, lower, digit bool
	for _, r := range value {
		upper = upper || unicode.IsUpper(r)
		lower = lower || unicode.IsLower(r)
		digit = digit || unicode.IsDigit(r)
	}
	return upper && lower && digit
}

func envFlag(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}

// --- Journal projection, request identity, field limits ---

// journalSummaryOf projects a stored entry into the shape the journal list
// returns.
//
// The frontend's JournalList.vue splices the response of create/update/post
// straight into its rows and renders entry.total, but domain.JournalEntry has
// no total and no source field: a freshly saved journal therefore appeared in
// the table with an amount of Rp 0 until the page was reloaded. Reusing
// service.JournalSummary - the exact type the list endpoint already returns -
// keeps a single definition of the list shape; the total is the sum of debits,
// which equals the credits because a journal is only stored when it balances.
func journalSummaryOf(entry domain.JournalEntry) service.JournalSummary {
	var total int64
	for _, line := range entry.Lines {
		total += line.Debit
	}
	return service.JournalSummary{
		ID:       entry.ID,
		BranchID: entry.BranchID,
		Number:   entry.Number,
		Date:     entry.Date,
		Source:   entry.SourceType,
		Memo:     entry.Memo,
		Status:   entry.Status,
		Total:    total,
	}
}

// currentUser returns the authenticated user, answering 401 and reporting false
// when the context carries none.
//
// The Auth middleware always populates it, so this is not reachable through the
// router today. It exists because two of the call sites write through the
// pointer (handleProfileUpdate) or read its fields (handleMe): a future route
// mounted without the middleware would otherwise nil-panic inside a handler
// instead of answering 401.
func (s *Server) currentUser(w http.ResponseWriter, r *http.Request) (*domain.User, bool) {
	user := UserFromContext(r)
	if user == nil {
		Error(w, domain.ErrUnauthorized("authentication required"))
		return nil, false
	}
	return user, true
}

// inheritJournalBranch keeps an edited journal on the branch it already
// belongs to. See inheritInvoiceBranch for why this is necessary.
func (s *Server) inheritJournalBranch(companyID, id uint, target *uint) error {
	if *target != 0 {
		return nil
	}
	current, err := s.store.GetJournalEntry(companyID, id)
	if err != nil {
		return err
	}
	*target = current.BranchID
	return nil
}

// inheritInvoiceBranch carries the stored branch onto an update when the client
// omits branchId.
//
// Both inputs carry a BranchID, and an omitted field decodes to 0, which the
// service resolves to branches[0] (service.resolveBranch). The frontend's edit
// forms do not send branchId at all, so editing a draft silently re-parented
// the document to the first branch - a data-integrity bug that only surfaces
// once somebody filters a report by branch. Reading the stored value back turns
// "field absent" into "unchanged", while an explicit branchId is still honoured
// and still validated by the service.
func (s *Server) inheritInvoiceBranch(companyID, id uint, target *uint) error {
	if *target != 0 {
		return nil
	}
	current, err := s.store.GetInvoice(companyID, id)
	if err != nil {
		return err
	}
	*target = current.BranchID
	return nil
}

// userNamesFor resolves the display name of every distinct author on a page of
// audit rows. The store is asked once for the whole set instead of once per
// row, which took a 50-row page from 51 queries to 2.
func (s *Server) userNamesFor(logs []domain.AuditLog) map[uint]string {
	ids := make([]uint, 0, len(logs))
	seen := make(map[uint]bool, len(logs))
	for _, entry := range logs {
		if entry.UserID != 0 && !seen[entry.UserID] {
			seen[entry.UserID] = true
			ids = append(ids, entry.UserID)
		}
	}
	names := make(map[uint]string, len(ids))
	directory, batched := s.store.(repository.UserDirectory)
	if !batched {
		for _, id := range ids {
			if user, ok := s.store.FindUserByID(id); ok {
				names[id] = user.Name
			}
		}
		return names
	}
	for id, user := range directory.FindUsersByIDs(ids) {
		if user != nil {
			names[id] = user.Name
		}
	}
	return names
}

// verifyReadable separates "no matching rows" from "the query failed".
//
// The service layer is written against the slice-returning Store contract, so a
// database outage reaches a list handler as an empty page - indistinguishable
// from an empty tenant, which is how an incident turns into "all our data is
// gone" reports. Re-reading through the store's error-returning variant costs
// one extra round trip and only runs when the page is already empty.
func (s *Server) verifyReadable(pageEmpty bool, read func(repository.FallibleStore) error) error {
	if !pageEmpty {
		return nil
	}
	fallible, ok := s.store.(repository.FallibleStore)
	if !ok {
		return nil
	}
	if err := read(fallible); err != nil {
		return fmt.Errorf("read failed: %w", err)
	}
	return nil
}

// Field length limits.
//
// Each value that reaches a VARCHAR column is bounded by the schema
// (repository/migrations/001_initial.sql and 002_portfolio_core.sql). Nothing
// checked them before: a 51-character account code satisfied service validation
// and came back as a raw Postgres "value too long", which the HTTP layer can only
// report as an opaque 500. Limits marked "API guard rail" sit on TEXT columns,
// which the database does not bound; those are chosen here to keep a document
// from growing without limit.
const (
	limitAccountCode     = 50  // accounts.code VARCHAR(50)
	limitAccountName     = 255 // accounts.name VARCHAR(255)
	limitCompanyName     = 255 // companies.name VARCHAR(255)
	limitCompanyNPWP     = 50  // companies.npwp VARCHAR(50)
	limitCompanyAddress  = 255 // companies.address TEXT - API guard rail
	limitBranchName      = 255 // branches.name VARCHAR(255)
	limitCustomerName    = 255 // customers.name VARCHAR(255)
	limitCustomerEmail   = 255 // customers.email VARCHAR(255)
	limitCustomerPhone   = 100 // customers.phone VARCHAR(100)
	limitCustomerAddress = 255 // customers.address TEXT - API guard rail
	limitInvoiceNotes    = 500 // invoices.notes TEXT - API guard rail
	limitInvoiceCustomer = 255 // invoices.customer_name VARCHAR(255)
	limitInvoiceLineName = 255 // invoice_lines.name VARCHAR(255)
	limitJournalMemo     = 300 // journal_entries.memo TEXT - API guard rail
	limitJournalLineMemo = 200 // journal_lines.memo TEXT - API guard rail
	limitUserName        = 255 // users.name VARCHAR(255)
	limitUserEmail       = 255 // users.email VARCHAR(255)
)

// fieldLimit pairs a decoded request field with the maximum length accepted for
// it. The limit counts runes, matching how PostgreSQL counts VARCHAR characters,
// so a 50-character code in a non-Latin script is not rejected.
type fieldLimit struct {
	name  string
	value string
	max   int
}

// validateRequestLimits returns a 422 validation error naming every field that
// is too long, so a client can highlight all of them at once instead of
// discovering them one round trip at a time. A nil result means the request is
// within bounds. Empty values are skipped: a missing value is a "required"
// problem that the service layer already reports.
func validateRequestLimits(limits ...fieldLimit) error {
	fields := make(map[string]string)
	for _, limit := range limits {
		if limit.max <= 0 || limit.value == "" {
			continue
		}
		if utf8.RuneCountInString(limit.value) > limit.max {
			fields[limit.name] = fmt.Sprintf("must be at most %d characters", limit.max)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return domain.NewValidationError("one or more fields exceed the maximum length", fields)
}

// accountLimits covers the account columns written on create and update.
func accountLimits(input service.AccountInput) []fieldLimit {
	return []fieldLimit{
		{"code", input.Code, limitAccountCode},
		{"name", input.Name, limitAccountName},
	}
}

// companyLimits covers the company columns written by the settings form.
func companyLimits(input service.CompanyInput) []fieldLimit {
	return []fieldLimit{
		{"name", input.Name, limitCompanyName},
		{"npwp", input.NPWP, limitCompanyNPWP},
		{"address", input.Address, limitCompanyAddress},
	}
}

// branchLimits covers branches.name, the only free-text column on a branch -
// the table carries no code or address column, so there is nothing else to
// bound.
func branchLimits(name string) []fieldLimit {
	return []fieldLimit{{"name", name, limitBranchName}}
}

// customerLimits covers every free-text customer column.
func customerLimits(c domain.Customer) []fieldLimit {
	return []fieldLimit{
		{"name", c.Name, limitCustomerName},
		{"email", c.Email, limitCustomerEmail},
		{"phone", c.Phone, limitCustomerPhone},
		{"address", c.Address, limitCustomerAddress},
	}
}

// invoiceLimits covers the invoice header and its line descriptions, which are
// reported per index so the client can point at the offending row.
func invoiceLimits(input service.InvoiceInput) []fieldLimit {
	limits := []fieldLimit{
		{"notes", input.Notes, limitInvoiceNotes},
		{"customerName", input.CustomerName, limitInvoiceCustomer},
	}
	for i, line := range input.Lines {
		limits = append(limits, fieldLimit{fmt.Sprintf("lines[%d].name", i), line.Name, limitInvoiceLineName})
	}
	return limits
}

// journalLineLimits covers the entry memo and every line memo, both of which the
// ledger renders verbatim.
func journalLineLimits(input service.JournalInput) []fieldLimit {
	limits := []fieldLimit{{"memo", input.Memo, limitJournalMemo}}
	for i, line := range input.Lines {
		limits = append(limits, fieldLimit{fmt.Sprintf("lines[%d].memo", i), line.Memo, limitJournalLineMemo})
	}
	return limits
}

// Rate limiting.
//
// A single limiter serves every unauthenticated auth endpoint; the key prefix
// keeps their budgets independent so a burst of password guesses cannot also
// lock a legitimate user out of registration or recovery.
const (
	// loginLimiterPrefix scopes login attempts to one email from one client.
	loginLimiterPrefix = "login:"
	// maxAttemptsPerWindow is the number of failures tolerated per key.
	maxAttemptsPerWindow = 5
	// attemptWindow is how long a failure budget lasts.
	attemptWindow = 15 * time.Minute
	// retryAfterSeconds is advertised alongside a 429; it matches attemptWindow.
	retryAfterSeconds = 900
	// maxLimiterEntries bounds the in-memory table. Rotating email addresses is
	// a trivial way to grow it without limit, and entries were otherwise only
	// evicted when the very same key came back.
	maxLimiterEntries = 4096
)

type loginAttempt struct {
	failures    int
	windowStart time.Time
}

type loginAttemptLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func newLoginAttemptLimiter() *loginAttemptLimiter {
	return &loginAttemptLimiter{attempts: map[string]loginAttempt{}}
}

// allowLogin checks the failure budget for one (client, email) pair, writes the
// 429 itself when the budget is spent, and returns the key the caller must
// report the outcome against.
func (s *Server) allowLogin(w http.ResponseWriter, r *http.Request, email string) (string, bool) {
	key := loginLimiterPrefix + clientIP(r) + "|" + email
	if s.loginAttempts.allowed(key, time.Now()) {
		return key, true
	}
	rateLimited(w, "too many failed login attempts; try again later")
	return "", false
}

// allowVolume caps an unauthenticated endpoint that has no credential to get
// wrong, so request volume is the only thing there is to limit. The key is per
// client rather than per account: a shared address must not be able to
// discover which accounts exist by timing recovery requests against it.
func (s *Server) allowVolume(w http.ResponseWriter, r *http.Request, prefix string) bool {
	if s.loginAttempts.hit(prefix+clientIP(r), time.Now()) {
		return true
	}
	rateLimited(w, "too many requests; try again later")
	return false
}

// rateLimited writes the shared 429. Retry-After matches the failure window, so
// a client that honours it is never rejected on a fresh budget.
func rateLimited(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	Error(w, domain.NewError(domain.CodeRateLimited, message, http.StatusTooManyRequests))
}

// hit records a request against a volume budget and reports whether it fits.
// Unlike allowed it counts requests rather than failures, and re-opens the
// window once the previous one has closed.
func (l *loginAttemptLimiter) hit(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.attempts) >= maxLimiterEntries {
		l.sweepLocked(now)
	}
	attempt, ok := l.attempts[key]
	if !ok || now.Sub(attempt.windowStart) >= attemptWindow {
		l.attempts[key] = loginAttempt{failures: 1, windowStart: now}
		return true
	}
	if attempt.failures >= maxAttemptsPerWindow {
		return false
	}
	attempt.failures++
	l.attempts[key] = attempt
	return true
}

func (l *loginAttemptLimiter) allowed(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt, ok := l.attempts[key]
	if !ok || now.Sub(attempt.windowStart) >= attemptWindow {
		if ok {
			delete(l.attempts, key)
		}
		return true
	}
	return attempt.failures < maxAttemptsPerWindow
}

func (l *loginAttemptLimiter) failed(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.attempts) >= maxLimiterEntries {
		l.sweepLocked(now)
	}
	attempt := l.attempts[key]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) >= attemptWindow {
		attempt = loginAttempt{windowStart: now}
	}
	attempt.failures++
	l.attempts[key] = attempt
}

// sweepLocked drops every key whose window has already closed. It runs only
// once the table reaches maxLimiterEntries, so the steady state stays a single
// map lookup and the sweep amortises to nothing.
func (l *loginAttemptLimiter) sweepLocked(now time.Time) {
	for key, attempt := range l.attempts {
		if now.Sub(attempt.windowStart) >= attemptWindow {
			delete(l.attempts, key)
		}
	}
}

func (l *loginAttemptLimiter) succeeded(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// clientIP returns the address the rate limiter and the request log key on.
//
// Trust assumption: the API only ever runs behind a reverse proxy we operate
// (Render's edge in front of nginx). That proxy appends the address of the peer
// it received the request from, so the RIGHTMOST X-Forwarded-For entry is the
// client address as seen at our edge and cannot be spoofed by a caller - a
// caller-supplied leftmost entry sits before it and is ignored. Reading
// RemoteAddr instead (the previous behaviour) collapsed every request onto the
// proxy address, so the limiter degraded to per-email and any anonymous visitor
// could lock a known account for the whole window. If the service is ever
// exposed directly, the header is absent and RemoteAddr is the only source that
// can be trusted anyway, which is why it stays the fallback.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if candidate := normalizedIP(strings.TrimSpace(parts[i])); candidate != "" {
				return candidate
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// normalizedIP keeps only well-formed IP literals, so a malformed or spoofed
// header entry cannot become an unbounded rate-limiter key.
func normalizedIP(value string) string {
	if ip := net.ParseIP(value); ip != nil {
		return ip.String()
	}
	return ""
}
