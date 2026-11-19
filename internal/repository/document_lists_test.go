package repository

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"tera/internal/domain"
)

type documentQueryCounter struct{ count atomic.Int64 }

func (c *documentQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.count.Add(1)
	return ctx
}

func (*documentQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Use a private schema so these query-count tests never reset another test's
// tenant. A one-connection pool also catches queries issued with rows open.
func documentListStore(t *testing.T) (*PostgresStore, *documentQueryCounter) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL document-list regressions")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{fmt.Sprintf("document_lists_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close(ctx)
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	counter := &documentQueryCounter{}
	cfg.ConnConfig.Tracer = counter
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := MigrateTables(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO companies (id, name) VALUES (1, 'Test'), (2, 'Other tenant');
		INSERT INTO branches (id, company_id, name) VALUES (1, 1, 'Main'), (2, 2, 'Other');
		INSERT INTO accounts (id, company_id, code, name, type) VALUES (1, 1, '1100', 'Cash', 'asset'), (2, 2, '1100', 'Other cash', 'asset');
		INSERT INTO journal_entries (id, company_id, branch_id, number, date, status) VALUES
			(101, 1, 1, 'J-001', '2026-10-01', 'Posted'), (102, 1, 1, 'J-002', '2026-10-02', 'Draft'),
			(103, 1, 1, 'J-003', '2026-10-03', 'Posted'), (201, 2, 2, 'J-004', '2026-10-04', 'Posted');
		INSERT INTO journal_lines (id, journal_entry_id, account_id, debit, credit, memo) VALUES
			(12, 101, 1, 0, 125, 'second'), (11, 101, 1, 125, 0, 'first'),
			(13, 103, 1, 99, 0, 'third'), (21, 201, 2, 777, 0, 'other tenant');
		INSERT INTO invoices (id, company_id, branch_id, number, customer_name, date, due_date, status, created_at) VALUES
			(101, 1, 1, 'INV-001', 'A', '2026-10-01', '2026-10-31', 'Draft', '2026-10-01'),
			(102, 1, 1, 'INV-002', 'B', '2026-10-02', '2026-10-31', 'Draft', '2026-10-02'),
			(103, 1, 1, 'INV-003', 'C', '2026-10-03', '2026-10-31', 'Draft', '2026-10-02'),
			(201, 2, 2, 'INV-004', 'D', '2026-10-04', '2026-10-31', 'Draft', '2026-10-04');
		INSERT INTO invoice_lines (id, invoice_id, name, qty, price, discount_percent, subtotal) VALUES
			(12, 101, 'second', 2, 150, 10, 270), (11, 101, 'first', 1, 125, 0, 125),
			(13, 103, 'third', 3, 99, 0, 297), (21, 201, 'other tenant', 1, 777, 0, 777);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return &PostgresStore{pool: pool, db: pool}, counter
}

func TestPostgresJournalListsUseBoundedQueries(t *testing.T) {
	store, counter := documentListStore(t)
	for _, status := range []string{"", domain.JournalStatusPosted, "missing"} {
		counter.count.Store(0)
		entries, err := store.ReadJournalEntries(1, status)
		if err != nil {
			t.Fatal(err)
		}
		if queries := counter.count.Load(); queries > 2 {
			t.Errorf("status=%q: %d queries for %d journals, want at most 2", status, queries, len(entries))
		}
		wantIDs := []uint{103, 102, 101}
		if status == domain.JournalStatusPosted {
			wantIDs = []uint{103, 101}
		} else if status == "missing" {
			wantIDs = []uint{}
		}
		if len(entries) != len(wantIDs) || entries == nil {
			t.Fatalf("status=%q: journals=%v, want IDs %v", status, entries, wantIDs)
		}
		for i, entry := range entries {
			if entry.ID != wantIDs[i] {
				t.Fatalf("journal %d: ID=%d, want %d", i, entry.ID, wantIDs[i])
			}
			detail, err := store.GetJournalEntry(1, entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(entry, *detail) {
				t.Errorf("journal %d differs from its detail (including line order): got %+v, want %+v", entry.ID, entry, *detail)
			}
		}
	}
}

func TestPostgresInvoiceListsUseBoundedQueries(t *testing.T) {
	store, counter := documentListStore(t)
	for _, companyID := range []uint{1, 999} {
		counter.count.Store(0)
		invoices, err := store.ReadInvoices(companyID)
		if err != nil {
			t.Fatal(err)
		}
		if queries := counter.count.Load(); queries > 2 {
			t.Errorf("%d queries for %d invoices, want at most 2", queries, len(invoices))
		}
		wantIDs := []uint{103, 102, 101}
		if companyID == 999 {
			wantIDs = []uint{}
		}
		if len(invoices) != len(wantIDs) || invoices == nil {
			t.Fatalf("invoices=%v, want IDs %v", invoices, wantIDs)
		}
		for i, invoice := range invoices {
			if invoice.ID != wantIDs[i] {
				t.Fatalf("invoice %d: ID=%d, want %d", i, invoice.ID, wantIDs[i])
			}
			detail, err := store.GetInvoice(companyID, invoice.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(invoice, *detail) {
				t.Errorf("invoice %d differs from its detail (including line order): got %+v, want %+v", invoice.ID, invoice, *detail)
			}
		}
	}
}
