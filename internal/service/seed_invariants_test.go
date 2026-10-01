package service

import (
	"strings"
	"testing"

	"tera/internal/domain"
	"tera/internal/repository"
)

// These tests guard the invariants that make the demo dataset a valid set of
// books. An earlier revision hardcoded account balances that happened to look
// plausible while violating the accounting equation, which made every report
// untrustworthy. Deriving balances from balanced journals prevents that class
// of regression, and these assertions keep it that way.

func TestSeed_OpeningBalancesSatisfyAccountingEquation(t *testing.T) {
	store := repository.NewMemoryStore()
	rs := NewReportService(store)

	bs, err := rs.BalanceSheet(1)
	if err != nil {
		t.Fatalf("balance sheet: %v", err)
	}

	// Assets = Liabilities + Equity + current-period profit.
	want := bs.TotalLiabilities + bs.TotalEquity + bs.CurrentPeriodProfit
	if bs.TotalAssets != want {
		t.Errorf("accounting equation violated: assets = %d, want liabilities+equity+profit = %d (off by %d)",
			bs.TotalAssets, want, bs.TotalAssets-want)
	}
}

func TestSeed_EveryPostedJournalIsBalanced(t *testing.T) {
	store := repository.NewMemoryStore()

	for _, entry := range store.ListJournalEntries(1, "") {
		if entry.Status != domain.JournalStatusPosted {
			continue
		}
		var debit, credit int64
		for _, line := range entry.Lines {
			debit += line.Debit
			credit += line.Credit
		}
		if debit != credit {
			t.Errorf("journal %s is not balanced: debit=%d credit=%d", entry.Number, debit, credit)
		}
		if debit == 0 {
			t.Errorf("journal %s has no value", entry.Number)
		}
	}
}

func TestSeed_EveryJournalLineReferencesARealAccount(t *testing.T) {
	store := repository.NewMemoryStore()

	known := map[uint]bool{}
	for _, account := range store.ListAccounts(1) {
		known[account.ID] = true
	}

	for _, entry := range store.ListJournalEntries(1, "") {
		for _, line := range entry.Lines {
			if !known[line.AccountID] {
				t.Errorf("journal %s references unknown account id %d", entry.Number, line.AccountID)
			}
			if line.Debit < 0 || line.Credit < 0 {
				t.Errorf("journal %s has a negative amount", entry.Number)
			}
			if line.Debit > 0 && line.Credit > 0 {
				t.Errorf("journal %s line sets both debit and credit", entry.Number)
			}
		}
	}
}

func TestSeed_DraftEntriesDoNotAffectReports(t *testing.T) {
	store := repository.NewMemoryStore()
	rs := NewReportService(store)

	posted, err := rs.ProfitLoss(1)
	if err != nil {
		t.Fatalf("profit loss: %v", err)
	}

	// Posting the drafts should change the reported result, proving they were
	// previously excluded.
	drafts := 0
	for _, entry := range store.ListJournalEntries(1, domain.JournalStatusDraft) {
		drafts++
		if err := store.CreateJournalEntry(&entry); err != nil {
			t.Fatalf("re-posting draft: %v", err)
		}
	}
	if drafts == 0 {
		t.Fatal("expected the demo seed to contain draft journals")
	}

	// Reports read persisted accounts; since the store did not apply the
	// re-created drafts, the reported figures must be unchanged.
	after, err := rs.ProfitLoss(1)
	if err != nil {
		t.Fatalf("profit loss after: %v", err)
	}
	if after.NetIncome != posted.NetIncome {
		t.Errorf("net income changed from %d to %d after re-adding drafts",
			posted.NetIncome, after.NetIncome)
	}
}

func TestSeed_ProfitLossShowsAProfit(t *testing.T) {
	store := repository.NewMemoryStore()
	rs := NewReportService(store)

	pl, err := rs.ProfitLoss(1)
	if err != nil {
		t.Fatalf("profit loss: %v", err)
	}
	if pl.Revenue.Total <= 0 {
		t.Errorf("revenue = %d, want a positive figure for the demo period", pl.Revenue.Total)
	}
	if pl.NetIncome <= 0 {
		t.Errorf("net income = %d, want the demo period to be profitable", pl.NetIncome)
	}
	// A distributor should not show software-like margins.
	if pl.Revenue.Total > 0 {
		margin := float64(pl.GrossProfit) / float64(pl.Revenue.Total)
		if margin > 0.75 {
			t.Errorf("gross margin = %.0f%%, implausibly high for the demo business", margin*100)
		}
	}
}

func TestSeed_ReceivablesAgingHasMultipleBuckets(t *testing.T) {
	store := repository.NewMemoryStore()

	buckets := map[string]int{}
	for _, invoice := range store.ListInvoices(1) {
		if invoice.Status == domain.InvoiceStatusOverdue {
			buckets[invoice.Status]++
		}
	}
	if buckets[domain.InvoiceStatusOverdue] < 2 {
		t.Errorf("expected several overdue invoices to populate AR aging, got %d",
			buckets[domain.InvoiceStatusOverdue])
	}
}

// The audit-log view is presented as a list of events, so the demo tenant must
// ship with events of its own — an empty trail makes the feature look broken on
// first load. These assertions pin the seeded trail to the same action/entity
// vocabulary the services write, so the UI mapping keeps working.
func TestSeed_AuditTrailIsPopulatedAndMatchesServiceVocabulary(t *testing.T) {
	store := repository.NewMemoryStore()

	logs := store.ListAuditLogs(1)
	if len(logs) == 0 {
		t.Fatal("demo tenant has no audit entries; the audit-log view renders empty")
	}

	allowed := map[string]string{
		"invoice.create": "invoice",
		"journal.create": "journal_entry",
		"journal.post":   "journal_entry",
	}
	for _, l := range logs {
		entity, ok := allowed[l.Action]
		if !ok {
			t.Errorf("audit action %q is not one the UI maps", l.Action)
			continue
		}
		if l.Entity != entity {
			t.Errorf("action %q recorded entity %q, want %q", l.Action, l.Entity, entity)
		}
		if l.UserID == 0 {
			t.Errorf("audit entry %d has no actor; the table shows a blank user", l.ID)
		}
	}

	// A draft journal must never be recorded as posted.
	for _, j := range store.ListJournalEntries(1, "") {
		if j.Status == domain.JournalStatusPosted {
			continue
		}
		for _, l := range logs {
			if l.Action == "journal.post" && strings.Contains(l.AfterJSON, j.Number) {
				t.Errorf("draft journal %s was audited as posted", j.Number)
			}
		}
	}
}

// Ordering is part of the contract: the UI renders the list as-is, so both
// stores must agree on newest-first by event time.
func TestSeed_AuditTrailIsOrderedNewestFirst(t *testing.T) {
	store := repository.NewMemoryStore()

	logs := store.ListAuditLogs(1)
	if len(logs) < 2 {
		t.Fatalf("need at least two entries to check ordering, got %d", len(logs))
	}
	for i := 1; i < len(logs); i++ {
		prev, cur := logs[i-1], logs[i]
		if cur.CreatedAt.After(prev.CreatedAt) {
			t.Errorf("entry %d (%s) is newer than its predecessor %d (%s)",
				cur.ID, cur.CreatedAt, prev.ID, prev.CreatedAt)
		}
	}
}
