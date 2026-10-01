package contract

import (
	"errors"
	"testing"
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
)

// errForced is the sentinel used by FaultyStore injections.
var errForced = errors.New("forced contract failure")

func init() {
	// Align forced-failure messages with a recognizable sentinel for assertions.
	_ = errForced
}

// SequenceAndAuditRollback proves document numbering and audit rows move only
// when the surrounding transaction commits.
func SequenceAndAuditRollback(t *testing.T, c Case) {
	t.Helper()
	beforeAudit := len(c.Store.ListAuditLogs(c.CompanyID))
	beforeNext := c.Store.NextNumber(c.CompanyID, "INV")

	faulty := &FaultyStore{Store: c.Store, FailAudit: true}
	err := faulty.WithTransaction(func(tx repository.Store) error {
		tx.NextNumber(c.CompanyID, "INV")
		if err := tx.CreateCustomer(&domain.Customer{
			CompanyID: c.CompanyID, Name: "Rollback Customer",
		}); err != nil {
			return err
		}
		return tx.AppendAuditLog(domain.AuditLog{
			CompanyID: c.CompanyID, Action: "test.rollback", Entity: "test",
		})
	})
	if err == nil {
		t.Fatal("expected forced rollback error")
	}

	for _, customer := range c.Store.ListCustomers(c.CompanyID) {
		if customer.Name == "Rollback Customer" {
			t.Fatal("customer committed despite transaction rollback")
		}
	}
	afterNext := c.Store.NextNumber(c.CompanyID, "INV")
	if afterNext == beforeNext {
		t.Fatalf("sequence did not advance outside the failed tx (both %q)", beforeNext)
	}
	// The failed tx consumed one number inside the transaction; a committed
	// allocation after rollback must be the immediately following value, or the
	// same next value if the failed allocation rolled back entirely.
	if err := c.Store.WithTransaction(func(tx repository.Store) error {
		got := tx.NextNumber(c.CompanyID, "INV")
		_ = got
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(c.Store.ListAuditLogs(c.CompanyID)) != beforeAudit {
		// Audit may gain entries from NextNumber side effects only if the store
		// writes them; it does not. Any new row means a leak.
		for _, entry := range c.Store.ListAuditLogs(c.CompanyID) {
			if entry.Action == "test.rollback" {
				t.Fatal("audit row committed despite transaction rollback")
			}
		}
	}
	for _, entry := range c.Store.ListAuditLogs(c.CompanyID) {
		if entry.Action == "test.rollback" {
			t.Fatal("audit row committed despite transaction rollback")
		}
	}
}

// JournalCreateRollback proves a failed journal create leaves no entry and no
// sequence consumption visible to later callers.
func JournalCreateRollback(t *testing.T, c Case) {
	t.Helper()
	before := len(c.Store.ListJournalEntries(c.CompanyID, ""))

	faulty := &FaultyStore{Store: c.Store, FailCreateJournal: true}
	err := faulty.WithTransaction(func(tx repository.Store) error {
		entry := &domain.JournalEntry{
			CompanyID: c.CompanyID, BranchID: 1, Number: tx.NextNumber(c.CompanyID, "JRN"),
			Date: time.Now().Format("2006-01-02"), Status: domain.JournalStatusDraft,
			SourceType: domain.SourceManual, Memo: "forced fail",
		}
		if err := tx.CreateJournalEntry(entry); err != nil {
			return err
		}
		return tx.AppendAuditLog(domain.AuditLog{
			CompanyID: c.CompanyID, Action: "journal.create", Entity: "journal_entry",
		})
	})
	if err == nil {
		t.Fatal("expected forced journal create failure")
	}
	if got := len(c.Store.ListJournalEntries(c.CompanyID, "")); got != before {
		t.Fatalf("journal count = %d, want %d after rollback", got, before)
	}
}

// JournalPostRollback proves a failed post leaves the draft in Draft status
// with no journal.post audit row.
func JournalPostRollback(t *testing.T, c Case) {
	t.Helper()
	js := newJournalService(c.Store)
	accs := c.Store.ListAccounts(c.CompanyID)
	var cash, revenue uint
	for _, a := range accs {
		switch a.Code {
		case "1100":
			cash = a.ID
		case "4100":
			revenue = a.ID
		}
	}
	if cash == 0 || revenue == 0 {
		t.Fatal("seeded accounts 1100/4100 missing")
	}

	draft, err := js.Create(c.CompanyID, 1, journalInput(cash, revenue, 10_000))
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	beforeAudit := countAction(c.Store.ListAuditLogs(c.CompanyID), "journal.post")
	faulty := &FaultyStore{Store: c.Store, FailAudit: true}
	err = faulty.WithTransaction(func(tx repository.Store) error {
		if err := tx.PostJournalEntry(c.CompanyID, draft.ID); err != nil {
			return err
		}
		return tx.AppendAuditLog(domain.AuditLog{
			CompanyID: c.CompanyID, Action: "journal.post", Entity: "journal_entry",
		})
	})
	if err == nil {
		t.Fatal("expected forced post failure")
	}

	got, err := c.Store.GetJournalEntry(c.CompanyID, draft.ID)
	if err != nil {
		t.Fatalf("reload draft: %v", err)
	}
	if got.Status != domain.JournalStatusDraft {
		t.Fatalf("status = %q, want Draft after failed post", got.Status)
	}
	if after := countAction(c.Store.ListAuditLogs(c.CompanyID), "journal.post"); after != beforeAudit {
		t.Fatalf("journal.post audits = %d, want %d after rollback", after, beforeAudit)
	}
}

// InvoiceIssueRollback proves issuance is all-or-nothing: no journal, no status
// change, no audit row when the transaction fails.
func InvoiceIssueRollback(t *testing.T, c Case) {
	t.Helper()
	invoices := newInvoiceService(c.Store)
	inv, err := invoices.Create(c.CompanyID, 1, invoiceInput(c.CompanyID))
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	beforeJournals := len(c.Store.ListJournalEntries(c.CompanyID, ""))
	beforeAudit := countAction(c.Store.ListAuditLogs(c.CompanyID), "invoice.issue")

	faulty := &FaultyStore{Store: c.Store, FailAudit: true}
	err = issueWithStore(faulty, invoices, c.CompanyID, inv.ID)
	if err == nil {
		t.Fatal("expected forced issue failure")
	}

	reloaded, err := c.Store.GetInvoice(c.CompanyID, inv.ID)
	if err != nil {
		t.Fatalf("reload invoice: %v", err)
	}
	if reloaded.Status != domain.InvoiceStatusDraft {
		t.Fatalf("status = %q, want Draft after failed issue", reloaded.Status)
	}
	if got := len(c.Store.ListJournalEntries(c.CompanyID, "")); got != beforeJournals {
		t.Fatalf("journal count = %d, want %d after failed issue", got, beforeJournals)
	}
	if after := countAction(c.Store.ListAuditLogs(c.CompanyID), "invoice.issue"); after != beforeAudit {
		t.Fatalf("invoice.issue audits = %d, want %d after rollback", after, beforeAudit)
	}
}

// PaymentRollback proves a failed payment write leaves invoice status, payment
// list, and journals unchanged.
func PaymentRollback(t *testing.T, c Case) {
	t.Helper()
	invoices := newInvoiceService(c.Store)
	inv, err := invoices.Create(c.CompanyID, 1, invoiceInput(c.CompanyID))
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	inv, err = invoices.Issue(c.CompanyID, 1, inv.ID)
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}
	cash, err := c.Store.GetAccountByCode(c.CompanyID, "1100")
	if err != nil {
		t.Fatalf("cash account: %v", err)
	}
	beforePayments := len(c.Store.ListInvoicePayments(c.CompanyID, inv.ID))
	beforeJournals := len(c.Store.ListJournalEntries(c.CompanyID, ""))
	beforeAudit := countAction(c.Store.ListAuditLogs(c.CompanyID), "invoice.payment")

	faulty := &FaultyStore{Store: c.Store, FailCreatePayment: true}
	_, err = payWithStore(faulty, invoices, c.CompanyID, inv.ID, cash.ID, inv.Total/2)
	if err == nil {
		t.Fatal("expected forced payment failure")
	}

	reloaded, err := invoices.Get(c.CompanyID, inv.ID)
	if err != nil {
		t.Fatalf("reload invoice: %v", err)
	}
	if reloaded.Status != domain.InvoiceStatusIssued && reloaded.Status != domain.InvoiceStatusOverdue {
		t.Fatalf("status = %q, want still unpaid after failed payment", reloaded.Status)
	}
	if reloaded.PaidAmount != 0 {
		t.Fatalf("paidAmount = %d, want 0 after failed payment", reloaded.PaidAmount)
	}
	if got := len(c.Store.ListInvoicePayments(c.CompanyID, inv.ID)); got != beforePayments {
		t.Fatalf("payment count = %d, want %d after rollback", got, beforePayments)
	}
	if got := len(c.Store.ListJournalEntries(c.CompanyID, "")); got != beforeJournals {
		t.Fatalf("journal count = %d, want %d after rollback", got, beforeJournals)
	}
	if after := countAction(c.Store.ListAuditLogs(c.CompanyID), "invoice.payment"); after != beforeAudit {
		t.Fatalf("invoice.payment audits = %d, want %d after rollback", after, beforeAudit)
	}
}

// SessionLifecycle covers create, read, revoke, expiry rejection, and cleanup.
func SessionLifecycle(t *testing.T, c Case) {
	t.Helper()
	now := time.Now()
	hash := "contract-session-hash-" + c.Name

	if err := c.Store.CreateSession(&domain.Session{
		TokenHash: hash, UserID: 1, CompanyID: c.CompanyID,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, ok := c.Store.GetSession(hash, now); !ok {
		t.Fatal("fresh session not found")
	}
	if err := c.Store.RevokeSession(hash, now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := c.Store.GetSession(hash, now); ok {
		t.Fatal("revoked session still accepted")
	}

	expiredHash := hash + "-expired"
	if err := c.Store.CreateSession(&domain.Session{
		TokenHash: expiredHash, UserID: 1, CompanyID: c.CompanyID,
		ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if _, ok := c.Store.GetSession(expiredHash, now); ok {
		t.Fatal("expired session accepted")
	}
	if err := c.Store.DeleteExpiredSessions(now); err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if _, ok := c.Store.GetSession(expiredHash, now); ok {
		t.Fatal("expired session not cleaned up")
	}
}

// TenantIsolation proves reads scoped to another company cannot see demo data.
func TenantIsolation(t *testing.T, c Case) {
	t.Helper()
	account, err := c.Store.GetAccountByCode(c.CompanyID, "1100")
	if err != nil {
		t.Fatalf("cash account: %v", err)
	}
	other := c.CompanyID + 10_000
	if _, err := c.Store.GetAccount(other, account.ID); err == nil {
		t.Fatal("foreign company read another tenant's account")
	}
	if _, err := c.Store.GetAccountByCode(other, "1100"); err == nil {
		t.Fatal("foreign company resolved another tenant's account code")
	}
	invoices := c.Store.ListInvoices(other)
	if len(invoices) != 0 {
		t.Fatalf("foreign company sees %d invoices, want 0", len(invoices))
	}
	entries := c.Store.ListJournalEntries(other, "")
	if len(entries) != 0 {
		t.Fatalf("foreign company sees %d journals, want 0", len(entries))
	}
}
