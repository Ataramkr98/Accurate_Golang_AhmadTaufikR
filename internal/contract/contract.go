// Package contract provides dual-store helpers so the repository and service
// contract suites run identically against the in-memory store and, when
// TEST_DATABASE_URL is set (CI's backend-postgres job), against PostgreSQL.
package contract

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
)

// DemoEmail is the public demo credential shared by seeds, README, and tests.
const DemoEmail = "demo@tera.co.id"

// Case is one store under test together with the demo tenant's company ID.
type Case struct {
	Name      string
	Store     repository.Store
	CompanyID uint
}

var (
	pgOnce  sync.Once
	pgStore *repository.PostgresStore
	pgErr   error
	pgURL   string
)

// Stores returns the contract cases for this run: always memory, plus a
// wiped-and-reseeded PostgreSQL store when TEST_DATABASE_URL is configured.
// PostgreSQL state is shared across cases in the package so migrations run
// once; each call resets the demo tenant for isolation.
func Stores(t *testing.T) []Case {
	t.Helper()

	mem := repository.NewMemoryStore()
	cases := []Case{{
		Name:      "memory",
		Store:     mem,
		CompanyID: DemoCompanyID(t, mem),
	}}

	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		return cases
	}

	pgOnce.Do(func() {
		pgStore, pgErr = openPostgres(url)
		pgURL = url
	})
	if pgErr != nil {
		t.Fatalf("open postgres for contract suite (TEST_DATABASE_URL): %v", pgErr)
	}
	if strings.TrimSpace(os.Getenv("TEST_DATABASE_URL")) != pgURL {
		t.Fatal("TEST_DATABASE_URL changed mid-run; contract suite expects a stable database")
	}

	ResetPostgres(t, pgStore)
	cases = append(cases, Case{
		Name:      "postgres",
		Store:     pgStore,
		CompanyID: DemoCompanyID(t, pgStore),
	})
	return cases
}

func openPostgres(url string) (*repository.PostgresStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return repository.NewPostgresStore(ctx, url)
}

// ResetPostgres wipes the demo tenant and reseeds so each contract case starts
// from the shared seed dataset.
func ResetPostgres(t *testing.T, store *repository.PostgresStore) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.WipeDemoTenant(ctx); err != nil {
		t.Fatalf("wipe demo tenant: %v", err)
	}
	if err := repository.SeedDemoData(store); err != nil {
		t.Fatalf("seed demo data: %v", err)
	}
}

// DemoCompanyID resolves the demo tenant's company ID, which is not fixed on
// PostgreSQL after a wipe/reseed sequence.
func DemoCompanyID(t *testing.T, store repository.Store) uint {
	t.Helper()
	user, ok := store.FindUserByEmail(DemoEmail)
	if !ok {
		t.Fatalf("demo user %s not found; store was not seeded", DemoEmail)
	}
	return user.CompanyID
}

// FaultyStore wraps a Store so selected operations inside WithTransaction fail
// on demand, proving that numbering, documents, journals, payments, and audit
// rows commit or roll back as a unit.
type FaultyStore struct {
	repository.Store
	FailAudit         bool
	FailCreateJournal bool
	FailPostJournal   bool
	FailCreatePayment bool
}

// WithTransaction passes a fault-injecting view of the transactional store to fn.
func (f *FaultyStore) WithTransaction(fn func(repository.Store) error) error {
	return f.Store.WithTransaction(func(tx repository.Store) error {
		return fn(&FaultyTx{Store: tx, root: f})
	})
}

// FaultyTx is the transactional view used inside FaultyStore.WithTransaction.
type FaultyTx struct {
	repository.Store
	root *FaultyStore
}

func (f *FaultyTx) AppendAuditLog(log domain.AuditLog) error {
	if f.root.FailAudit {
		return domain.ErrValidation("forced audit write failure")
	}
	return f.Store.AppendAuditLog(log)
}

func (f *FaultyTx) CreateJournalEntry(entry *domain.JournalEntry) error {
	if f.root.FailCreateJournal {
		return domain.ErrValidation("forced journal create failure")
	}
	return f.Store.CreateJournalEntry(entry)
}

func (f *FaultyTx) PostJournalEntry(companyID, id uint) error {
	if f.root.FailPostJournal {
		return domain.ErrValidation("forced journal post failure")
	}
	return f.Store.PostJournalEntry(companyID, id)
}

func (f *FaultyTx) CreateInvoicePayment(payment *domain.InvoicePayment) error {
	if f.root.FailCreatePayment {
		return domain.ErrValidation("forced payment write failure")
	}
	return f.Store.CreateInvoicePayment(payment)
}
