package repository

import (
	"context"
	"time"

	"tera/internal/domain"
)

// Store is the persistence contract for the domain. The in-memory
// implementation satisfies it today; a PostgreSQL implementation can be
// swapped in behind the same interface without touching the service layer
// (PRD §10.6 multi-tenancy abstraction via repositories).
//
// The list methods return slices rather than (slice, error). That contract is
// what the service layer is written against, so a query failure reaches a
// service as an empty slice - a database outage is indistinguishable from an
// empty tenant at that boundary. FallibleStore below exists for callers that
// need to tell the two apart; the slice methods on each implementation are thin
// wrappers over it that also log the failure.
type Store interface {
	Ready(ctx context.Context) error
	WithTransaction(fn func(Store) error) error
	// Auth & tenants
	FindUserByEmail(email string) (*domain.User, bool)
	FindUserByID(id uint) (*domain.User, bool)
	CreateUser(user *domain.User) error
	UpdateUser(user *domain.User) error
	CreateSession(session *domain.Session) error
	GetSession(tokenHash string, now time.Time) (*domain.Session, bool)
	RevokeSession(tokenHash string, at time.Time) error
	RevokeSessionsForUser(ctx context.Context, userID int64) error
	DeleteExpiredSessions(now time.Time) error
	// DeleteExpiredPasswordResetTokens drops spent and expired reset tokens.
	// It takes a context (unlike DeleteExpiredSessions) because the auth layer
	// already holds the request-scoped one when it sweeps.
	DeleteExpiredPasswordResetTokens(ctx context.Context, now time.Time) error
	CreatePasswordResetToken(token *domain.PasswordResetToken) error
	ResetPassword(tokenHash, passwordHash string, now time.Time) (uint, error)
	GetCompany(id uint) (*domain.Company, error)
	CreateCompany(company *domain.Company) error
	UpdateCompany(company *domain.Company) error
	ListBranches(companyID uint) []domain.Branch
	GetBranch(companyID, id uint) (*domain.Branch, error)
	CreateBranch(branch *domain.Branch) error
	UpdateBranch(branch *domain.Branch) error
	DeleteBranch(companyID, id uint) error

	// Accounts
	ListAccounts(companyID uint) []domain.Account
	GetAccount(companyID, id uint) (*domain.Account, error)
	GetAccountByCode(companyID uint, code string) (*domain.Account, error)
	CreateAccount(account *domain.Account) error
	UpdateAccount(account *domain.Account) error
	DeleteAccount(companyID, id uint) error

	// Journal entries
	ListJournalEntries(companyID uint, status string) []domain.JournalEntry
	GetJournalEntry(companyID, id uint) (*domain.JournalEntry, error)
	CreateJournalEntry(entry *domain.JournalEntry) error
	UpdateJournalEntry(entry *domain.JournalEntry) error
	PostJournalEntry(companyID, id uint) error
	DeleteJournalEntry(companyID, id uint) error
	ListLedgerEntries(companyID, accountID uint) []domain.LedgerEntry
	NextNumber(companyID uint, prefix string) string

	// Invoices
	ListInvoices(companyID uint) []domain.Invoice
	GetInvoice(companyID, id uint) (*domain.Invoice, error)
	CreateInvoice(invoice *domain.Invoice) error
	// UpdateInvoice replaces a draft invoice and its lines. Published invoices
	// can only change through TransitionInvoiceStatus as part of a posting flow.
	UpdateInvoice(invoice *domain.Invoice) error
	TransitionInvoiceStatus(companyID, id uint, from, to string) error
	DeleteInvoice(companyID, id uint) error
	ListInvoicePayments(companyID, invoiceID uint) []domain.InvoicePayment
	CreateInvoicePayment(payment *domain.InvoicePayment) error
	ListCustomers(companyID uint) []domain.Customer
	GetCustomer(companyID, id uint) (*domain.Customer, error)
	CreateCustomer(customer *domain.Customer) error
	UpdateCustomer(customer *domain.Customer) error
	DeleteCustomer(companyID, id uint) error

	// Audit
	AppendAuditLog(log domain.AuditLog) error
	ListAuditLogs(companyID uint) []domain.AuditLog
}

// FallibleStore is an optional capability: error-returning read variants of the
// slice-returning Store list methods.
//
// A store can only keep the promise in a single place, so each implementation
// writes its query once against these and lets the slice-returning methods wrap
// it. Callers that must not confuse "no rows" with "the database is down" - the
// HTTP list handlers - use these; the service layer keeps the slice contract.
type FallibleStore interface {
	ReadBranches(companyID uint) ([]domain.Branch, error)
	ReadAccounts(companyID uint) ([]domain.Account, error)
	ReadJournalEntries(companyID uint, status string) ([]domain.JournalEntry, error)
	ReadLedgerEntries(companyID, accountID uint) ([]domain.LedgerEntry, error)
	ReadInvoices(companyID uint) ([]domain.Invoice, error)
	ReadInvoicePayments(companyID, invoiceID uint) ([]domain.InvoicePayment, error)
	ReadCustomers(companyID uint) ([]domain.Customer, error)
	ReadAuditLogs(companyID uint) ([]domain.AuditLog, error)
}

// UserDirectory is an optional capability: a store that resolves many users in
// one query. The audit-log page needs a display name per row, and looking each
// one up separately cost one query per row.
type UserDirectory interface {
	FindUsersByIDs(ids []uint) map[uint]*domain.User
}

// SequenceStore is an optional capability: a store that can pin its
// per-company document counters to a known value. Seeding uses it so the first
// document a user creates continues after the seeded numbers instead of
// colliding with them. Stores that cannot do this simply do not implement it.
type SequenceStore interface {
	SetNextNumber(companyID uint, prefix string, value int)
}
