package service

import (
	"testing"

	"tera/internal/domain"
	"tera/internal/repository"
)

// The demo seed is date-relative and evolves, so these tests derive their
// expectations from the store instead of hardcoding the seeded figures. They
// assert the accounting invariants that must hold regardless of the dataset.

func demoStore(t *testing.T) repository.Store {
	t.Helper()
	return repository.NewMemoryStore()
}

func TestBalanceSheet_Balances(t *testing.T) {
	store := demoStore(t)
	rs := NewReportService(store)

	bs, err := rs.BalanceSheet(1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A balance sheet must satisfy the accounting equation:
	// Assets = Liabilities + Equity (+ current period profit).
	right := bs.TotalLiabilities + bs.TotalEquity + bs.CurrentPeriodProfit
	if bs.TotalAssets != right {
		t.Errorf("assets = %d, want liabilities+equity+profit = %d", bs.TotalAssets, right)
	}
	if bs.TotalAssets == 0 {
		t.Error("expected a non-zero asset total from the demo seed")
	}
}

func TestProfitLoss_NetIncomeArithmetic(t *testing.T) {
	store := demoStore(t)
	rs := NewReportService(store)

	pl, err := rs.ProfitLoss(1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Gross profit and net income must be internally consistent.
	if want := pl.Revenue.Total - pl.COGS.Total; pl.GrossProfit != want {
		t.Errorf("gross profit = %d, want revenue-cogs = %d", pl.GrossProfit, want)
	}
	if want := pl.GrossProfit - pl.OperatingExpense.Total; pl.NetIncome != want {
		t.Errorf("net income = %d, want gross-profit-opex = %d", pl.NetIncome, want)
	}
}

func TestDashboard_KPIs(t *testing.T) {
	store := demoStore(t)
	rs := NewReportService(store)

	dash, err := rs.Dashboard(1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dash.KPIs) != 3 {
		t.Fatalf("kpis = %d, want 3", len(dash.KPIs))
	}

	// Cash KPI mirrors the seeded cash account balance.
	cash, err := store.GetAccountByCode(1, "1100")
	if err != nil {
		t.Fatal(err)
	}
	if dash.KPIs[0].Value != cash.Balance {
		t.Errorf("cash KPI = %d, want seeded cash balance %d", dash.KPIs[0].Value, cash.Balance)
	}

	// Overdue receivables must be strictly less than total open receivables.
	totalOverdue := int64(0)
	for _, invoice := range store.ListInvoices(1) {
		if invoice.Status == domain.InvoiceStatusOverdue {
			totalOverdue += invoice.Total
		}
	}
	if dash.KPIs[1].Value != totalOverdue {
		t.Errorf("overdue KPI = %d, want %d", dash.KPIs[1].Value, totalOverdue)
	}
	if totalOverdue == 0 {
		t.Error("expected the demo seed to contain overdue invoices")
	}

	// Profit KPI agrees with the profit and loss report.
	pl, err := rs.ProfitLoss(1)
	if err != nil {
		t.Fatalf("profit loss: %v", err)
	}
	if dash.KPIs[2].Value != pl.NetIncome {
		t.Errorf("profit KPI = %d, want net income %d", dash.KPIs[2].Value, pl.NetIncome)
	}

	if len(dash.RecentTransactions) == 0 {
		t.Error("expected recent transactions")
	}
}

func TestLedger_RunningBalance(t *testing.T) {
	store := demoStore(t)
	rs := NewReportService(store)

	account, err := store.GetAccountByCode(1, "1100")
	if err != nil {
		t.Fatal(err)
	}

	detail, err := rs.Ledger(1, account.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detail.ClosingBalance != detail.Opening+detail.TotalDebit-detail.TotalCredit {
		t.Errorf("closing = %d, want opening+debit-credit = %d",
			detail.ClosingBalance, detail.Opening+detail.TotalDebit-detail.TotalCredit)
	}
}

func TestLedger_ExcludesDraftEntries(t *testing.T) {
	store := demoStore(t)
	rs := NewReportService(store)
	account, err := store.GetAccountByCode(1, "1100")
	if err != nil {
		t.Fatal(err)
	}

	// Identify a draft entry so the test targets real data.
	drafts := map[string]bool{}
	for _, entry := range store.ListJournalEntries(1, domain.JournalStatusDraft) {
		drafts[entry.Number] = true
	}
	if len(drafts) == 0 {
		t.Fatal("expected the demo seed to contain draft journals")
	}

	detail, err := rs.Ledger(1, account.ID)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	for _, row := range detail.Rows {
		if drafts[row.Ref] {
			t.Fatalf("draft journal %s leaked into the posted general ledger", row.Ref)
		}
	}
}

func TestLedger_UsesCreditNormalBalance(t *testing.T) {
	store := demoStore(t)
	js := NewJournalService(store)
	rs := NewReportService(store)

	liability, err := store.GetAccountByCode(1, "2100")
	if err != nil {
		t.Fatal(err)
	}
	cash, err := store.GetAccountByCode(1, "1100")
	if err != nil {
		t.Fatal(err)
	}

	before, err := rs.Ledger(1, liability.ID)
	if err != nil {
		t.Fatalf("ledger before: %v", err)
	}

	_, err = js.Create(1, 1, JournalInput{
		Memo:   "utang baru",
		Status: domain.JournalStatusPosted,
		Lines: []JournalLineInput{
			{AccountID: cash.ID, Debit: 1_000_000},
			{AccountID: liability.ID, Credit: 1_000_000},
		},
	})
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}

	after, err := rs.Ledger(1, liability.ID)
	if err != nil {
		t.Fatalf("ledger after: %v", err)
	}

	// A credit-normal liability increases by the credited amount, and the
	// running balance walks from the recomputed opening to the closing figure.
	if after.ClosingBalance != before.ClosingBalance+1_000_000 {
		t.Fatalf("liability closing = %d, want %d (increased by the credit)",
			after.ClosingBalance, before.ClosingBalance+1_000_000)
	}
	if after.Opening != after.ClosingBalance-after.TotalCredit+after.TotalDebit {
		t.Fatalf("liability opening = %d, want closing-credit+debit = %d",
			after.Opening, after.ClosingBalance-after.TotalCredit+after.TotalDebit)
	}
}

func TestInvoiceCreate_NonPKPDoesNotChargeVAT(t *testing.T) {
	store := demoStore(t)
	company := &domain.Company{Name: "CV Non PKP", SAKMode: domain.SAKEMKM, IsPKP: false}
	if err := store.CreateCompany(company); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBranch(&domain.Branch{CompanyID: company.ID, Name: "Kantor Pusat"}); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountService(store)
	if err := accounts.EnsureDefaults(company.ID); err != nil {
		t.Fatalf("ensure defaults: %v", err)
	}
	js := NewJournalService(store)
	is := NewInvoiceService(store, NewTaxService(), js)

	branch := store.ListBranches(company.ID)[0]
	invoice, err := is.Create(company.ID, branch.ID, InvoiceInput{
		CustomerName: "Pelanggan Non PKP",
		Lines:        []InvoiceLineInput{{Name: "Jasa", Qty: 1, Price: 1_000_000}},
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if invoice.TaxAmount != 0 || invoice.Total != 1_000_000 {
		t.Fatalf("tax/total = %d/%d, want 0/1000000", invoice.TaxAmount, invoice.Total)
	}
}

func TestInvoiceIssue_AutoJournal(t *testing.T) {
	store := demoStore(t)
	tax := NewTaxService()
	js := NewJournalService(store)
	is := NewInvoiceService(store, tax, js)

	branch := store.ListBranches(1)[0]
	inv, err := is.Create(1, branch.ID, InvoiceInput{
		CustomerName: "PT Uji Coba",
		Lines: []InvoiceLineInput{
			{Name: "Jasa Konsultasi", Qty: 2, Price: 5_000_000},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.Subtotal != 10_000_000 || inv.TaxAmount != 1_100_000 || inv.Total != 11_100_000 {
		t.Errorf("subtotal/tax/total = %d/%d/%d, want 10000000/1100000/11100000",
			inv.Subtotal, inv.TaxAmount, inv.Total)
	}
	inv, err = is.Issue(1, 1, inv.ID)
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}

	// Issuance creates the journal, which must balance.
	entries := store.ListJournalEntries(1, "")
	found := false
	for _, e := range entries {
		if e.SourceType == domain.SourceInvoice && e.SourceID == inv.ID {
			found = true
			var d, c int64
			for _, l := range e.Lines {
				d += l.Debit
				c += l.Credit
			}
			if d != c {
				t.Errorf("auto journal not balanced: debit=%d credit=%d", d, c)
			}
			if d != inv.Total {
				t.Errorf("auto journal total = %d, want %d", d, inv.Total)
			}
		}
	}
	if !found {
		t.Error("expected auto-generated journal for the invoice")
	}
}
