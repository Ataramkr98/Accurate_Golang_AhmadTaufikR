package service

import (
	"testing"

	"tera/internal/domain"
	"tera/internal/repository"
)

func newInvoiceFixture(t *testing.T) (*repository.MemoryStore, *InvoiceService, InvoiceInput) {
	t.Helper()
	store := repository.NewMemoryStore()
	journal := NewJournalService(store)
	service := NewInvoiceService(store, NewTaxService(), journal)
	return store, service, InvoiceInput{
		CustomerName: "PT Portfolio Customer",
		Date:         "2026-09-21",
		DueDate:      "2026-10-21",
		Notes:        "Portfolio invoice",
		Lines: []InvoiceLineInput{
			{Name: "Accounting implementation", Qty: 1, Price: 10_000_000},
		},
	}
}

func TestInvoiceCreateStoresDraftWithoutPosting(t *testing.T) {
	store, invoices, input := newInvoiceFixture(t)
	before := len(store.ListJournalEntries(1, ""))

	invoice, err := invoices.Create(1, 1, input)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if invoice.Status != domain.InvoiceStatusDraft {
		t.Fatalf("status = %q, want %q", invoice.Status, domain.InvoiceStatusDraft)
	}
	if got := len(store.ListJournalEntries(1, "")); got != before {
		t.Fatalf("journal count = %d, want %d before issue", got, before)
	}
}

func TestInvoiceDraftCanBeEditedButIssuedInvoiceIsImmutable(t *testing.T) {
	_, invoices, input := newInvoiceFixture(t)
	invoice, err := invoices.Create(1, 1, input)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	input.Lines[0].Price = 12_000_000
	input.Notes = "Revised scope"
	updated, err := invoices.UpdateDraft(1, 1, invoice.ID, input)
	if err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if updated.Total <= invoice.Total || updated.Notes != "Revised scope" {
		t.Fatalf("draft was not fully updated: %#v", updated)
	}
	if _, err := invoices.Issue(1, 1, invoice.ID); err != nil {
		t.Fatalf("issue invoice: %v", err)
	}
	if _, err := invoices.UpdateDraft(1, 1, invoice.ID, input); err == nil {
		t.Fatal("issued invoice should be immutable")
	}
}

func TestInvoiceIssuePostsExactlyOnceAndLocksDocument(t *testing.T) {
	store, invoices, input := newInvoiceFixture(t)
	invoice, err := invoices.Create(1, 1, input)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	before := len(store.ListJournalEntries(1, ""))

	issued, err := invoices.Issue(1, 1, invoice.ID)
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}
	if issued.Status != domain.InvoiceStatusIssued {
		t.Fatalf("status = %q, want %q", issued.Status, domain.InvoiceStatusIssued)
	}
	if got := len(store.ListJournalEntries(1, "")); got != before+1 {
		t.Fatalf("journal count = %d, want %d", got, before+1)
	}
	if _, err := invoices.Issue(1, 1, invoice.ID); err == nil {
		t.Fatal("issuing an invoice twice should fail")
	}
	if err := invoices.Delete(1, 1, invoice.ID); err == nil {
		t.Fatal("issued invoices cannot be deleted")
	}
}

func TestInvoicePaymentsDerivePartialAndPaidStatus(t *testing.T) {
	store, invoices, input := newInvoiceFixture(t)
	invoice, err := invoices.Create(1, 1, input)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	invoice, err = invoices.Issue(1, 1, invoice.ID)
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}
	cash, err := store.GetAccountByCode(1, "1100")
	if err != nil {
		t.Fatalf("cash account: %v", err)
	}

	first, err := invoices.RecordPayment(1, 1, invoice.ID, InvoicePaymentInput{
		Date: "2026-09-22", Amount: 4_000_000, CashAccountID: cash.ID,
	})
	if err != nil {
		t.Fatalf("record partial payment: %v", err)
	}
	if first.Status != domain.InvoiceStatusPartiallyPaid {
		t.Fatalf("status = %q, want %q", first.Status, domain.InvoiceStatusPartiallyPaid)
	}
	if first.Outstanding != invoice.Total-4_000_000 {
		t.Fatalf("outstanding = %d, want %d", first.Outstanding, invoice.Total-4_000_000)
	}

	paid, err := invoices.RecordPayment(1, 1, invoice.ID, InvoicePaymentInput{
		Date: "2026-09-23", Amount: first.Outstanding, CashAccountID: cash.ID,
	})
	if err != nil {
		t.Fatalf("record final payment: %v", err)
	}
	if paid.Status != domain.InvoiceStatusPaid || paid.Outstanding != 0 {
		t.Fatalf("final invoice = status %q outstanding %d", paid.Status, paid.Outstanding)
	}
	payments, err := invoices.ListPayments(1, invoice.ID)
	if err != nil {
		t.Fatalf("list payments: %v", err)
	}
	if len(payments) != 2 {
		t.Fatalf("payment count = %d, want 2", len(payments))
	}
}
